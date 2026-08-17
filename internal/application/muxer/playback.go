package muxer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/streammux/streammux/internal/application/ffmpeg"
	"github.com/streammux/streammux/internal/domain/model"
)

type playbackState struct {
	mu sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc

	cacheDir string
	active   *generation
	all      []*generation

	starting  bool
	startWait chan struct{}
	startErr  error
	directURL string

	// placeholder is the generation playing the local intro/loop video while
	// the film is prepared. placeholderStarted guards one-shot startup.
	placeholder        *generation
	placeholderStarted bool
	placeholderWait    chan struct{}
	// placeholderHighest is the last segment index known to have been produced
	// by the placeholder; used to stitch the film's HLS at the handoff so the
	// player never sees a discontinuity or reused segment numbers.
	placeholderHighest int
	// stitched is true once the master has been rewritten to include both the
	// placeholder prefix and the film suffix. Segment lookups must check it.
	stitched bool

	recovering   bool
	recoveryWait chan struct{}
	recoveryErr  error

	nextGeneration uint64
	nextPlan       int
	lastRequested  int
	lastAccess     time.Time
	lastRecovery   time.Time
	closed         bool
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
	// isPlaceholder marks a generation that plays a local intro/loop video
	// while the real film is being prepared in the background.
	isPlaceholder bool
	// handoffAt is set on a film generation that was stitched after a
	// placeholder: segments [0..handoffAt] live in placeholder.dir, the
	// remainder in dir with renumbered filenames.
	handoffAt int
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
		lastAccess:    time.Now(),
	}
	m.states[job.ID] = state
	job.CacheDir = dir
	return state, nil
}

// EnsurePlaylist returns as soon as a playable playlist exists. If a local
// placeholder video is configured and the real film is not ready yet, it
// starts the placeholder immediately (so the player gets instant playback) and
// prepares the film in the background. When the film becomes ready, the active
// generation switches to it and the same master URL starts serving the film.
func (m *Muxer) EnsurePlaylist(ctx context.Context, job *model.MuxJob) error {
	state, err := m.stateFor(job)
	if err != nil {
		return err
	}

	state.mu.Lock()
	state.lastAccess = time.Now()
	if state.active != nil && fileExists(generationPlaylistPath(state.active)) {
		state.mu.Unlock()
		return nil
	}
	if state.directURL != "" {
		direct := state.directURL
		cause := state.startErr
		state.mu.Unlock()
		return &DirectFallbackError{URL: direct, Err: cause}
	}

	// If a placeholder is available and the film is not ready, start the
	// placeholder now and kick off film preparation in the background.
	if m.placeholderIntroPath != "" || m.placeholderLoopPath != "" {
		if !state.placeholderStarted {
			state.placeholderStarted = true
			state.placeholderWait = make(chan struct{})
			go m.runPlaceholder(job, state)
		}
		if !state.starting {
			state.starting = true
			state.startWait = make(chan struct{})
			state.startErr = nil
			go m.runStartup(job, state)
		}
		wait := state.placeholderWait
		state.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait:
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.placeholder != nil && fileExists(generationPlaylistPath(state.placeholder)) {
			return nil
		}
		// Placeholder failed to start; fall through to waiting for the film.
		if state.active != nil && fileExists(generationPlaylistPath(state.active)) {
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

	// No placeholder: block until the film is ready (original behavior).
	start := false
	if !state.starting {
		state.starting = true
		state.startWait = make(chan struct{})
		state.startErr = nil
		start = true
	}
	wait := state.startWait
	state.mu.Unlock()

	if start {
		go m.runStartup(job, state)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wait:
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.active != nil && fileExists(generationPlaylistPath(state.active)) {
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
	stitchOffset := 0
	state.mu.Lock()
	if state.placeholder != nil {
		h := highestCompleteSegment(state.placeholder.dir)
		if h >= 0 {
			state.placeholderHighest = h
			stitchOffset = h + 1
		}
	}
	state.mu.Unlock()
	winner, err := m.coordinateAttempts(job, state, 0, stitchOffset, m.policy.StartupTimeout)
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
	state.lastRecovery = time.Now()
	placeholder := state.placeholder
	if placeholder != nil {
		state.mu.Unlock()
		placeholder.session.Cancel()
		select {
		case <-placeholder.session.Done():
		case <-time.After(2 * time.Second):
		}
		state.mu.Lock()
		state.stitched = true
		state.mu.Unlock()
		state.mu.Lock()
	}
	wait := state.startWait
	ho := state.placeholderHighest
	stitched := state.stitched && placeholder != nil
	state.mu.Unlock()

	job.Duration = winner.prepared.duration
	job.PlaylistReady = true
	if wait != nil {
		close(wait)
	}
	if stitched {
		log.Printf("mux: startup selected plan %d/%d %s (%s) stitched handoff=%d film start=%d", winner.planIndex+1, len(job.Plans), winner.plan.Kind, winner.plan.Video.Parsed.Resolution, ho, winner.startSegment)
	} else {
		log.Printf("mux: startup selected plan %d/%d %s (%s)", winner.planIndex+1, len(job.Plans), winner.plan.Kind, winner.plan.Video.Parsed.Resolution)
	}
	go m.monitorGeneration(job, state, winner)
}

// runPlaceholder starts the local intro/loop video session so the player gets
// immediate playback while the film is prepared in the background. It writes
// into a dedicated generation directory and closes placeholderWait once the
// placeholder master is ready.
func (m *Muxer) runPlaceholder(job *model.MuxJob, state *playbackState) {
	state.mu.Lock()
	state.nextGeneration++
	generationID := state.nextGeneration
	state.mu.Unlock()

	dir := filepath.Join(state.cacheDir, fmt.Sprintf("generation-%06d", generationID))
	if err := os.MkdirAll(dir, 0755); err != nil {
		state.mu.Lock()
		state.placeholderStarted = false
		wait := state.placeholderWait
		state.mu.Unlock()
		if wait != nil {
			close(wait)
		}
		log.Printf("mux: placeholder dir: %v", err)
		return
	}

	session, err := m.ffmpeg.StartPlaceholderSession(state.ctx, m.placeholderIntroPath, m.placeholderLoopPath, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		state.mu.Lock()
		state.placeholderStarted = false
		wait := state.placeholderWait
		state.mu.Unlock()
		if wait != nil {
			close(wait)
		}
		log.Printf("mux: placeholder start: %v", err)
		return
	}

	gen := &generation{
		id:            generationID,
		dir:           dir,
		session:       session,
		startSegment:  0,
		startedAt:     time.Now(),
		isPlaceholder: true,
	}

	segmentPath := generationSegmentPath(gen, 0)
	audioSegPath := generationAudioSegmentPath(gen, 0)
	masterPath := generationPlaylistPath(gen)
	videoPlPath := generationVideoPlaylistPath(gen)
	audioPlPath := generationAudioPlaylistPath(gen)
	ticker := time.NewTicker(75 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(10 * time.Second)
	for {
		if fileExists(segmentPath) && fileExists(audioSegPath) && fileExists(masterPath) && fileExists(videoPlPath) && fileExists(audioPlPath) {
			break
		}
		select {
		case <-session.Done():
			_ = os.RemoveAll(dir)
			state.mu.Lock()
			state.placeholderStarted = false
			wait := state.placeholderWait
			state.mu.Unlock()
			if wait != nil {
				close(wait)
			}
			log.Printf("mux: placeholder ended before first segment: %v", session.Err())
			return
		case <-deadline:
			session.Cancel()
			_ = os.RemoveAll(dir)
			state.mu.Lock()
			state.placeholderStarted = false
			wait := state.placeholderWait
			state.mu.Unlock()
			if wait != nil {
				close(wait)
			}
			log.Printf("mux: placeholder timed out before first segment")
			return
		case <-ticker.C:
		}
	}

	state.mu.Lock()
	state.placeholder = gen
	wait := state.placeholderWait
	state.mu.Unlock()
	if wait != nil {
		close(wait)
	}
	log.Printf("mux: placeholder playing (intro=%v loop=%v)", m.placeholderIntroPath != "", m.placeholderLoopPath != "")
}

// coordinateAttempts tries playback plans sequentially, one at a time, until
// one produces its first segment. Sequential (not parallel) is deliberate:
// each attempt opens two debrid connections (video + audio), and debrid
// services cap concurrent slots (~2-3). Running multiple plans in parallel
// could exceed that cap and trigger rate-limits on every attempt.
//
// Two phases, each with its own budget: strict first, then lenient. Strict
// accepts a track only when its language is confirmed (tag, title, or a
// single-track dubbed source). Lenient is the last resort — it also accepts an
// und/untagged track from a dubbed multiaudio source. Giving lenient its own
// window ensures a slow strict phase cannot starve the retry that matters most
// when strict fails (probes are cached, so lenient re-runs are fast).
func (m *Muxer) coordinateAttempts(job *model.MuxJob, state *playbackState, startPlan, startSegment int, timeout time.Duration) (*generation, error) {
	if startPlan >= len(job.Plans) {
		return nil, fmt.Errorf("no playback plans remain")
	}

	// Lenient gets a fresh budget (cached probes make it quick), independent of
	// how much the strict phase consumed.
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

		generation, err := m.startAttempt(ctx, job, state, planIndex, startSegment, lenient)
		if err == nil && generation != nil {
			return generation, nil
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("plan %d: %w", planIndex, err))
			log.Printf("mux: plan %d failed: %v", planIndex, err)
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

	// The A/V offset is estimated by cross-correlating the first seconds of
	// the video source's primary audio against the dubbed track. The estimate
	// is cached per source pair so it never consumes the attempt budget twice.
	// DetectAudioOffset returns how far the dubbed audio lags the video's
	// audio (positive = audio starts later); the offset we apply is inverted
	// because it must move the audio back into alignment (-itsoffset positive
	// delays the audio). The correlation is only trusted when the peak is clear.
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

	// The session must outlive the attempt: it is bound to the playback
	// context, while attemptCtx only gates how long we wait for the first
	// segment below.
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
	playlistPath := generationPlaylistPath(generation)
	ticker := time.NewTicker(75 * time.Millisecond)
	defer ticker.Stop()

	for {
		if fileExists(segmentPath) && fileExists(audioSegmentPath) && fileExists(playlistPath) {
			return generation, nil
		}
		select {
		case <-attemptCtx.Done():
			session.Cancel()
			go cleanupFailedGeneration(generation)
			return nil, fmt.Errorf("first segment deadline: %w", attemptCtx.Err())
		case <-session.Done():
			if fileExists(segmentPath) && fileExists(audioSegmentPath) && fileExists(playlistPath) {
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

func (m *Muxer) PlaylistPath(job *model.MuxJob) string {
	state := m.lookupState(job.ID)
	if state == nil {
		return ""
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.lastAccess = time.Now()
	if state.placeholder != nil && state.active == nil {
		path := generationPlaylistPath(state.placeholder)
		if fileExists(path) {
			return path
		}
	}
	if state.active == nil {
		return ""
	}
	path := generationPlaylistPath(state.active)
	if !fileExists(path) {
		return ""
	}
	return path
}

// VideoPlaylistPath returns the video-only media playlist path.
func (m *Muxer) VideoPlaylistPath(job *model.MuxJob) string {
	state := m.lookupState(job.ID)
	if state == nil {
		return ""
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.lastAccess = time.Now()
	if state.placeholder != nil && state.active == nil {
		path := generationVideoPlaylistPath(state.placeholder)
		if fileExists(path) {
			return path
		}
	}
	if state.active == nil {
		return ""
	}
	return generationVideoPlaylistPath(state.active)
}

// AudioPlaylistPath returns the audio-only media playlist path.
func (m *Muxer) AudioPlaylistPath(job *model.MuxJob) string {
	state := m.lookupState(job.ID)
	if state == nil {
		return ""
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.lastAccess = time.Now()
	if state.placeholder != nil && state.active == nil {
		path := generationAudioPlaylistPath(state.placeholder)
		if fileExists(path) {
			return path
		}
	}
	if state.active == nil {
		return ""
	}
	return generationAudioPlaylistPath(state.active)
}

func (m *Muxer) StitchedVideoPlaylist(job *model.MuxJob) ([]byte, bool) {
	state := m.lookupState(job.ID)
	if state == nil {
		return nil, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.lastAccess = time.Now()
	return m.stitchedVideoPlaylistContent(state)
}

func (m *Muxer) StitchedAudioPlaylist(job *model.MuxJob) ([]byte, bool) {
	state := m.lookupState(job.ID)
	if state == nil {
		return nil, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.lastAccess = time.Now()
	return m.stitchedAudioPlaylistContent(state)
}

// AudioSegmentPath returns the audio segment path for the given index, if it
// exists in any generation.
func (m *Muxer) AudioSegmentPath(job *model.MuxJob, segment int) string {
	state := m.lookupState(job.ID)
	if state == nil {
		return ""
	}
	state.mu.Lock()
	state.lastAccess = time.Now()
	if state.stitched && state.active != nil && state.placeholder != nil {
		if segment <= state.placeholderHighest {
			path := generationAudioSegmentPath(state.placeholder, segment)
			if fileExists(path) {
				state.mu.Unlock()
				return path
			}
		}
		path := generationAudioSegmentPath(state.active, segment)
		if fileExists(path) {
			state.mu.Unlock()
			return path
		}
		state.mu.Unlock()
		return ""
	}
	generations := append([]*generation(nil), state.all...)
	hasPlaceholder := state.placeholder != nil
	placeholder := state.placeholder
	state.mu.Unlock()

	for index := len(generations) - 1; index >= 0; index-- {
		path := generationAudioSegmentPath(generations[index], segment)
		if fileExists(path) {
			return path
		}
	}
	if hasPlaceholder {
		if path := generationAudioSegmentPath(placeholder, segment); fileExists(path) {
			return path
		}
	}
	return ""
}

func (m *Muxer) SegmentPath(job *model.MuxJob, segment int) string {
	state := m.lookupState(job.ID)
	if state == nil {
		return ""
	}
	state.mu.Lock()
	state.lastAccess = time.Now()
	if state.stitched && state.active != nil && state.placeholder != nil {
		if segment <= state.placeholderHighest {
			path := generationSegmentPath(state.placeholder, segment)
			if fileExists(path) {
				state.mu.Unlock()
				return path
			}
		}
		path := generationSegmentPath(state.active, segment)
		if fileExists(path) {
			state.mu.Unlock()
			return path
		}
		state.mu.Unlock()
		return ""
	}
	generations := append([]*generation(nil), state.all...)
	hasPlaceholder := state.placeholder != nil
	placeholder := state.placeholder
	state.mu.Unlock()

	for index := len(generations) - 1; index >= 0; index-- {
		path := generationSegmentPath(generations[index], segment)
		if fileExists(path) {
			return path
		}
	}
	if hasPlaceholder {
		if path := generationSegmentPath(placeholder, segment); fileExists(path) {
			return path
		}
	}
	return ""
}

func (m *Muxer) EnsureSegment(ctx context.Context, job *model.MuxJob, segment int) (string, error) {
	if err := m.EnsurePlaylist(ctx, job); err != nil {
		return "", err
	}
	if path := m.SegmentPath(job, segment); path != "" {
		return path, nil
	}

	state := m.lookupState(job.ID)
	if state == nil {
		return "", fmt.Errorf("playback state not found")
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, m.policy.SegmentTimeout)
	defer cancel()

	for {
		if path := m.SegmentPath(job, segment); path != "" {
			return path, nil
		}

		state.mu.Lock()
		state.lastAccess = time.Now()
		state.lastRequested = segment
		active := state.active
		recovering := state.recovering
		recoveryWait := state.recoveryWait
		nextPlan := state.nextPlan
		state.mu.Unlock()

		if active == nil {
			if !recovering {
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
				if highest >= active.startSegment && segment > highest+3 && !recovering {
					m.ensureRecovery(job, state, segment, active.planIndex, "forward seek")
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
			if err != nil && m.SegmentPath(job, segment) == "" {
				return "", err
			}
		}
	}
}

// EnsureAudioSegment waits for the audio-only segment to be produced by the
// active session (the same ffmpeg run that produces the video segments).
func (m *Muxer) EnsureAudioSegment(ctx context.Context, job *model.MuxJob, segment int) (string, error) {
	if err := m.EnsurePlaylist(ctx, job); err != nil {
		return "", err
	}
	if path := m.AudioSegmentPath(job, segment); path != "" {
		return path, nil
	}

	state := m.lookupState(job.ID)
	if state == nil {
		return "", fmt.Errorf("playback state not found")
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, m.policy.SegmentTimeout)
	defer cancel()

	for {
		if path := m.AudioSegmentPath(job, segment); path != "" {
			return path, nil
		}

		state.mu.Lock()
		state.lastAccess = time.Now()
		state.lastRequested = segment
		active := state.active
		recovering := state.recovering
		recoveryWait := state.recoveryWait
		nextPlan := state.nextPlan
		state.mu.Unlock()

		if active == nil {
			if !recovering {
				m.ensureRecovery(job, state, segment, nextPlan, "no active session")
			}
		} else {
			select {
			case <-active.session.Done():
				if !recovering {
					m.ensureRecovery(job, state, segment, nextPlan, "session ended")
				}
			default:
			}
		}

		select {
		case <-deadlineCtx.Done():
			return "", fmt.Errorf("timeout waiting for audio segment %d: %w", segment, deadlineCtx.Err())
		case <-time.After(100 * time.Millisecond):
		case <-recoveryWait:
			state.mu.Lock()
			err := state.recoveryErr
			state.mu.Unlock()
			if err != nil && m.AudioSegmentPath(job, segment) == "" {
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

func generationPlaylistPath(generation *generation) string {
	if generation == nil {
		return ""
	}
	return filepath.Join(generation.dir, "master.m3u8")
}

func generationVideoPlaylistPath(generation *generation) string {
	if generation == nil {
		return ""
	}
	return filepath.Join(generation.dir, "video", "video.m3u8")
}

func generationAudioPlaylistPath(generation *generation) string {
	if generation == nil {
		return ""
	}
	return filepath.Join(generation.dir, "audio", "audio.m3u8")
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



func rewriteStitchedPlaylists(placeholder, film *generation) error {
	return nil
}

func (m *Muxer) stitchedVideoPlaylistContent(state *playbackState) ([]byte, bool) {
	if !state.stitched || state.placeholder == nil || state.active == nil {
		return nil, false
	}
	phPath := filepath.Join(state.placeholder.dir, "video", "video.m3u8")
	phRaw, err1 := os.ReadFile(phPath)
	if err1 != nil {
		return nil, false
	}
	filmPath := filepath.Join(state.active.dir, "video", "video.m3u8")
	filmRaw, err2 := os.ReadFile(filmPath)
	if err2 != nil {
		phLines := strings.Split(strings.TrimRight(string(phRaw), "\n"), "\n")
		var out []string
		for _, l := range phLines {
			if strings.TrimSpace(l) == "#EXT-X-ENDLIST" {
				continue
			}
			out = append(out, l)
		}
		return []byte(strings.Join(out, "\n") + "\n"), true
	}
	phLines := strings.Split(strings.TrimRight(string(phRaw), "\n"), "\n")
	filmLines := strings.Split(strings.TrimRight(string(filmRaw), "\n"), "\n")
	var out []string
	headerDone := false
	for _, l := range phLines {
		trim := strings.TrimSpace(l)
		if strings.HasPrefix(trim, "#EXTINF:") || (!headerDone && trim != "" && !strings.HasPrefix(trim, "#")) {
			headerDone = true
		}
		if headerDone && strings.HasPrefix(trim, "#EXT-X-ENDLIST") {
			continue
		}
		out = append(out, l)
		if strings.HasPrefix(trim, "#EXTINF:") {
			headerDone = true
		}
	}
	hasFilmSeg := false
	for _, l := range filmLines {
		if strings.HasPrefix(strings.TrimSpace(l), "#EXTINF:") {
			hasFilmSeg = true
			break
		}
	}
	if hasFilmSeg {
		needsDisc := true
		for _, l := range out {
			if strings.TrimSpace(l) == "#EXT-X-DISCONTINUITY" {
				needsDisc = false
				break
			}
		}
		if needsDisc {
			out = append(out, "#EXT-X-DISCONTINUITY")
		}
	}
	inHeader := true
	for _, l := range filmLines {
		trim := strings.TrimSpace(l)
		if inHeader {
			if strings.HasPrefix(trim, "#EXTM3U") || strings.HasPrefix(trim, "#EXT-X-VERSION") || strings.HasPrefix(trim, "#EXT-X-TARGETDURATION") || strings.HasPrefix(trim, "#EXT-X-MEDIA-SEQUENCE") || strings.HasPrefix(trim, "#EXT-X-PLAYLIST-TYPE") || strings.HasPrefix(trim, "#EXT-X-INDEPENDENT-SEGMENTS") {
				continue
			}
			if strings.HasPrefix(trim, "#EXTINF:") || (trim != "" && !strings.HasPrefix(trim, "#")) {
				inHeader = false
			} else {
				continue
			}
		}
		if strings.HasPrefix(trim, "#EXT-X-ENDLIST") {
			continue
		}
		out = append(out, l)
	}
	return []byte(strings.Join(out, "\n") + "\n"), true
}

func (m *Muxer) stitchedAudioPlaylistContent(state *playbackState) ([]byte, bool) {
	if !state.stitched || state.placeholder == nil || state.active == nil {
		return nil, false
	}
	phPath := filepath.Join(state.placeholder.dir, "audio", "audio.m3u8")
	phRaw, err1 := os.ReadFile(phPath)
	if err1 != nil {
		return nil, false
	}
	filmPath := filepath.Join(state.active.dir, "audio", "audio.m3u8")
	filmRaw, err2 := os.ReadFile(filmPath)
	if err2 != nil {
		phLines := strings.Split(strings.TrimRight(string(phRaw), "\n"), "\n")
		var out []string
		for _, l := range phLines {
			if strings.TrimSpace(l) == "#EXT-X-ENDLIST" {
				continue
			}
			out = append(out, l)
		}
		return []byte(strings.Join(out, "\n") + "\n"), true
	}
	phLines := strings.Split(strings.TrimRight(string(phRaw), "\n"), "\n")
	filmLines := strings.Split(strings.TrimRight(string(filmRaw), "\n"), "\n")
	var out []string
	headerDone := false
	for _, l := range phLines {
		trim := strings.TrimSpace(l)
		if strings.HasPrefix(trim, "#EXTINF:") || (!headerDone && trim != "" && !strings.HasPrefix(trim, "#")) {
			headerDone = true
		}
		if headerDone && strings.HasPrefix(trim, "#EXT-X-ENDLIST") {
			continue
		}
		out = append(out, l)
		if strings.HasPrefix(trim, "#EXTINF:") {
			headerDone = true
		}
	}
	hasFilmSeg := false
	for _, l := range filmLines {
		if strings.HasPrefix(strings.TrimSpace(l), "#EXTINF:") {
			hasFilmSeg = true
			break
		}
	}
	if hasFilmSeg {
		needsDisc := true
		for _, l := range out {
			if strings.TrimSpace(l) == "#EXT-X-DISCONTINUITY" {
				needsDisc = false
				break
			}
		}
		if needsDisc {
			out = append(out, "#EXT-X-DISCONTINUITY")
		}
	}
	inHeader := true
	for _, l := range filmLines {
		trim := strings.TrimSpace(l)
		if inHeader {
			if strings.HasPrefix(trim, "#EXTM3U") || strings.HasPrefix(trim, "#EXT-X-VERSION") || strings.HasPrefix(trim, "#EXT-X-TARGETDURATION") || strings.HasPrefix(trim, "#EXT-X-MEDIA-SEQUENCE") || strings.HasPrefix(trim, "#EXT-X-PLAYLIST-TYPE") || strings.HasPrefix(trim, "#EXT-X-INDEPENDENT-SEGMENTS") {
				continue
			}
			if strings.HasPrefix(trim, "#EXTINF:") || (trim != "" && !strings.HasPrefix(trim, "#")) {
				inHeader = false
			} else {
				continue
			}
		}
		if strings.HasPrefix(trim, "#EXT-X-ENDLIST") {
			continue
		}
		out = append(out, l)
	}
	return []byte(strings.Join(out, "\n") + "\n"), true
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
