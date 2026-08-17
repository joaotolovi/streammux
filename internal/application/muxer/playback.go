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
// static once the film is ready: segment n always maps to file seg_%05d.ts of
// whichever generation produced it, and the media playlists expose the full
// film duration so the player can seek anywhere immediately. Before the film
// is ready, an optional live placeholder serves the same URLs while sources
// are resolved and probed.
type playbackState struct {
	mu sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc

	cacheDir string

	// active is the ffmpeg session currently encoding the film (or the
	// terminal error video). Segments produced by earlier generations remain
	// servable via all (searched newest first).
	active *generation
	all    []*generation

	// placeholder is the live intro playing while the film prepares.
	// retiredPlaceholder keeps its segments servable after the handoff.
	placeholder        *generation
	retiredPlaceholder *generation
	placeholderStarted bool
	placeholderWait    chan struct{}

	// filmBase is the public segment index where film content 0:00 lives
	// (nonzero when a placeholder played first). While >= 0 the placeholder
	// live window is frozen at [..filmBase-1] so the media sequence can only
	// move forward across the handoff (players reject sequence regression).
	filmBase        int
	placeholderLive bool
	discontinuities []int

	// errorGeneration is the terminal "no source worked" video.
	errorGeneration *generation
	errorStart      int

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

	duration      float64 // film duration in seconds (from probe)
	lastRequested int
	maxRequested  int
	lastAccess    time.Time
	lastRecovery  time.Time
	closed        bool

	deliveries []deliverySample
}

type generation struct {
	id           uint64
	planIndex    int
	plan         model.PlaybackPlan
	prepared     *preparedPlan
	dir          string
	session      *ffmpeg.Session
	startSegment int // first public segment number this generation writes
	startedAt    time.Time
	isLocal      bool // placeholder or error video
	isError      bool
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

// EnsurePlaylist returns as soon as a playable timeline exists. With a
// placeholder configured that means the placeholder is live (the film keeps
// preparing in the background and takes over the timeline when ready).
// Without one, it blocks until the film (or the error video) is ready.
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
	if state.placeholder != nil {
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

	if m.placeholderPath != "" && !state.placeholderStarted && state.startErr == nil {
		state.placeholderStarted = true
		state.placeholderWait = make(chan struct{})
		go m.runPlaceholder(job, state)
	}

	if !state.starting {
		state.starting = true
		state.startWait = make(chan struct{})
		state.startErr = nil
		state.lastStart = time.Now()
		go m.runStartup(job, state)
	}
	phWait := state.placeholderWait
	wait := state.startWait
	state.mu.Unlock()

	if phWait != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-phWait:
		}
		state.mu.Lock()
		if state.placeholder != nil || state.active != nil {
			state.mu.Unlock()
			return nil
		}
		// Placeholder failed to start; fall through to waiting for the film
		// when its startup is still in flight.
		filmWait := state.startWait
		starting := state.starting
		state.mu.Unlock()
		if starting && filmWait != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-filmWait:
			}
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

// lastCommonSegment returns the last segment index advertised by both the
// video and audio playlists of a live generation.
func lastCommonSegment(gen *generation) int {
	if gen == nil {
		return -1
	}
	videoLast := lastPlaylistSegment(generationVideoPlaylistPath(gen))
	audioLast := lastPlaylistSegment(generationAudioPlaylistPath(gen))
	if videoLast < 0 || audioLast < 0 {
		return -1
	}
	if audioLast < videoLast {
		return audioLast
	}
	return videoLast
}

func lastPlaylistSegment(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	last := -1
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var segment int
		if _, err := fmt.Sscanf(line, "seg_%05d.ts", &segment); err == nil && segment > last {
			last = segment
		}
	}
	return last
}

// runPlaceholder starts the local intro so the player gets immediate playback
// while the film is prepared in the background.
func (m *Muxer) runPlaceholder(job *model.MuxJob, state *playbackState) {
	state.mu.Lock()
	state.nextGeneration++
	generationID := state.nextGeneration
	state.mu.Unlock()

	dir := filepath.Join(state.cacheDir, fmt.Sprintf("generation-%06d", generationID))
	session, err := m.ffmpeg.StartSinglePlaceholderSession(state.ctx, m.placeholderPath, dir, true)
	fail := func(format string, args ...any) {
		_ = os.RemoveAll(dir)
		state.mu.Lock()
		state.placeholderStarted = false
		wait := state.placeholderWait
		state.placeholderWait = nil
		state.mu.Unlock()
		if wait != nil {
			close(wait)
		}
		log.Printf("mux: placeholder: "+format, args...)
	}
	if err != nil {
		fail("%v", err)
		return
	}

	gen := &generation{
		id:           generationID,
		dir:          dir,
		session:      session,
		startSegment: 0,
		startedAt:    time.Now(),
		isLocal:      true,
	}

	segmentPath := generationSegmentPath(gen, 0)
	audioSegPath := generationAudioSegmentPath(gen, 0)
	masterPath := generationVideoPlaylistPath(gen)
	audioPlPath := generationAudioPlaylistPath(gen)
	ticker := time.NewTicker(75 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(10 * time.Second)
	for {
		if fileExists(segmentPath) && fileExists(audioSegPath) && fileExists(masterPath) && fileExists(audioPlPath) {
			break
		}
		select {
		case <-session.Done():
			fail("ended before first segment: %v", session.Err())
			return
		case <-deadline:
			session.Cancel()
			fail("timed out before first segment")
			return
		case <-ticker.C:
		}
	}

	state.mu.Lock()
	if state.active != nil || state.closed {
		gen.session.Cancel()
		wait := state.placeholderWait
		state.placeholderStarted = false
		state.mu.Unlock()
		_ = os.RemoveAll(dir)
		if wait != nil {
			close(wait)
		}
		return
	}
	state.placeholder = gen
	state.all = append(state.all, gen)
	wait := state.placeholderWait
	state.mu.Unlock()
	if wait != nil {
		close(wait)
	}
	log.Printf("mux: placeholder playing")
}

// runStartup prepares film sources (resolve + probe + A/V sync estimate) and
// launches the winning session. With a placeholder playing, the film is
// numbered from the placeholder's last common segment so the handoff is a
// single DISCONTINUITY on an otherwise static timeline.
func (m *Muxer) runStartup(job *model.MuxJob, state *playbackState) {
	deadline := time.Now().Add(m.policy.StartupTimeout)

	var winner *generation
	var lastErr error
	for {
		state.mu.Lock()
		startPlan := state.nextPlan
		state.mu.Unlock()
		if startPlan < 0 {
			startPlan = 0
		}

		prepared, planIndex, err := m.prepareBestPlan(job, state, startPlan, time.Until(deadline))
		if err != nil {
			lastErr = err
			break
		}

		// Keep the placeholder playing for its minimum time even when the
		// film is ready sooner: cutting too early feels like a glitch.
		state.mu.Lock()
		ph := state.placeholder
		state.mu.Unlock()
		if ph != nil && m.policy.PlaceholderMinTime > 0 {
			if remaining := m.policy.PlaceholderMinTime - time.Since(ph.startedAt); remaining > 0 {
				select {
				case <-time.After(remaining):
				case <-state.ctx.Done():
				}
			}
		}

		// Handoff point: freeze the advertised placeholder window at its
		// last common segment. The session keeps running until the film
		// takes over (rendering caps at filmBase-1), so the media sequence
		// can only move forward — players reject sequence regression.
		state.mu.Lock()
		base := 0
		if ph != nil {
			if common := lastCommonSegment(ph); common >= 0 {
				base = common + 1
			}
			state.placeholderLive = false
		}
		state.filmBase = base
		state.mu.Unlock()

		gen, err := m.launchGeneration(job, state, planIndex, prepared, base, 0)
		if err == nil {
			winner = gen
			break
		}
		lastErr = err
		log.Printf("mux: plan %d launch failed: %v", planIndex, err)
		state.mu.Lock()
		if planIndex+1 > state.nextPlan {
			state.nextPlan = planIndex + 1
		}
		state.mu.Unlock()
		if time.Now().After(deadline) {
			break
		}
	}

	if winner == nil {
		m.startupFailed(job, state, lastErr)
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
	placeholder := state.placeholder
	state.placeholder = nil
	state.retiredPlaceholder = placeholder
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
	if state.filmBase > 0 {
		state.discontinuities = append(state.discontinuities, state.filmBase)
	}
	base := state.filmBase
	wait := state.startWait
	state.mu.Unlock()

	if placeholder != nil {
		placeholder.session.Cancel()
	}

	job.Duration = winner.prepared.duration
	job.PlaylistReady = true
	if wait != nil {
		close(wait)
	}
	log.Printf("mux: startup selected plan %d/%d %s (%s) at segment %d", winner.planIndex+1, len(job.Plans), winner.plan.Kind, winner.plan.Video.Parsed.Resolution, base)
	go m.monitorGeneration(job, state, winner)
}

// startupFailed handles total startup failure: prefer the direct subtitled
// fallback, then the error video, then a plain error.
func (m *Muxer) startupFailed(job *model.MuxJob, state *playbackState, cause error) {
	direct := m.resolveDirectFallback(job, state)

	errGen := m.startErrorGeneration(state, -1)
	state.mu.Lock()
	if errGen != nil {
		state.active = errGen
		state.errorGeneration = errGen
		state.starting = false
		state.startErr = cause
		state.directURL = ""
		wait := state.startWait
		phWait := state.placeholderWait
		state.mu.Unlock()
		if wait != nil {
			close(wait)
		}
		if phWait != nil {
			select {
			case <-phWait:
			default:
				close(phWait)
			}
		}
		log.Printf("mux: startup failed; serving error video: %v", cause)
		return
	}
	state.starting = false
	state.startErr = cause
	state.directURL = direct
	wait := state.startWait
	phWait := state.placeholderWait
	state.mu.Unlock()
	if wait != nil {
		close(wait)
	}
	if phWait != nil {
		select {
		case <-phWait:
		default:
			close(phWait)
		}
	}
	log.Printf("mux: startup failed (next retry from plan %d): %v", state.nextPlan, cause)
}

// startErrorGeneration launches the local error video continuing the current
// timeline. atSeg < 0 continues after the live placeholder (or from 0).
func (m *Muxer) startErrorGeneration(state *playbackState, atSeg int) *generation {
	if m.errorPath == "" {
		return nil
	}

	state.mu.Lock()
	state.nextGeneration++
	generationID := state.nextGeneration
	ph := state.placeholder
	state.mu.Unlock()

	start := atSeg
	if start < 0 {
		start = 0
		if ph != nil {
			if common := lastCommonSegment(ph); common >= 0 {
				start = common + 1
			}
			ph.session.Cancel()
			state.mu.Lock()
			state.placeholder = nil
			state.retiredPlaceholder = ph
			state.mu.Unlock()
		}
	}

	dir := filepath.Join(state.cacheDir, fmt.Sprintf("generation-%06d", generationID))
	session, err := m.ffmpeg.StartSinglePlaceholderSession(state.ctx, m.errorPath, dir, false)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil
	}
	gen := &generation{
		id:           generationID,
		dir:          dir,
		session:      session,
		startSegment: start,
		startedAt:    time.Now(),
		isLocal:      true,
		isError:      true,
	}

	ticker := time.NewTicker(75 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(10 * time.Second)
	for {
		if fileExists(generationSegmentPath(gen, start)) && fileExists(generationAudioSegmentPath(gen, start)) {
			state.mu.Lock()
			state.all = append(state.all, gen)
			state.errorStart = start
			state.mu.Unlock()
			return gen
		}
		select {
		case <-session.Done():
			_ = os.RemoveAll(dir)
			return nil
		case <-deadline:
			session.Cancel()
			_ = os.RemoveAll(dir)
			return nil
		case <-ticker.C:
		}
	}
}

// prepareBestPlan prepares plans sequentially (strict, then lenient) without
// launching any session, returning the first plan that validates.
func (m *Muxer) prepareBestPlan(job *model.MuxJob, state *playbackState, startPlan int, budget time.Duration) (*preparedPlan, int, error) {
	if startPlan >= len(job.Plans) {
		return nil, 0, fmt.Errorf("no playback plans remain")
	}
	if budget <= 0 {
		budget = time.Second
	}

	lenientBudget := budget / 2
	if lenientBudget < 5*time.Second {
		lenientBudget = 5 * time.Second
	}

	strictCtx, strictCancel := context.WithTimeout(state.ctx, budget)
	strictCancelFn := strictCancel
	prepared, planIndex, strictErr := m.preparePlans(strictCtx, job, state, startPlan, false)
	strictCancelFn()
	if prepared != nil {
		return prepared, planIndex, nil
	}

	lenientCtx, lenientCancel := context.WithTimeout(state.ctx, lenientBudget)
	prepared, planIndex, lenientErr := m.preparePlans(lenientCtx, job, state, startPlan, true)
	lenientCancel()
	if prepared != nil {
		return prepared, planIndex, nil
	}
	return nil, 0, errors.Join(strictErr, lenientErr)
}

func (m *Muxer) preparePlans(ctx context.Context, job *model.MuxJob, state *playbackState, startPlan int, lenient bool) (*preparedPlan, int, error) {
	var failures []error
	for planIndex := startPlan; planIndex < len(job.Plans); planIndex++ {
		select {
		case <-ctx.Done():
			failures = append(failures, ctx.Err())
			return nil, 0, errors.Join(failures...)
		default:
		}
		if !job.Plans[planIndex].HasTargetAudio {
			continue
		}
		prepared, err := m.prepareAttempt(ctx, job, state, planIndex, lenient)
		if err == nil {
			return prepared, planIndex, nil
		}
		failures = append(failures, fmt.Errorf("plan %d: %w", planIndex, err))
		log.Printf("mux: plan %d failed: %v", planIndex, err)
		state.mu.Lock()
		if planIndex+1 > state.nextPlan {
			state.nextPlan = planIndex + 1
		}
		state.mu.Unlock()
	}
	return nil, 0, errors.Join(failures...)
}

// prepareAttempt resolves and probes a plan and estimates the A/V offset.
func (m *Muxer) prepareAttempt(ctx context.Context, job *model.MuxJob, state *playbackState, planIndex int, lenient bool) (*preparedPlan, error) {
	plan := job.Plans[planIndex]
	prepared, err := m.preparePlanMode(ctx, job, plan, lenient)
	if err != nil {
		return nil, err
	}
	if prepared.duration <= 0 {
		return nil, fmt.Errorf("plan has no probeable duration")
	}

	dualSource := strings.TrimSpace(prepared.audioURL) != "" && prepared.audioURL != prepared.videoURL
	if !dualSource {
		return prepared, nil
	}
	if _, ok := m.audioOffsetFor(plan); ok {
		return prepared, nil
	}
	lag, track, confidence, err := m.detectOffset(prepared)
	if err != nil {
		log.Printf("mux: audio offset estimation failed (continuing without): %v", err)
		return prepared, nil
	}
	if confidence < ffmpeg.SyncMinConfidence {
		log.Printf("mux: audio offset inconclusive (conf %.1f, continuing without): video track a:%d", confidence, track)
		return prepared, nil
	}
	offset := -lag
	m.cacheAudioOffset(plan, offset)
	log.Printf("mux: estimated audio offset %s from video track a:%d (conf %.1f)", offset, track, confidence)
	return prepared, nil
}

// launchGeneration starts the ffmpeg session for a prepared plan and waits
// for its first segment. startNumber is the first public segment written;
// startTime is the content offset in seconds (0 for a fresh start).
func (m *Muxer) launchGeneration(job *model.MuxJob, state *playbackState, planIndex int, prepared *preparedPlan, startNumber int, startTime float64) (*generation, error) {
	attemptCtx, cancel := context.WithTimeout(state.ctx, m.policy.AttemptTimeout)
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

	var audioOffset time.Duration
	if offset, ok := m.audioOffsetFor(prepared.plan); ok {
		audioOffset = offset
	}

	session, err := m.ffmpeg.StartSession(state.ctx, ffmpeg.SessionSpec{
		VideoURL:        prepared.videoURL,
		AudioURL:        prepared.audioURL,
		VideoTrackIndex: prepared.videoTrackIndex,
		AudioTrackIndex: prepared.audioTrackIndex,
		StartSegment:    startNumber,
		StartTime:       startTime,
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
		plan:         prepared.plan,
		prepared:     prepared,
		dir:          dir,
		session:      session,
		startSegment: startNumber,
		startedAt:    time.Now(),
	}

	segmentPath := generationSegmentPath(generation, startNumber)
	audioSegmentPath := generationAudioSegmentPath(generation, startNumber)
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
				m.invalidatePlanSources(prepared.plan)
				return nil, session.Err()
			}
			return nil, fmt.Errorf("ffmpeg ended before producing segment %d", startNumber)
		case <-ticker.C:
		}
	}
}

// coordinateAttempts prepares and launches plans sequentially until one
// produces its first segment. Used by recovery and seek paths where no
// placeholder handoff is involved; startTime derives from filmBase.
func (m *Muxer) coordinateAttempts(job *model.MuxJob, state *playbackState, startPlan, startSegment int, timeout time.Duration) (*generation, error) {
	if startPlan >= len(job.Plans) {
		return nil, fmt.Errorf("no playback plans remain")
	}

	deadline := time.Now().Add(timeout)
	var failures []error
	for planIndex := startPlan; planIndex < len(job.Plans); planIndex++ {
		if !job.Plans[planIndex].HasTargetAudio {
			continue
		}
		if time.Now().After(deadline) {
			break
		}
		prepared, err := m.prepareAttempt(state.ctx, job, state, planIndex, false)
		if err != nil {
			failures = append(failures, fmt.Errorf("plan %d: %w", planIndex, err))
			state.mu.Lock()
			if planIndex+1 > state.nextPlan {
				state.nextPlan = planIndex + 1
			}
			state.mu.Unlock()
			continue
		}
		state.mu.Lock()
		base := state.filmBase
		state.mu.Unlock()
		startTime := float64(startSegment-base) * ffmpeg.SegDuration()
		if startTime < 0 {
			startTime = 0
		}
		gen, err := m.launchGeneration(job, state, planIndex, prepared, startSegment, startTime)
		if err == nil {
			return gen, nil
		}
		failures = append(failures, fmt.Errorf("plan %d: %w", planIndex, err))
		log.Printf("mux: plan %d failed: %v", planIndex, err)
	}
	if len(failures) == 0 {
		return nil, fmt.Errorf("no playback plans remain")
	}
	return nil, errors.Join(failures...)
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

// MasterPlaylist renders the master we serve. The audio rendition group is
// declared in every phase — without it players ignore the audio playlist and
// every film plays silently.
func (m *Muxer) MasterPlaylist(job *model.MuxJob) ([]byte, bool) {
	state := m.lookupState(job.ID)
	if state == nil {
		return nil, false
	}
	state.mu.Lock()
	state.lastAccess = time.Now()
	active := state.active
	placeholder := state.placeholder
	duration := state.duration
	state.mu.Unlock()

	if active != nil && !active.isError && active.prepared != nil && duration > 0 {
		bitrate := active.prepared.videoBitrate
		if bitrate <= 0 {
			bitrate = float64(active.plan.EstimatedBandwidth())
		}
		bandwidth := int64(bitrate * 1.2)
		if bandwidth <= 0 {
			bandwidth = 8_000_000
		}
		return m.renderMaster(bandwidth, active.prepared.videoWidth, active.prepared.videoHeight, job.TargetLanguage), true
	}
	if placeholder != nil || (active != nil && active.isError) {
		// Small placeholder/error rendition.
		return m.renderMaster(3_000_000, 0, 0, job.TargetLanguage), true
	}
	return nil, false
}

func (m *Muxer) renderMaster(bandwidth int64, width, height int, targetLanguage string) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:6\n")
	code := ffmpeg.LanguageCode(targetLanguage)
	name := targetLanguage
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
	if width > 0 && height > 0 {
		streamInf += fmt.Sprintf(",RESOLUTION=%dx%d", width, height)
	}
	streamInf += ",AUDIO=\"aud\"\n"
	b.WriteString(streamInf)
	b.WriteString("video/video.m3u8\n")
	return []byte(b.String())
}

// VideoPlaylist renders the video media playlist for the current phase.
func (m *Muxer) VideoPlaylist(job *model.MuxJob) ([]byte, bool) {
	return m.renderMediaPlaylist(job)
}

// AudioPlaylist renders the audio media playlist for the current phase.
func (m *Muxer) AudioPlaylist(job *model.MuxJob) ([]byte, bool) {
	return m.renderMediaPlaylist(job)
}

func (m *Muxer) renderMediaPlaylist(job *model.MuxJob) ([]byte, bool) {
	state := m.lookupState(job.ID)
	if state == nil {
		return nil, false
	}
	state.mu.Lock()
	state.lastAccess = time.Now()
	placeholder := state.placeholder
	active := state.active
	retired := state.retiredPlaceholder
	base := state.filmBase
	duration := state.duration
	disc := append([]int(nil), state.discontinuities...)
	errGen := state.errorGeneration
	errStart := state.errorStart
	state.mu.Unlock()

	// Live placeholder phase: synchronized sliding window of both renditions,
	// capped at the frozen handoff point once the film is being launched.
	if placeholder != nil && active == nil {
		return synchronizedLiveWindow(placeholder, base-1)
	}

	segDur := ffmpeg.SegDuration()
	if segDur <= 0 {
		segDur = 4.0
	}

	discSet := make(map[int]bool, len(disc))
	for _, d := range disc {
		discSet[d] = true
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:6\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(segDur))))

	if duration > 0 {
		// Film timeline (possibly truncated by the error tail).
		segs := computeEqualLengthSegments(segDur, duration)
		first := base
		last := len(segs) - 1
		if errGen != nil && errStart <= last {
			last = errStart - 1
		}
		b.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", first))
		b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
		b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
		for i := first; i <= last; i++ {
			if discSet[i] {
				b.WriteString("#EXT-X-DISCONTINUITY\n")
			}
			b.WriteString(fmt.Sprintf("#EXTINF:%.6f,\n", segs[i]))
			b.WriteString(fmt.Sprintf("seg_%05d.ts\n", i))
		}
		if errGen != nil {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
			for i := errStart; i < errStart+m.errorSegmentCount(); i++ {
				b.WriteString(fmt.Sprintf("#EXTINF:%.6f,\n", segDur))
				b.WriteString(fmt.Sprintf("seg_%05d.ts\n", i))
			}
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		return []byte(b.String()), true
	}

	if errGen != nil {
		// Error-only timeline: placeholder prefix (if any) + error video.
		b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
		b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
		b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
		for i := 0; i < errStart; i++ {
			b.WriteString(fmt.Sprintf("#EXTINF:%.6f,\n", segDur))
			b.WriteString(fmt.Sprintf("seg_%05d.ts\n", i))
		}
		if errStart > 0 {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		for i := errStart; i < errStart+m.errorSegmentCount(); i++ {
			b.WriteString(fmt.Sprintf("#EXTINF:%.6f,\n", segDur))
			b.WriteString(fmt.Sprintf("seg_%05d.ts\n", i))
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		return []byte(b.String()), true
	}

	_ = retired
	return nil, false
}

// synchronizedLiveWindow renders the common A/V window of a live generation
// so both renditions advertise exactly the same segments. cap limits the last
// advertised segment (>=0) to freeze the window at a handoff point.
func synchronizedLiveWindow(gen *generation, cap int) ([]byte, bool) {
	videoPlaylist, err := readLivePlaylist(generationVideoPlaylistPath(gen))
	if err != nil {
		return nil, false
	}
	audioPlaylist, err := readLivePlaylist(generationAudioPlaylistPath(gen))
	if err != nil {
		return nil, false
	}
	first := videoPlaylist.first
	if audioPlaylist.first > first {
		first = audioPlaylist.first
	}
	last := videoPlaylist.last
	if audioPlaylist.last < last {
		last = audioPlaylist.last
	}
	if cap >= 0 && last > cap {
		last = cap
	}
	if first < 0 || last < first {
		return nil, false
	}
	return videoPlaylist.render(first, last), true
}

type livePlaylist struct {
	header   []string
	segments map[int][]string
	first    int
	last     int
}

func readLivePlaylist(path string) (livePlaylist, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return livePlaylist{}, err
	}
	playlist := livePlaylist{segments: make(map[int][]string), first: -1, last: -1}
	var pending []string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "#EXT-X-MEDIA-SEQUENCE:"):
			continue
		case strings.HasPrefix(trimmed, "#EXTINF:"):
			pending = append(pending[:0], line)
		case trimmed != "" && !strings.HasPrefix(trimmed, "#"):
			var segment int
			if _, err := fmt.Sscanf(trimmed, "seg_%05d.ts", &segment); err != nil {
				continue
			}
			lines := append([]string(nil), pending...)
			lines = append(lines, line)
			playlist.segments[segment] = lines
			if playlist.first < 0 || segment < playlist.first {
				playlist.first = segment
			}
			if segment > playlist.last {
				playlist.last = segment
			}
			pending = pending[:0]
		case len(pending) == 0:
			playlist.header = append(playlist.header, line)
		default:
			pending = append(pending, line)
		}
	}
	return playlist, nil
}

func (p livePlaylist) render(first, last int) []byte {
	var out []string
	insertedSequence := false
	for _, line := range p.header {
		out = append(out, line)
		if strings.HasPrefix(strings.TrimSpace(line), "#EXT-X-TARGETDURATION:") {
			out = append(out, fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d", first))
			insertedSequence = true
		}
	}
	if !insertedSequence {
		out = append(out, fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d", first))
	}
	for segment := first; segment <= last; segment++ {
		out = append(out, p.segments[segment]...)
	}
	return []byte(strings.Join(out, "\n") + "\n")
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
	errGen := state.errorGeneration
	errStart := state.errorStart
	all := append([]*generation(nil), state.all...)
	state.mu.Unlock()

	if segment < 0 {
		return ""
	}
	// While the placeholder is live the timeline is open-ended.
	if duration <= 0 && errGen == nil {
		// unbounded
	} else if errGen != nil {
		if segment >= errStart+m.errorSegmentCount() {
			return ""
		}
	} else if segment >= vodSegmentCount(duration) {
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
	count := 0
	if state.duration > 0 {
		count = vodSegmentCount(state.duration)
	}
	hasError := state.errorGeneration != nil
	state.mu.Unlock()
	if count > 0 && segment >= count && !hasError {
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
		placeholderActive := state.placeholder != nil && active == nil
		recovering := state.recovering
		recoveryWait := state.recoveryWait
		recoveryErr := state.recoveryErr
		nextPlan := state.nextPlan
		if !placeholderActive {
			state.lastRequested = segment
			if segment > state.maxRequested {
				state.maxRequested = segment
			}
		}
		maxReq := state.maxRequested
		state.mu.Unlock()

		if placeholderActive {
			// Film is preparing behind the placeholder; just wait.
			select {
			case <-deadlineCtx.Done():
				return "", fmt.Errorf("timeout waiting for placeholder segment %d: %w", segment, deadlineCtx.Err())
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}

		if active == nil {
			switch {
			case recovering:
				// recovery in flight; wait below
			case recoveryErr != nil:
				return "", recoveryErr
			default:
				m.ensureRecovery(job, state, segment, nextPlan, "no active session")
			}
		} else if !active.isError {
			highest := highestCompleteSegment(active.dir)
			select {
			case <-active.session.Done():
				if !recovering {
					m.ensureRecovery(job, state, segment, nextPlan, "session ended")
				}
			default:
				if (segment < active.startSegment || isForwardSeek(maxReq, segment, highest, active.startSegment)) && !recovering {
					m.ensureRecovery(job, state, segment, active.planIndex, "seek")
				}
			}
		} else {
			// Error video: it produces in real time; just wait.
			select {
			case <-deadlineCtx.Done():
				return "", fmt.Errorf("timeout waiting for error segment %d: %w", segment, deadlineCtx.Err())
			case <-time.After(50 * time.Millisecond):
			}
			continue
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
	if state.closed || state.recovering || state.errorGeneration != nil {
		state.mu.Unlock()
		return
	}
	if startPlan >= len(job.Plans) {
		state.recoveryErr = fmt.Errorf("fallback exhausted after %s", reason)
		state.mu.Unlock()
		// Every plan failed mid-playback: serve the error video from here.
		if errGen := m.startErrorGeneration(state, startSegment); errGen != nil {
			state.mu.Lock()
			state.active = errGen
			state.errorGeneration = errGen
			state.recoveryErr = nil
			state.mu.Unlock()
			log.Printf("mux: recovery exhausted after %s; serving error video at segment %d", reason, startSegment)
		}
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
