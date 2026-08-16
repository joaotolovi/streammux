package muxer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
}

type attemptResult struct {
	generation *generation
	planIndex  int
	err        error
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

// EnsurePlaylist starts the bounded startup race on first access and waits only
// for a real FFmpeg playlist containing a complete first segment.
func (m *Muxer) EnsurePlaylist(ctx context.Context, job *model.MuxJob) error {
	state, err := m.stateFor(job)
	if err != nil {
		return err
	}

	start := false
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
	winner, err := m.coordinateAttempts(job, state, 0, 0, m.policy.StartupTimeout)
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

func (m *Muxer) coordinateAttempts(job *model.MuxJob, state *playbackState, startPlan, startSegment int, timeout time.Duration) (*generation, error) {
	if startPlan >= len(job.Plans) {
		return nil, fmt.Errorf("no playback plans remain")
	}

	ctx, cancel := context.WithTimeout(state.ctx, timeout)
	defer cancel()

	results := make(chan attemptResult, len(job.Plans)-startPlan)
	nextPlan := startPlan
	running := 0
	var failures []error

	launchNext := func() bool {
		if nextPlan >= len(job.Plans) || running >= 2 {
			return false
		}
		planIndex := nextPlan
		nextPlan++
		running++
		go func() {
			generation, err := m.startAttempt(ctx, job, state, planIndex, startSegment)
			results <- attemptResult{generation: generation, planIndex: planIndex, err: err}
		}()
		return true
	}

	launchNext()
	fallbackTimer := time.NewTimer(m.policy.FallbackDelay)
	defer fallbackTimer.Stop()

	for {
		if running == 0 && nextPlan >= len(job.Plans) {
			return nil, errors.Join(failures...)
		}

		select {
		case <-ctx.Done():
			failures = append(failures, ctx.Err())
			return nil, errors.Join(failures...)

		case <-fallbackTimer.C:
			launchNext()
			if nextPlan < len(job.Plans) {
				fallbackTimer.Reset(m.policy.FallbackDelay)
			}

		case result := <-results:
			running--
			if result.err == nil && result.generation != nil {
				return result.generation, nil
			}
			if result.err != nil {
				failures = append(failures, fmt.Errorf("plan %d: %w", result.planIndex, result.err))
				log.Printf("mux: plan %d failed: %v", result.planIndex, result.err)
			}
			for running < 2 && nextPlan < len(job.Plans) {
				if !launchNext() {
					break
				}
			}
		}
	}
}

func (m *Muxer) startAttempt(parent context.Context, job *model.MuxJob, state *playbackState, planIndex, startSegment int) (*generation, error) {
	attemptCtx, cancel := context.WithTimeout(parent, m.policy.AttemptTimeout)
	defer cancel()

	plan := job.Plans[planIndex]
	prepared, err := m.preparePlan(attemptCtx, job, plan)
	if err != nil {
		return nil, err
	}

	state.mu.Lock()
	state.nextGeneration++
	generationID := state.nextGeneration
	state.mu.Unlock()

	dir := filepath.Join(state.cacheDir, fmt.Sprintf("generation-%06d", generationID))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create generation directory: %w", err)
	}

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
	playlistPath := generationPlaylistPath(generation)
	ticker := time.NewTicker(75 * time.Millisecond)
	defer ticker.Stop()

	for {
		if fileExists(segmentPath) && fileExists(playlistPath) {
			return generation, nil
		}
		select {
		case <-attemptCtx.Done():
			session.Cancel()
			go cleanupFailedGeneration(generation)
			return nil, fmt.Errorf("first segment deadline: %w", attemptCtx.Err())
		case <-session.Done():
			if fileExists(segmentPath) && fileExists(playlistPath) {
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
	if state.active == nil {
		return ""
	}
	path := generationPlaylistPath(state.active)
	if !fileExists(path) {
		return ""
	}
	return path
}

func (m *Muxer) SegmentPath(job *model.MuxJob, segment int) string {
	state := m.lookupState(job.ID)
	if state == nil {
		return ""
	}
	state.mu.Lock()
	state.lastAccess = time.Now()
	generations := append([]*generation(nil), state.all...)
	state.mu.Unlock()

	for index := len(generations) - 1; index >= 0; index-- {
		path := generationSegmentPath(generations[index], segment)
		if fileExists(path) {
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
	return filepath.Join(generation.dir, "live.m3u8")
}

func generationSegmentPath(generation *generation, segment int) string {
	if generation == nil {
		return ""
	}
	return filepath.Join(generation.dir, fmt.Sprintf("seg_%05d.ts", segment))
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

func highestCompleteSegment(dir string) int {
	entries, err := os.ReadDir(dir)
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
