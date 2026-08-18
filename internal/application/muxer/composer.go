package muxer

import (
	"context"
	"fmt"
	"log"
	"sort"

	"github.com/streammux/streammux/internal/application/analyzer"
	"github.com/streammux/streammux/internal/application/ffmpeg"
	"github.com/streammux/streammux/internal/domain/constants"
	"github.com/streammux/streammux/internal/domain/model"
)

// The composer assembles playback compositions dynamically from two ordered
// candidate queues: best video first, best target-language audio first. Each
// source is resolved/probed at most once per job (ledger) and the global
// resolve/probe caches avoid network repetition across jobs. When one side of
// a composition fails, only that side advances — a working video is reused
// with the next audio and vice versa — until the queues are exhausted.

type sourceState struct {
	stream model.CollectedStream
	idx    int // position in its queue, used for logging

	url       string
	probe     *ffmpeg.ProbeResult
	failed    bool
	failErr   error
	trackDone bool // audio track selection attempted (strict)
	track     int  // strict target-language audio track (-1 = none)
	trackLeni int  // lenient track (-1 = none)
}

type failClass int

const (
	failNone failClass = iota
	failVideo
	failAudio
	failCompose
	failLaunch
)

type composition struct {
	video  *sourceState
	audio  *sourceState
	single bool
	// lenient marks compositions from the second pass (und/untagged tracks
	// accepted).
	lenient bool
	// ordinal identifies this composition in logs / generations.
	ordinal int
}

type composer struct {
	videos  []*sourceState
	audios  []*sourceState
	vi, ai  int
	lenient bool
	done    bool
	nextOrd int
	// lastKey guards against delivering the same composition twice without
	// an intervening fail() — callers that re-acquire without failing are
	// treated as if the composition had failed generically.
	lastKey string
}

// newComposer derives the ordered candidate queues from the planner output.
// One ledger entry per source: a dubbed source referenced by both queues is
// the SAME sourceState, so acquire() recognizes single-source compositions
// (same entry) and resolve/probe/track work is never duplicated.
func newComposer(job *model.MuxJob) *composer {
	c := &composer{}

	ledger := map[string]*sourceState{}
	getState := func(stream model.CollectedStream) *sourceState {
		key := stream.SourceKey()
		if s, ok := ledger[key]; ok {
			return s
		}
		s := &sourceState{stream: stream, track: -1, trackLeni: -1}
		ledger[key] = s
		return s
	}

	seenVideo := map[string]bool{}
	seenAudio := map[string]bool{}
	for _, plan := range job.Plans {
		if plan.Kind == model.PlanSubtitledFallback {
			continue
		}
		vk := plan.Video.SourceKey()
		if vk != "" && !seenVideo[vk] {
			seenVideo[vk] = true
			c.videos = append(c.videos, getState(plan.Video))
		}
		if plan.HasTargetAudio {
			ak := plan.Audio.SourceKey()
			if ak != "" && !seenAudio[ak] {
				seenAudio[ak] = true
				c.audios = append(c.audios, getState(plan.Audio))
			}
		}
	}

	sort.SliceStable(c.videos, func(i, j int) bool {
		a, b := c.videos[i].stream, c.videos[j].stream
		if analyzer.VideoScore(a) != analyzer.VideoScore(b) {
			return analyzer.VideoScore(a) > analyzer.VideoScore(b)
		}
		return videoRoleRank(a.AddonRole) < videoRoleRank(b.AddonRole)
	})
	sort.SliceStable(c.audios, func(i, j int) bool {
		a, b := c.audios[i].stream, c.audios[j].stream
		if analyzer.AudioScore(a) != analyzer.AudioScore(b) {
			return analyzer.AudioScore(a) > analyzer.AudioScore(b)
		}
		return audioRoleRank(a.AddonRole) < audioRoleRank(b.AddonRole)
	})
	for i, s := range c.videos {
		s.idx = i
	}
	for i, s := range c.audios {
		s.idx = i
	}
	return c
}

func videoRoleRank(role string) int {
	switch role {
	case constants.RoleVideo, constants.RoleBoth:
		return 0
	case "":
		return 1
	default:
		return 2
	}
}

func audioRoleRank(role string) int {
	switch role {
	case constants.RoleAudio, constants.RoleBoth:
		return 0
	case "":
		return 1
	default:
		return 2
	}
}

// acquire returns the next untried composition, advancing past sources known
// to have failed or (for audio) to lack the target language. Must be called
// with state.mu held.
func (c *composer) acquire() *composition {
	for {
		if c.done {
			return nil
		}
		if c.vi >= len(c.videos) {
			if !c.lenient && len(c.audios) > 0 {
				// Second pass: accept und/untagged tracks from dubbed
				// multiaudio sources.
				c.lenient = true
				c.vi, c.ai = 0, 0
				continue
			}
			c.done = true
			return nil
		}
		v := c.videos[c.vi]
		if v.failed {
			c.vi++
			c.ai = 0
			continue
		}
		if c.ai >= len(c.audios) {
			c.vi++
			c.ai = 0
			continue
		}
		a := c.audios[c.ai]
		if a.failed {
			c.ai++
			continue
		}
		// Skip audio sources confirmed to lack the target language in the
		// current pass (the lenient pass may still find one).
		if c.lenient {
			if a.trackDone && a.trackLeni < 0 {
				c.ai++
				continue
			}
		} else if a.trackDone && a.track < 0 {
			c.ai++
			continue
		}
		if a == v {
			key := "single:" + v.stream.SourceKey()
			if key == c.lastKey {
				c.ai++
				continue
			}
			c.lastKey = key
			c.nextOrd++
			return &composition{video: v, audio: a, single: true, lenient: c.lenient, ordinal: c.nextOrd}
		}
		key := v.stream.SourceKey() + "\x00" + a.stream.SourceKey()
		if key == c.lastKey {
			c.ai++
			continue
		}
		c.lastKey = key
		c.nextOrd++
		return &composition{video: v, audio: a, single: false, lenient: c.lenient, ordinal: c.nextOrd}
	}
}

// fail records a failed composition and advances only the responsible side.
// Must be called with state.mu held.
func (c *composer) fail(comp *composition, class failClass, err error) {
	switch class {
	case failVideo:
		comp.video.failed = true
		comp.video.failErr = err
		c.vi++
		c.ai = 0
	case failAudio:
		if comp.single {
			// The source doubles as audio: mark the track as missing so it is
			// skipped as an audio candidate but kept as a video candidate.
			comp.audio.trackDone = true
			if comp.audio.track < 0 && !c.lenient {
				comp.audio.track = -1
			}
			c.ai++
		} else {
			comp.audio.failed = true
			comp.audio.failErr = err
			c.ai++
		}
	case failCompose:
		// Both sources work but not together (duration/edition mismatch):
		// try the next audio with the same video first.
		c.ai++
	case failLaunch:
		// The ffmpeg session died: blame the heavier side (video) and also
		// drop this audio pairing.
		comp.video.failed = true
		comp.video.failErr = err
		c.vi++
		c.ai = 0
	default:
		c.ai++
	}
}

// exhausted reports whether every composition has been tried (including the
// lenient pass). Must be called with state.mu held.
func (c *composer) exhausted() bool {
	return c.done
}

// markFailed flags a source as unusable in both queues. Must be called with
// state.mu held.
func (c *composer) markFailed(sourceKey string) {
	for _, q := range [][]*sourceState{c.videos, c.audios} {
		for _, s := range q {
			if s.stream.SourceKey() == sourceKey && !s.failed {
				s.failed = true
				s.failErr = fmt.Errorf("source failed during playback")
				break
			}
		}
	}
}

// reset relaunches the composer from scratch (used when a session dies and
// the current sources must be reconsidered). Must be called with state.mu held.
func (c *composer) reset() {
	c.vi, c.ai = 0, 0
	c.lenient = false
	c.done = false
}

func (m *Muxer) composerFor(job *model.MuxJob, state *playbackState) *composer {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.composer == nil {
		state.composer = newComposer(job)
	}
	return state.composer
}

// prepareComposition resolves, probes and validates one composition without
// launching ffmpeg. The ledger memoizes per-source results; only the first
// composition touching a source pays for resolve/probe.
func (m *Muxer) prepareComposition(ctx context.Context, job *model.MuxJob, comp *composition) (*preparedPlan, failClass, error) {
	if err := m.ensureVideoSource(ctx, job, comp.video); err != nil {
		return nil, failVideo, fmt.Errorf("video source: %w", err)
	}

	audio := comp.video // single-source default
	if !comp.single {
		if err := m.ensureAudioSource(ctx, job, comp.audio, comp.isLenient()); err != nil {
			return nil, failAudio, fmt.Errorf("audio source: %w", err)
		}
		audio = comp.audio
	} else {
		if err := m.ensureAudioSource(ctx, job, comp.video, comp.isLenient()); err != nil {
			return nil, failAudio, fmt.Errorf("audio source: %w", err)
		}
	}

	track := audio.selectedTrack(comp.isLenient())
	if track < 0 {
		return nil, failAudio, fmt.Errorf("source has no confirmed %s audio track", job.TargetLanguage)
	}
	log.Printf("mux: composition %d selected audio track a:%d (of %d tracks) from %s", comp.ordinal, track, len(audio.probe.AudioTracks), audio.stream.SourceKey())

	if comp.video.probe.Duration <= 0 {
		return nil, failVideo, fmt.Errorf("video source has no probeable duration")
	}

	if comp.single {
		audio = comp.video
	} else if err := compatibleReleases(makeCompositionPlan(comp), comp.video.probe, audio.probe, m.policy.DurationTolerance); err != nil {
		return nil, failCompose, err
	}

	prepared := &preparedPlan{
		plan:            makeCompositionPlan(comp),
		videoURL:        comp.video.url,
		audioURL:        audio.url,
		videoTrackIndex: comp.video.probe.VideoStreams[0].Index,
		audioTrackIndex: track,
		audioMode:       ffmpeg.AudioModeCopy,
		duration:        comp.video.probe.Duration,
		videoBitrate:    comp.video.probe.VideoBitrate,
		videoIdx:        comp.video.idx,
		audioIdx:        comp.audio.idx,
	}
	if len(comp.video.probe.VideoStreams) > 0 {
		prepared.videoWidth = comp.video.probe.VideoStreams[0].Width
		prepared.videoHeight = comp.video.probe.VideoStreams[0].Height
	}
	prepared.videoAudioTracks = comp.video.probe.AudioTracks

	for _, t := range audio.probe.AudioTracks {
		if t.Index == track {
			prepared.audioMode = compatibleAudioMode(t.Codec)
			break
		}
	}

	// A/V offset estimation for dual-source compositions (cached per pair).
	if !comp.single {
		if _, ok := m.audioOffsetFor(prepared.plan); !ok {
			lag, trackNo, confidence, err := m.detectOffset(prepared)
			if err != nil {
				log.Printf("mux: audio offset estimation failed (continuing without): %v", err)
			} else if confidence < ffmpeg.SyncMinConfidence {
				log.Printf("mux: audio offset inconclusive (conf %.1f, continuing without): video track a:%d", confidence, trackNo)
			} else {
				m.cacheAudioOffset(prepared.plan, -lag)
				log.Printf("mux: estimated audio offset %s from video track a:%d (conf %.1f)", -lag, trackNo, confidence)
			}
		}
	}
	return prepared, failNone, nil
}

func (c *composition) isLenient() bool { return c.lenient }

// ensureVideoSource resolves and probes a video candidate once.
func (m *Muxer) ensureVideoSource(ctx context.Context, job *model.MuxJob, s *sourceState) error {
	if s.failed {
		return s.failErr
	}
	if s.url == "" {
		url, err := m.resolveSource(ctx, job, s.stream)
		if err != nil {
			s.failed = true
			s.failErr = err
			return err
		}
		s.url = url
	}
	if s.probe == nil {
		probe, err := m.probeSource(ctx, s.url)
		if err != nil {
			s.failed = true
			s.failErr = err
			return err
		}
		s.probe = probe
	}
	if len(s.probe.VideoStreams) == 0 {
		err := fmt.Errorf("source has no video stream")
		s.failed = true
		s.failErr = err
		return err
	}
	return nil
}

// ensureAudioSource resolves and probes an audio candidate once and selects
// the target-language track for the current pass.
func (m *Muxer) ensureAudioSource(ctx context.Context, job *model.MuxJob, s *sourceState, lenient bool) error {
	if s.failed {
		return s.failErr
	}
	if s.url == "" {
		url, err := m.resolveSource(ctx, job, s.stream)
		if err != nil {
			s.failed = true
			s.failErr = err
			return err
		}
		s.url = url
	}
	if s.probe == nil {
		probe, err := m.probeSource(ctx, s.url)
		if err != nil {
			s.failed = true
			s.failErr = err
			return err
		}
		s.probe = probe
	}
	s.selectTrack(job, lenient)
	return nil
}

func (s *sourceState) selectTrack(job *model.MuxJob, lenient bool) {
	if lenient && !s.trackDone {
		// compute strict first so lenient can compare
		s.track = targetAudioTrackStrict(s.probe.AudioTracks, job.TargetLanguage, s.stream, false)
		s.trackDone = true
	}
	if lenient {
		s.trackLeni = targetAudioTrackStrict(s.probe.AudioTracks, job.TargetLanguage, s.stream, true)
		return
	}
	if !s.trackDone {
		s.track = targetAudioTrackStrict(s.probe.AudioTracks, job.TargetLanguage, s.stream, false)
		s.trackDone = true
	}
}

func (s *sourceState) selectedTrack(lenient bool) int {
	if lenient {
		if s.trackLeni >= 0 {
			return s.trackLeni
		}
		return s.track
	}
	if s.track >= 0 {
		return s.track
	}
	return s.trackLeni
}

// makeCompositionPlan builds the synthetic PlaybackPlan for a composition so
// downstream consumers (master playlist, logging) keep their shape.
func makeCompositionPlan(comp *composition) model.PlaybackPlan {
	kind := model.PlanSingleSource
	if !comp.single {
		kind = model.PlanDualSource
	}
	return model.PlaybackPlan{
		Kind:           kind,
		Video:          comp.video.stream,
		Audio:          comp.audio.stream,
		HasTargetAudio: true,
	}
}
