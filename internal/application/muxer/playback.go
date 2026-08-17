package muxer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/streammux/streammux/internal/application/ffmpeg"
	"github.com/streammux/streammux/internal/domain/model"
)

// ErrBeyondEnd reports a segment request past the end of the film.
var ErrBeyondEnd = errors.New("segment beyond end of film")

// playbackState is the per-job VOD timeline. The public HLS timeline is
// static: segment n always maps to file seg_%05d.ts of whichever generation
// produced it, and the media playlists expose the full film duration from the
// first request so the player can seek anywhere immediately.
type playbackState struct {
	mu sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc

	cacheDir string

	// active is the ffmpeg session currently encoding. Segments produced by
	// earlier generations remain servable via all (newest first).
	active *generation
	all    []*generation

	starting  bool
	startWait chan struct{}
	startErr  error
	directURL string
	lastStart time.Time

	recovering   bool
	recoveryWait chan struct{}
	recoveryErr  error

	nextGeneration uint64
	nextPlan       int

	duration        float64 // film duration in seconds (from probe)
	discontinuities []int   // public segments preceded by EXT-X-DISCONTINUITY
	lastRequested   int
	maxRequested    int
	lastAccess      time.Time
	lastRecovery    time.Time
	closed          bool

	deliveries []deliverySample
}

type generation struct {
	id           uint64
	planIndex    int
	plan         model.PlaybackPlan
	prepared     *preparedPlan
	dir          string
	session      *ffmpeg.Session
	startSegment int
	startedAt    time.Time
}

func (m *Muxer) stateFor(job *model.MuxJob) (*playbackState, error) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if state, ok := m.states[job.ID]; ok {
		return state, nil
	}

	dir, err := os.MkdirTemp("", "streammux-"+job.ID+"-*")
	if err != nil {
		return nil, fmt.Errorf("create playback cache: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	state := &playbackState{
		ctx:           ctx,
		cancel:        cancel,
		cacheDir:      dir,
		lastRequested: -1,
		maxRequested:  -1,
		lastAccess:    time.Now(),
	}
	m.states[job.ID] = state
	job.CacheDir = dir
	return state, nil
}

// EnsurePlaylist blocks until a playable generation exists, retrying failed
// startups after a cooldown (resuming from the next untried plan).
func (m *Muxer) EnsurePlaylist(ctx context.Context, job *model.MuxJob) error {
	state, err := m.stateFor(job)
	if err != nil {
		return err
	}

	state.mu.Lock()
	state.lastAccess = time.Now()
	if state.active != nil {
		state.mu.Unlock()
		return nil
	}
	if state.directURL != "" {
		direct := state.directURL
		cause := state.startErr
		state.mu.Unlock()
		return &DirectFallbackError{URL: direct, Err: cause}
	}
	// Allow one retry per cooldown window; retries resume from nextPlan so
	// the user's second click tries fresh sources instead of repeating.
	if state.startErr != nil && time.Since(state.lastStart) > m.policy.RetryCooldown {
		state.starting = false
		state.startErr = nil
	}
	if !state.starting {
		state.starting = true
		state.startWait = make(chan struct{})
		state.startErr = nil
		state.lastStart = time.Now()
		go m.runStartup(job, state)
	}
	wait := state.startWait
	state.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wait:
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.active != nil {
		return nil
	}
	if state.directURL != "" {
		return &DirectFallbackError{URL: state.directURL, Err: state.startErr}
	}
	if state.startErr != nil {
		return state.startErr
	}
	return fmt.Errorf("playback startup finished without a playable source")
}

func (m *Muxer) runStartup(job *model.MuxJob, state *playbackState) {
	startPlan := state.nextPlan
	if startPlan < 0 {
		startPlan = 0
	}
	winner, err := m.coordinateAttempts(job, state, startPlan, 0, m.policy.StartupTimeout)
	if err != nil {
		direct := m.resolveDirectFallback(job, state)
		state.mu.Lock()
		state.starting = false
		state.startErr = err
		state.directURL = direct
		wait := state.startWait
		state.mu.Unlock()
		if wait != nil {
			close(wait)
		}
		log.Printf("mux: startup failed (next retry from plan %d): %v", state.nextPlan, err)
		return
	}

	state.mu.Lock()
	if state.closed {
		state.starting = false
		state.startErr = context.Canceled
		wait := state.startWait
		state.startWait = nil
		state.mu.Unlock()
		winner.session.Cancel()
		if wait != nil {
			close(wait)
		}
		return
	}
	state.active = winner
	state.all = append(state.all, winner)
	state.nextPlan = winner.planIndex + 1
	state.starting = false
	state.startErr = nil
	state.directURL = ""
	state.duration = winner.prepared.duration
	state.lastRecovery = time.Now()
	state.lastRequested = -1
	state.maxRequested = -1
	wait := state.startWait
	state.mu.Unlock()

	job.Duration = winner.prepared.duration
	job.PlaylistReady = true
	if wait != nil {
		close(wait)
	}
	log.Printf("mux: startup selected plan %d/%d %s (%s)", winner.planIndex+1, len(job.Plans), winner.plan.Kind, winner.plan.Video.Parsed.Resolution)
	go m.monitorGeneration(job, state, winner)
}

// coordinateAttempts tries playback plans sequentially, one at a time, until
// one produces its first segment. Sequential is deliberate: each attempt
// opens up to two debrid connections and debrid services cap concurrent
// slots (~2-3). Two phases with separate budgets: strict first, then lenient
// (lenient also accepts und/untagged tracks from a dubbed multiaudio source).
func (m *Muxer) coordinateAttempts(job *model.MuxJob, state *playbackState, startPlan, startSegment int, timeout time.Duration) (*generation, error) {
	if startPlan >= len(job.Plans) {
		return nil, fmt.Errorf("no playback plans remain")
	}

	lenientTimeout := m.policy.StartupTimeout / 2
	if lenientTimeout < 5*time.Second {
		lenientTimeout = 5 * time.Second
	}

	strictCtx, strictCancel := context.WithTimeout(state.ctx, timeout)
	generation, strictErr := m.tryPlans(strictCtx, job, state, startPlan, startSegment, false)
	strictCancel()
	if generation != nil && strictErr == nil {
		return generation, nil
	}

	lenientCtx, lenientCancel := context.WithTimeout(state.ctx, lenientTimeout)
	generation, lenientErr := m.tryPlans(lenientCtx, job, state, startPlan, startSegment, true)
	lenientCancel()
	if generation != nil && lenientErr == nil {
		return generation, nil
	}

	return nil, errors.Join(strictErr, lenientErr)
}

func (m *Muxer) tryPlans(ctx context.Context, job *model.MuxJob, state *playbackState, startPlan, startSegment int, lenient bool) (*generation, error) {
	var failures []error
	for planIndex := startPlan; planIndex < len(job.Plans); planIndex++ {
		select {
		case <-ctx.Done():
			failures = append(failures, ctx.Err())
			return nil, errors.Join(failures...)
		default:
		}

		// Plans without the target audio are not HLS candidates; the
		// subtitled fallback is served as a direct redirect instead.
		if !job.Plans[planIndex].HasTargetAudio {
			continue
		}

		generation, err := m.startAttempt(ctx, job, state, planIndex, startSegment, lenient)
		if err == nil && generation != nil {
			return generation, nil
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("plan %d: %w", planIndex, err))
			log.Printf("mux: plan %d failed: %v", planIndex, err)
			// Advance nextPlan so retries and recoveries skip this plan.
			state.mu.Lock()
			if planIndex+1 > state.nextPlan {
				state.nextPlan = planIndex + 1
			}
			state.mu.Unlock()
		}
	}
	return nil, errors.Join(failures...)
}

func (m *Muxer) startAttempt(parent context.Context, job *model.MuxJob, state *playbackState, planIndex, startSegment int, lenient bool) (*generation, error) {
	plan := job.Plans[planIndex]
	prepared, err := m.preparePlanMode(parent, job, plan, lenient)
	if err != nil {
		return nil, err
	}
	if prepared.duration <= 0 {
		return nil, fmt.Errorf("plan has no probeable duration")
	}

	// The A/V offset is estimated by cross-correlating the first seconds of
	// the video source's primary audio against the dubbed track. The estimate
	// is cached per source pair.
	var audioOffset time.Duration
	dualSource := strings.TrimSpace(prepared.audioURL) != "" && prepared.audioURL != prepared.videoURL
	if dualSource {
		if offset, ok := m.audioOffsetFor(plan); ok {
			audioOffset = offset
			if offset != 0 {
				log.Printf("mux: using cached audio offset %s", offset)
			}
		} else {
			lag, track, confidence, err := m.detectOffset(prepared)
			if err != nil {
				log.Printf("mux: audio offset estimation failed (continuing without): %v", err)
			} else if confidence < ffmpeg.SyncMinConfidence {
				log.Printf("mux: audio offset inconclusive (conf %.1f, continuing without): video track a:%d", confidence, track)
			} else {
				audioOffset = -lag
				m.cacheAudioOffset(plan, audioOffset)
				log.Printf("mux: estimated audio offset %s from video track a:%d (conf %.1f)", audioOffset, track, confidence)
			}
		}
	}

	attemptCtx, cancel := context.WithTimeout(parent, m.policy.AttemptTimeout)
	defer cancel()

	state.mu.Lock()
	state.nextGeneration++
	generationID := state.nextGeneration
	state.mu.Unlock()

	dir := filepath.Join(state.cacheDir, fmt.Sprintf("generation-%06d", generationID))
	if err := os.MkdirAll(filepath.Join(dir, "video"), 0755); err != nil {
		return nil, fmt.Errorf("create generation video directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "audio"), 0755); err != nil {
		return nil, fmt.Errorf("create generation audio directory: %w", err)
	}

	// The session is bound to the playback context; attemptCtx only gates
	// how long we wait for the first segment below.
	session, err := m.ffmpeg.StartSession(state.ctx, ffmpeg.SessionSpec{
		VideoURL:        prepared.videoURL,
		AudioURL:        prepared.audioURL,
		VideoTrackIndex: prepared.videoTrackIndex,
		AudioTrackIndex: prepared.audioTrackIndex,
		StartSegment:    startSegment,
		OutputDir:       dir,
		AudioMode:       prepared.audioMode,
		AudioLanguage:   job.TargetLanguage,
		AudioTitle:      job.TargetLanguage,
		UserAgent:       browserUA,
		AudioOffset:     audioOffset,
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}

	generation := &generation{
		id:           generationID,
		planIndex:    planIndex,
		plan:         plan,
		prepared:     prepared,
		dir:          dir,
		session:      session,
		startSegment: startSegment,
		startedAt:    time.Now(),
	}

	segmentPath := generationSegmentPath(generation, startSegment)
	audioSegmentPath := generationAudioSegmentPath(generation, startSegment)
	ticker := time.NewTicker(75 * time.Millisecond)
	defer ticker.Stop()

	for {
		if fileExists(segmentPath) && fileExists(audioSegmentPath) {
			return generation, nil
		}
		select {
		case <-attemptCtx.Done():
			session.Cancel()
			go cleanupFailedGeneration(generation)
			return nil, fmt.Errorf("first segment deadline: %w", attemptCtx.Err())
		case <-session.Done():
			if fileExists(segmentPath) && fileExists(audioSegmentPath) {
				return generation, nil
			}
			go cleanupFailedGeneration(generation)
			if session.Err() != nil {
				m.invalidatePlanSources(plan)
				return nil, session.Err()
			}
			return nil, fmt.Errorf("ffmpeg ended before producing segment %d", startSegment)
		case <-ticker.C:
		}
	}
}

func cleanupFailedGeneration(generation *generation) {
	select {
	case <-generation.session.Done():
	case <-time.After(2 * time.Second):
	}
	_ = os.RemoveAll(generation.dir)
}

func (m *Muxer) resolveDirectFallback(job *model.MuxJob, state *playbackState) string {
	for _, plan := range job.Plans {
		if plan.Kind != model.PlanSubtitledFallback {
			continue
		}
		if plan.Video.Stream.URL != "" {
			return plan.Video.Stream.URL
		}
		ctx, cancel := context.WithTimeout(state.ctx, 2*time.Second)
		url, err := m.resolveSource(ctx, job, plan.Video)
		cancel()
		if err == nil {
			return url
		}
	}
	return ""
}

// MasterPlaylist renders the master we serve: a single variant whose audio is
// declared as a rendition group. The audio group declaration is mandatory —
// without it players ignore the audio playlist entirely.
func (m *Muxer) MasterPlaylist(job *model.MuxJob) ([]byte, bool) {
	state := m.lookupState(job.ID)
	if state == nil {
		return nil, false
	}
	state.mu.Lock()
	state.lastAccess = time.Now()
	active := state.active
	duration := state.duration
	state.mu.Unlock()
	if active == nil || active.prepared == nil || duration <= 0 {
		return nil, false
	}

	bitrate := active.prepared.videoBitrate
	if bitrate <= 0 {
		bitrate = float64(active.plan.EstimatedBandwidth())
	}
	// Headroom for the audio rendition and TS overhead.
	bandwidth := int64(bitrate * 1.2)
	if bandwidth <= 0 {
		bandwidth = 8_000_000
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:6\n")
	code := ffmpeg.LanguageCode(job.TargetLanguage)
	name := job.TargetLanguage
	if name == "" {
		name = "Audio"
	}
	media := fmt.Sprintf("#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=%q,DEFAULT=YES,AUTOSELECT=YES", name)
	if code != "" {
		media += ",LANGUAGE=\"" + code + "\""
	}
	media += ",URI=\"audio/audio.m3u8\"\n"
	b.WriteString(media)

	streamInf := "#EXT-X-STREAM-INF:BANDWIDTH=" + fmt.Sprint(bandwidth)
	if active.prepared.videoWidth > 0 && active.prepared.videoHeight > 0 {
		streamInf += fmt.Sprintf(",RESOLUTION=%dx%d", active.prepared.videoWidth, active.prepared.videoHeight)
	}
	streamInf += ",AUDIO=\"aud\"\n"
	b.WriteString(streamInf)
	b.WriteString("video/video.m3u8\n")
	return []byte(b.String()), true
}

// VideoPlaylist renders the full-length VOD media playlist for the video
// rendition: every segment of the film is listed from the first request, so
// the player knows the real duration and can seek freely.
func (m *Muxer) VideoPlaylist(job *model.MuxJob) ([]byte, bool) {
	return m.vodPlaylist(job)
}

// AudioPlaylist renders the same timeline for the audio rendition.
func (m *Muxer) AudioPlaylist(job *model.MuxJob) ([]byte, bool) {
	return m.vodPlaylist(job)
}

func (m *Muxer) vodPlaylist(job *model.MuxJob) ([]byte, bool) {
	state := m.lookupState(job.ID)
	if state == nil {
		return nil, false
	}
	state.mu.Lock()
	state.lastAccess = time.Now()
	duration := state.duration
	disc := append([]int(nil), state.discontinuities...)
	state.mu.Unlock()
	if duration <= 0 {
		return nil, false
	}
	return buildVodPlaylist(duration, disc)
}

// SegmentPath resolves a public segment index to a file produced by any
// generation, newest first.
func (m *Muxer) SegmentPath(job *model.MuxJob, segment int) string {
	return m.segmentPath(job, segment, false)
}

// AudioSegmentPath resolves a public audio segment index to a file.
func (m *Muxer) AudioSegmentPath(job *model.MuxJob, segment int) string {
	return m.segmentPath(job, segment, true)
}

func (m *Muxer) segmentPath(job *model.MuxJob, segment int, audio bool) string {
	state := m.lookupState(job.ID)
	if state == nil {
		return ""
	}
	state.mu.Lock()
	state.lastAccess = time.Now()
	duration := state.duration
	all := append([]*generation(nil), state.all...)
	state.mu.Unlock()

	if segment < 0 || duration <= 0 || segment >= vodSegmentCount(duration) {
		return ""
	}
	for i := len(all) - 1; i >= 0; i-- {
		var path string
		if audio {
			path = generationAudioSegmentPath(all[i], segment)
		} else {
			path = generationSegmentPath(all[i], segment)
		}
		if fileExists(path) {
			return path
		}
	}
	return ""
}

// isForwardSeek reports whether a segment request is a real user seek rather
// than pre-buffering: a large jump beyond the previous maximum that the
// encoder has not reached.
const seekJumpThreshold = 20

func isForwardSeek(maxRequested, physical, highest, startSegment int) bool {
	if maxRequested < 0 || physical <= maxRequested+seekJumpThreshold {
		return false
	}
	return highest >= startSegment && physical > highest+8
}

func vodSegmentCount(filmDuration float64) int {
	if filmDuration <= 0 {
		return 0
	}
	segDur := ffmpeg.SegDuration()
	if segDur <= 0 {
		segDur = 4.0
	}
	return int(math.Ceil(filmDuration / segDur))
}

// EnsureSegment serves (or waits for / restarts at) the requested video
// segment. Backward requests hit the on-disk cache; forward requests beyond
// what the encoder produced restart the session at that offset.
func (m *Muxer) EnsureSegment(ctx context.Context, job *model.MuxJob, segment int) (string, error) {
	return m.ensureMediaSegment(ctx, job, segment, false)
}

// EnsureAudioSegment is EnsureSegment for the audio rendition.
func (m *Muxer) EnsureAudioSegment(ctx context.Context, job *model.MuxJob, segment int) (string, error) {
	return m.ensureMediaSegment(ctx, job, segment, true)
}

func (m *Muxer) ensureMediaSegment(ctx context.Context, job *model.MuxJob, segment int, audio bool) (string, error) {
	if err := m.EnsurePlaylist(ctx, job); err != nil {
		return "", err
	}
	if path := m.segmentPath(job, segment, audio); path != "" {
		return path, nil
	}

	state := m.lookupState(job.ID)
	if state == nil {
		return "", fmt.Errorf("playback state not found")
	}

	state.mu.Lock()
	count := vodSegmentCount(state.duration)
	state.mu.Unlock()
	if segment >= count {
		return "", ErrBeyondEnd
	}

	deadlineCtx, cancel := context.WithTimeout(ctx, m.policy.SegmentTimeout)
	defer cancel()

	for {
		if path := m.segmentPath(job, segment, audio); path != "" {
			return path, nil
		}

		state.mu.Lock()
		state.lastAccess = time.Now()
		active := state.active
		recovering := state.recovering
		recoveryWait := state.recoveryWait
		recoveryErr := state.recoveryErr
		nextPlan := state.nextPlan
		state.lastRequested = segment
		if segment > state.maxRequested {
			state.maxRequested = segment
		}
		maxReq := state.maxRequested
		state.mu.Unlock()

		if active == nil {
			switch {
			case recovering:
				// recovery in flight; wait below
			case recoveryErr != nil:
				// Every plan already failed during recovery: fail fast
				// instead of burning the segment deadline.
				return "", recoveryErr
			default:
				m.ensureRecovery(job, state, segment, nextPlan, "no active session")
			}
		} else {
			highest := highestCompleteSegment(active.dir)
			select {
			case <-active.session.Done():
				if !recovering {
					m.ensureRecovery(job, state, segment, nextPlan, "session ended")
				}
			default:
				// A real seek: either behind this session's start (already
				// evicted from cache) or far ahead of production. Restart at
				// the requested offset, same plan first.
				if (segment < active.startSegment || isForwardSeek(maxReq, segment, highest, active.startSegment)) && !recovering {
					m.ensureRecovery(job, state, segment, active.planIndex, "seek")
				}
			}
		}

		select {
		case <-deadlineCtx.Done():
			return "", fmt.Errorf("timeout waiting for segment %d: %w", segment, deadlineCtx.Err())
		case <-time.After(100 * time.Millisecond):
		case <-recoveryWait:
			state.mu.Lock()
			err := state.recoveryErr
			state.mu.Unlock()
			if err != nil && m.segmentPath(job, segment, audio) == "" {
				return "", err
			}
		}
	}
}

func (m *Muxer) ensureRecovery(job *model.MuxJob, state *playbackState, startSegment, startPlan int, reason string) {
	state.mu.Lock()
	if state.closed || state.recovering {
		state.mu.Unlock()
		return
	}
	if startPlan >= len(job.Plans) {
		state.recoveryErr = fmt.Errorf("fallback exhausted after %s", reason)
		state.mu.Unlock()
		return
	}
	state.recovering = true
	state.recoveryWait = make(chan struct{})
	state.recoveryErr = nil
	state.mu.Unlock()
	go m.runRecovery(job, state, startSegment, startPlan, reason)
}

func (m *Muxer) runRecovery(job *model.MuxJob, state *playbackState, startSegment, startPlan int, reason string) {
	winner, err := m.coordinateAttempts(job, state, startPlan, startSegment, m.policy.StartupTimeout)

	state.mu.Lock()
	wait := state.recoveryWait
	old := state.active
	if err == nil && !state.closed {
		state.active = winner
		state.all = append(state.all, winner)
		if winner.planIndex+1 > state.nextPlan {
			state.nextPlan = winner.planIndex + 1
		}
		// Mark the cutover when the source changes so players reset their
		// decoders (resolution/HDR can differ between plans).
		if old != nil && old != winner && old.planIndex != winner.planIndex {
			state.discontinuities = append(state.discontinuities, startSegment)
		}
		state.lastRecovery = time.Now()
		state.recoveryErr = nil
	} else {
		state.recoveryErr = err
	}
	state.recovering = false
	state.recoveryWait = nil
	state.mu.Unlock()

	if wait != nil {
		close(wait)
	}
	if err != nil {
		log.Printf("mux: recovery after %s failed: %v", reason, err)
		return
	}
	if old != nil && old != winner {
		old.session.Cancel()
	}
	log.Printf("mux: switched at segment %d to plan %d (%s) after %s", startSegment, winner.planIndex, winner.plan.Kind, reason)
	go m.monitorGeneration(job, state, winner)
}

func (m *Muxer) lookupState(jobID string) *playbackState {
	m.stateMu.Lock()
	state := m.states[jobID]
	m.stateMu.Unlock()
	return state
}

func generationSegmentPath(generation *generation, segment int) string {
	if generation == nil {
		return ""
	}
	return filepath.Join(generation.dir, "video", fmt.Sprintf("seg_%05d.ts", segment))
}

func generationAudioSegmentPath(generation *generation, segment int) string {
	if generation == nil {
		return ""
	}
	return filepath.Join(generation.dir, "audio", fmt.Sprintf("seg_%05d.ts", segment))
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

func highestCompleteSegment(dir string) int {
	entries, err := os.ReadDir(filepath.Join(dir, "video"))
	if err != nil {
		return -1
	}
	highest := -1
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		var index int
		if _, err := fmt.Sscanf(entry.Name(), "seg_%05d.ts", &index); err == nil && index > highest {
			highest = index
		}
	}
	return highest
}

// buildVodPlaylist renders the complete VOD media playlist: every segment of
// the film with its duration, ENDLIST included, and DISCONTINUITY markers at
// plan cutover points.
func buildVodPlaylist(filmDuration float64, discontinuities []int) ([]byte, bool) {
	if filmDuration <= 0 {
		return nil, false
	}
	segDur := ffmpeg.SegDuration()
	if segDur <= 0 {
		segDur = 4.0
	}
	segs := computeEqualLengthSegments(segDur, filmDuration)
	if len(segs) == 0 {
		return nil, false
	}
	target := int(math.Ceil(segDur))
	if target < 1 {
		target = 1
	}
	disc := make(map[int]bool, len(discontinuities))
	for _, d := range discontinuities {
		if d > 0 && d < len(segs) {
			disc[d] = true
		}
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:6\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", target))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	for i, d := range segs {
		if disc[i] {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		b.WriteString(fmt.Sprintf("#EXTINF:%.6f,\n", d))
		b.WriteString(fmt.Sprintf("seg_%05d.ts\n", i))
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return []byte(b.String()), true
}

func computeEqualLengthSegments(segDur, total float64) []float64 {
	if segDur <= 0 || total <= 0 {
		return nil
	}
	n := int(total / segDur)
	rem := total - float64(n)*segDur
	if rem < 1e-9 {
		out := make([]float64, n)
		for i := range out {
			out[i] = segDur
		}
		return out
	}
	out := make([]float64, n+1)
	for i := 0; i < n; i++ {
		out[i] = segDur
	}
	out[n] = rem
	return out
}

func (m *Muxer) reapIdleSessions() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for now := range ticker.C {
		m.stateMu.Lock()
		states := make([]*playbackState, 0, len(m.states))
		for _, state := range m.states {
			states = append(states, state)
		}
		m.stateMu.Unlock()

		for _, state := range states {
			state.mu.Lock()
			if state.active != nil && now.Sub(state.lastAccess) > m.policy.IdleTimeout {
				active := state.active
				state.active = nil
				state.mu.Unlock()
				active.session.Cancel()
				continue
			}
			state.mu.Unlock()
		}
	}
}

// CleanupJob stops every generation before removing its private cache.
func (m *Muxer) CleanupJob(job *model.MuxJob) {
	m.stateMu.Lock()
	state := m.states[job.ID]
	delete(m.states, job.ID)
	m.stateMu.Unlock()
	if state == nil {
		if job.CacheDir != "" {
			_ = os.RemoveAll(job.CacheDir)
		}
		return
	}

	state.mu.Lock()
	state.closed = true
	state.cancel()
	generations := append([]*generation(nil), state.all...)
	state.mu.Unlock()
	for _, generation := range generations {
		generation.session.Cancel()
	}
	_ = os.RemoveAll(state.cacheDir)
}
