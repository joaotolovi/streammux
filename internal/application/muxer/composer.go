package muxer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"

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
	// Positions in each queue (a source can appear in both via the shared
	// ledger, so they are tracked separately).
	videoPos int
	audioPos int

	url       string
	probe     *ffmpeg.ProbeResult
	failed    bool
	failErr   error
	trackDone bool // audio track selection attempted (strict)
	track     int  // strict target-language audio track (-1 = none)
	trackLeni int  // lenient track (-1 = none)
	leniDone  bool // lenient selection attempted (distinguishes "rejected in
	// lenient" from "not yet evaluated" — the latter must still be tried last)
}

type failClass int

const (
	failNone failClass = iota
	failVideo
	failAudio
	failNoTrack
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
	ranked  []*composition
	cursor  int
	lenient bool
	done    bool
	nextOrd int
	// incompatible marks dual pairs that failed compatibility (duration /
	// edition mismatch): the sources remain valid for other pairings.
	incompatible map[string]bool
	// lastDelivered guards against delivering the same composition twice
	// without an intervening fail().
	lastDelivered string
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
				if audioConfidence(plan.Audio, job.TargetLanguage) >= 2 {
					seenAudio[ak] = true
					audioState := getState(plan.Audio)
					c.audios = append(c.audios, audioState)
					// A dubbed source may also carry excellent video (e.g. a
					// 55GB BluRay REMUX with PT-BR audio). Include it as a
					// video candidate so single-source compositions are
					// reachable — it is tried as a video at its natural score.
					if !seenVideo[ak] {
						seenVideo[ak] = true
						c.videos = append(c.videos, audioState)
					}
				}
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
		ai, aj := audioConfidence(a, job.TargetLanguage), audioConfidence(b, job.TargetLanguage)
		if ai != aj {
			return ai > aj
		}
		if analyzer.AudioScore(a) != analyzer.AudioScore(b) {
			return analyzer.AudioScore(a) > analyzer.AudioScore(b)
		}
		return audioRoleRank(a.AddonRole) < audioRoleRank(b.AddonRole)
	})
	// Set positions separately for each queue — a source can appear in both
	// (shared ledger) so a single idx field would be overwritten.
	for i := range c.videos {
		c.videos[i].videoPos = i
	}
	for i := range c.audios {
		c.audios[i].audioPos = i
	}

	// Rank every possible composition by combined quality using the geometric
	// mean: √(VideoScore × EffectiveAudioScore). The geometric mean ensures
	// "best video AND best audio" — a great video with garbage audio scores
	// low, unlike an additive sum where one side dominates.
	// EffectiveAudioScore weights the raw AudioScore by the language
	// confidence: an uncertain-language audio is nearly worthless because it
	// may not be in the user's language at all.
	c.incompatible = map[string]bool{}
	type rankedEntry struct {
		comp  *composition
		score float64
	}
	var entries []rankedEntry
	for _, v := range c.videos {
		for _, a := range c.audios {
			vs := float64(analyzer.VideoScore(v.stream))
			as := float64(analyzer.AudioScore(a.stream))
			// Language confidence multiplier: only sources with real evidence
			// of the target language keep their audio score; weak-evidence
			// sources (only the addon's self-reported language) are nearly
			// zeroed — they may be any language.
			conf := audioConfidence(a.stream, job.TargetLanguage)
			switch conf {
			case 3:
				as *= 1.0
			case 2:
				as *= 0.7
			default:
				as *= 0.05
			}
			single := v == a
			var combined float64
			if vs <= 0 || as <= 0 {
				combined = 0
			} else {
				combined = math.Sqrt(vs * as)
			}
			comp := &composition{video: v, audio: a, single: single}
			entries = append(entries, rankedEntry{comp, combined})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].score > entries[j].score })
	for i, e := range entries {
		e.comp.ordinal = i + 1
		c.ranked = append(c.ranked, e.comp)
	}

	log.Printf("mux: composer videos=%d audios=%d compositions=%d", len(c.videos), len(c.audios), len(c.ranked))
	for i, s := range c.videos {
		log.Printf("mux:   video#%d score=%d key=%s", i, analyzer.VideoScore(s.stream), s.stream.SourceKey())
	}
	for i, s := range c.audios {
		log.Printf("mux:   audio#%d conf=%d score=%d key=%s", i, audioConfidence(s.stream, job.TargetLanguage), analyzer.AudioScore(s.stream), s.stream.SourceKey())
	}
	for i, comp := range c.ranked {
		if i >= 8 {
			log.Printf("mux:   ... %d more", len(c.ranked)-8)
			break
		}
		vs := analyzer.VideoScore(comp.video.stream)
		as := analyzer.AudioScore(comp.audio.stream)
		conf := audioConfidence(comp.audio.stream, job.TargetLanguage)
		log.Printf("mux:   rank#%d video#%d(=%d) audio#%d(=%d,conf=%d) single=%v", comp.ordinal, comp.video.videoPos, vs, comp.audio.audioPos, as, conf, comp.single)
	}
	return c
}

// audioConfidence scores how trustworthy a source's target-language claim is
// before probing. Sources with explicit evidence (PTBR in the filename, an
// explicit dubbed language, or "Portuguese (Brazil)" in parsed languages)
// rank highest; sources that only match via the addon's self-reported
// language or a generic "Dual Audio" tag rank lowest — they may still be the
// right language but are tried last.
func audioConfidence(s model.CollectedStream, target string) int {
	targetShort := strings.TrimSpace(strings.SplitN(target, " ", 2)[0])
	for _, lang := range s.Parsed.Languages {
		if lang == target || strings.HasPrefix(lang, targetShort) || strings.HasPrefix(target, lang) {
			return 3
		}
	}
	if s.IsDubbed && s.Language == target {
		return 2
	}
	if s.AddonLanguage == target {
		return 1
	}
	if s.IsDubbed && s.Language == "Dual Audio" {
		return 1
	}
	for _, lang := range s.Parsed.Languages {
		if lang == "Dual Audio" {
			return 1
		}
	}
	return 0
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
// acquire returns the next composition from the ranked list, skipping
// exhausted sources and incompatible pairs. Must be called with state.mu held.
func (c *composer) acquire() *composition {
	return c.acquireWithin(0)
}

// acquireWithin is acquire with a video bitrate ceiling: compositions whose
// video source is estimated above maxBits bits/s are skipped. Used by
// recoveries on a lightweight ABR tier so a failed light session is not
// "recovered" back onto the heavy primary source. maxBits <= 0 means
// unlimited. Must be called with state.mu held.
func (c *composer) acquireWithin(maxBits int64) *composition {
	for {
		if c.done {
			return nil
		}
		if c.cursor >= len(c.ranked) {
			if !c.lenient && c.hasLenientCandidates() {
				// Second pass: accept und/untagged tracks from dubbed
				// multiaudio sources.
				c.lenient = true
				c.cursor = 0
				continue
			}
			c.done = true
			return nil
		}
		comp := c.ranked[c.cursor]
		if comp.video.failed || comp.audio.failed {
			c.cursor++
			continue
		}
		if !comp.single && c.incompatible[pairKey(comp)] {
			c.cursor++
			continue
		}
		if maxBits > 0 && streamBandwidth(comp.video.stream) > maxBits {
			c.cursor++
			continue
		}
		// Skip audio sources confirmed to lack the target language in the
		// current pass. A source not yet evaluated in the lenient pass is NOT
		// skipped here: it is acquired so its lenient track is computed and,
		// failing that, it is skipped on every subsequent encounter.
		if c.lenient {
			if comp.audio.trackDone && comp.audio.leniDone && comp.audio.trackLeni < 0 {
				c.cursor++
				continue
			}
		} else if comp.audio.trackDone && comp.audio.track < 0 {
			c.cursor++
			continue
		}
		key := c.compositionKey(comp)
		if key == c.lastDelivered {
			c.cursor++
			continue
		}
		c.lastDelivered = key
		log.Printf("mux: acquire rank=%d/%d -> video#%d audio#%d single=%v lenient=%v",
			c.cursor+1, len(c.ranked), comp.video.videoPos, comp.audio.audioPos, comp.single, c.lenient)
		return comp
	}
}

// hasLenientCandidates reports whether any ranked composition could succeed
// in the lenient pass. A source is a lenient candidate until it has actually
// been evaluated in the lenient pass (leniDone): only then can we know whether
// it offers an untagged/und track. Sources rejected in the strict pass are not
// discarded — they are deferred to the end of the queue.
func (c *composer) hasLenientCandidates() bool {
	for _, comp := range c.ranked {
		if comp.video.failed || comp.audio.failed {
			continue
		}
		if comp.audio.trackDone && !comp.audio.leniDone {
			return true
		}
		if !comp.audio.trackDone {
			return true
		}
	}
	return false
}

// fail records a failed composition. Mismatch marks the PAIR incompatible
// (the sources stay valid for other pairings); resolve/probe/launch failures
// mark the responsible source as failed for every pairing.
func (c *composer) fail(comp *composition, class failClass, err error) {
	switch class {
	case failVideo:
		comp.video.failed = true
		comp.video.failErr = err
		c.cursor++
	case failAudio:
		// Resolve/probe failure: the source is dead for every pairing.
		comp.audio.failed = true
		comp.audio.failErr = err
		c.cursor++
	case failNoTrack:
		// The audio source has no target-language track in this pass. The
		// track selection is already memoized (trackDone), so this and other
		// pairings skip it as audio; it remains a video candidate. The
		// lenient pass may still find an und/untagged track.
		c.cursor++
	case failCompose:
		// Both sources work but not together (duration/edition mismatch):
		// the pair is dead, the sources remain available to other pairings.
		if !comp.single {
			c.incompatible[pairKey(comp)] = true
		}
		c.cursor++
	case failLaunch:
		// The ffmpeg session died. A deadline/timeout is often transient
		// (remote input seek on a huge file): do not kill the source for
		// other pairings. Any other launch error blames the video source.
		if !errors.Is(err, context.DeadlineExceeded) {
			comp.video.failed = true
			comp.video.failErr = err
		}
		c.cursor++
	default:
		c.cursor++
	}
}

func pairKey(comp *composition) string {
	return comp.video.stream.SourceKey() + "\x00" + comp.audio.stream.SourceKey()
}

func (c *composer) compositionKey(comp *composition) string {
	if comp.single {
		return "single:" + comp.video.stream.SourceKey()
	}
	return pairKey(comp)
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

// reset relaunches the composer from scratch (used when a retry should
// re-evaluate every source). Clears failed flags and cursor positions.
// Must be called with state.mu held.
func (c *composer) reset() {
	c.cursor = 0
	c.lenient = false
	c.done = false
	c.incompatible = make(map[string]bool)
	c.lastDelivered = ""
	for _, q := range [][]*sourceState{c.videos, c.audios} {
		for _, s := range q {
			s.failed = false
			s.failErr = nil
			s.trackDone = false
			s.track = 0
			s.trackLeni = 0
			s.leniDone = false
		}
	}
}

// sourceByKey returns the sourceState for a given SourceKey, or nil when no
// source in either queue matches. Used by the ABR ladder to pair a lighter
// video with the current dub audio. Must be called with state.mu held.
func (c *composer) sourceByKey(sourceKey string) *sourceState {
	if sourceKey == "" {
		return nil
	}
	for _, q := range [][]*sourceState{c.videos, c.audios} {
		for _, s := range q {
			if s.stream.SourceKey() == sourceKey {
				return s
			}
		}
	}
	return nil
}

// withAudio builds a temporary pairing that preserves an already-playing
// audio source while the recovery coordinator tries a different video source.
// It is intentionally not part of ranked/cursor state: failing this pairing
// must not mark a healthy audio source as unusable for the normal fallback.
func (c *composer) withAudio(video *sourceState, audioKey string) *composition {
	audio := c.sourceByKey(audioKey)
	if video == nil || audio == nil || video.failed || audio.failed {
		return nil
	}
	return &composition{
		video:   video,
		audio:   audio,
		single:  video == audio,
		lenient: c.lenient,
		ordinal: -1,
	}
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
		// "No target track" is a property of the source+pass, not a dead
		// source: failNoTrack lets the composer skip it as audio while other
		// pairings (and the lenient pass) remain possible.
		return nil, failNoTrack, fmt.Errorf("source has no confirmed %s audio track", job.TargetLanguage)
	}
	lang, title := trackMeta(audio.probe.AudioTracks, track)
	log.Printf("mux: composition %d selected audio track a:%d (of %d, lang=%s title=%s) from %s lenient=%v",
		comp.ordinal, track, len(audio.probe.AudioTracks), lang, title, audio.stream.SourceKey(), comp.isLenient())

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
		videoIdx:        comp.video.videoPos,
		audioIdx:        comp.audio.audioPos,
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
			log.Printf("mux: video source resolve failed (video#%d): %v", s.videoPos, err)
			return err
		}
		s.url = url
	}
	if s.probe == nil {
		probe, err := m.probeSource(ctx, s.url)
		if err != nil {
			s.failed = true
			s.failErr = err
			log.Printf("mux: video source probe failed (video#%d): %v", s.videoPos, err)
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
		s.leniDone = true
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

// trackMeta returns the language tag and title of a selected audio track for
// logging (empty strings when the track is not found).
func trackMeta(tracks []ffmpeg.AudioTrack, index int) (lang, title string) {
	for _, t := range tracks {
		if t.Index == index {
			return t.Language, t.Title
		}
	}
	return "", ""
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
