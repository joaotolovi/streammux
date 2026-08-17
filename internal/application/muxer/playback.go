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

type playbackState struct {
	mu sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc

	cacheDir string
	active   *generation
	all      []*generation
	// variants holds a generation per plan index, started on demand when the
	// player requests that ABR variant. active is the primary (plan 0) used by
	// the health monitor and recovery; variants are additional renditions.
	variants     map[int]*generation
	variantPlans map[int]int
	starting     bool
	startWait    chan struct{}
	startErr     error
	directURL    string

	placeholder        *generation
	errorGeneration    *generation
	retiredPlaceholder *generation
	placeholderStarted bool
	placeholderWait    chan struct{}
	filmDuration       float64
	filmSequence       int

	recovering   bool
	recoveryWait chan struct{}
	recoveryErr  error

	nextGeneration uint64
	nextPlan       int
	lastRequested  int
	maxRequested   int
	lastAccess     time.Time
	lastRecovery   time.Time
	closed         bool
}

type generation struct {
	id            uint64
	planIndex     int
	plan          model.PlaybackPlan
	prepared      *preparedPlan
	dir           string
	session       *ffmpeg.Session
	startSegment  int
	startedAt     time.Time
	isPlaceholder bool
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
		variants:      make(map[int]*generation),
		variantPlans:  make(map[int]int),
		lastRequested: -1,
		maxRequested:  -1,
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

	if m.placeholderPath != "" {
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

func placeholderFilmSequence(placeholder *generation) int {
	if placeholder == nil {
		return 0
	}
	videoLast := lastPlaylistSegment(generationVideoPlaylistPath(placeholder))
	audioLast := lastPlaylistSegment(generationAudioPlaylistPath(placeholder))
	if videoLast < 0 || audioLast < 0 {
		return 0
	}
	if audioLast < videoLast {
		videoLast = audioLast
	}
	return videoLast + 1
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

func (m *Muxer) runStartup(job *model.MuxJob, state *playbackState) {
	winner, err := m.coordinateAttempts(job, state, 0, 0, m.policy.StartupTimeout)
	if err != nil {
		direct := m.resolveDirectFallback(job, state)
		if direct == "" && m.errorPath != "" {
			if errorGeneration, errorErr := m.startErrorGeneration(state); errorErr == nil {
				state.mu.Lock()
				state.active = errorGeneration
				state.errorGeneration = errorGeneration
				state.starting = false
				state.startErr = err
				state.directURL = ""
				wait := state.startWait
				state.mu.Unlock()
				if wait != nil {
					close(wait)
				}
				log.Printf("mux: startup failed; serving error video: %v", err)
				return
			}
		}
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
	placeholder := state.placeholder
	state.mu.Unlock()
	filmSequence := placeholderFilmSequence(placeholder)
	state.mu.Lock()
	if state.closed {
		state.mu.Unlock()
		winner.session.Cancel()
		return
	}
	state.active = winner
	state.all = append(state.all, winner)
	state.nextPlan = winner.planIndex + 1
	state.starting = false
	state.startErr = nil
	state.directURL = ""
	state.lastRecovery = time.Now()
	state.filmDuration = winner.prepared.duration
	state.filmSequence = filmSequence
	state.lastRequested = -1
	state.maxRequested = filmSequence - 1
	state.retiredPlaceholder = placeholder
	state.placeholder = nil
	state.mu.Unlock()

	if placeholder != nil {
		placeholder.session.Cancel()
		select {
		case <-placeholder.session.Done():
		case <-time.After(2 * time.Second):
		}
	}

	state.mu.Lock()
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

// runPlaceholder starts the local intro/loop video session so the player gets
// immediate playback while the film is prepared in the background. It writes
// into a dedicated generation directory and closes placeholderWait once the
// placeholder master is ready.
func (m *Muxer) startErrorGeneration(state *playbackState) (*generation, error) {
	state.mu.Lock()
	state.nextGeneration++
	generationID := state.nextGeneration
	placeholder := state.placeholder
	state.placeholder = nil
	state.mu.Unlock()
	if placeholder != nil {
		placeholder.session.Cancel()
	}

	dir := filepath.Join(state.cacheDir, fmt.Sprintf("generation-%06d", generationID))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	session, err := m.ffmpeg.StartSinglePlaceholderSession(state.ctx, m.errorPath, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	generation := &generation{id: generationID, dir: dir, session: session, isPlaceholder: true}
	ticker := time.NewTicker(75 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(10 * time.Second)
	for {
		if fileExists(generationSegmentPath(generation, 0)) && fileExists(generationAudioSegmentPath(generation, 0)) && fileExists(generationPlaylistPath(generation)) {
			return generation, nil
		}
		select {
		case <-session.Done():
			_ = os.RemoveAll(dir)
			return nil, session.Err()
		case <-deadline:
			session.Cancel()
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("error video timed out before first segment")
		case <-ticker.C:
		}
	}
}

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

	session, err := m.ffmpeg.StartSinglePlaceholderSession(state.ctx, m.placeholderPath, dir)
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
	if state.active != nil {
		gen.session.Cancel()
		state.mu.Unlock()
		_ = os.RemoveAll(dir)
		if wait := state.placeholderWait; wait != nil {
			select {
			case <-wait:
			default:
				close(wait)
			}
		}
		return
	}
	state.placeholder = gen
	wait := state.placeholderWait
	state.mu.Unlock()
	if wait != nil {
		close(wait)
	}
	log.Printf("mux: placeholder playing (%s)", m.placeholderPath)
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

// EnsureVariant starts (or reuses) the generation for the given plan index,
// used by the ABR master playlist when the player requests a non-primary
// variant. It returns the generation's video playlist path once the first
// segment exists. The primary plan (0) is started by runStartup; variants are
// started lazily here.
func (m *Muxer) EnsureVariant(ctx context.Context, job *model.MuxJob, variantIndex int) (string, error) {
	if variantIndex < 0 {
		return "", fmt.Errorf("variant %d out of range", variantIndex)
	}
	state, err := m.stateFor(job)
	if err != nil {
		return "", err
	}

	// Resolve the variant index to the real plan index via the mapping
	// established by MasterPlaylist.
	state.mu.Lock()
	planIndex, ok := state.variantPlans[variantIndex]
	if !ok {
		// No mapping yet — EnsurePlaylist hasn't been called or the master
		// hasn't been rendered. Fall back to variantIndex == planIndex.
		planIndex = variantIndex
	}
	if planIndex < 0 || planIndex >= len(job.Plans) {
		state.mu.Unlock()
		return "", fmt.Errorf("variant %d maps to plan %d out of range", variantIndex, planIndex)
	}
	// Reuse the active generation if it is the plan we need.
	if state.active != nil && state.active.planIndex == planIndex {
		active := state.active
		state.mu.Unlock()
		return generationVideoPlaylistPath(active), nil
	}
	// Reuse an already-started variant generation.
	if gen, ok := state.variants[planIndex]; ok {
		path := generationVideoPlaylistPath(gen)
		state.mu.Unlock()
		if fileExists(path) {
			return path, nil
		}
	} else {
		state.mu.Unlock()
	}

	gen, err := m.startAttempt(ctx, job, state, planIndex, 0, false)
	if err != nil {
		return "", err
	}
	state.mu.Lock()
	state.variants[planIndex] = gen
	state.mu.Unlock()
	go m.monitorGeneration(job, state, gen)
	return generationVideoPlaylistPath(gen), nil
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

// MasterPlaylist returns the ABR master playlist advertising the top plans as
// variants. Each variant points to a per-plan media playlist that is generated
// on demand when the player requests it. The primary plan (0) is the default.
func (m *Muxer) MasterPlaylist(job *model.MuxJob) ([]byte, bool) {
	state := m.lookupState(job.ID)
	if state == nil {
		return nil, false
	}
	state.mu.Lock()
	state.lastAccess = time.Now()
	active := state.active
	activePlan := -1
	activeBitrate := 0.0
	if active != nil {
		activePlan = active.planIndex
		if active.prepared != nil {
			activeBitrate = active.prepared.videoBitrate
		}
	}
	ready := active != nil
	state.mu.Unlock()
	if !ready {
		return nil, false
	}

	// Build the list of plan indices to advertise, starting with the active
	// plan (always v0), then remaining plans in order, capped at 4.
	planIndices := []int{activePlan}
	for i := range job.Plans {
		if i == activePlan {
			continue
		}
		if len(planIndices) >= 4 {
			break
		}
		planIndices = append(planIndices, i)
	}

	// Record the variant→plan mapping so EnsureVariant can resolve correctly.
	state.mu.Lock()
	for vIdx, pIdx := range planIndices {
		state.variantPlans[vIdx] = pIdx
	}
	state.mu.Unlock()

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:6\n")
	for vIdx, pIdx := range planIndices {
		plan := job.Plans[pIdx]
		bw := int64(plan.EstimatedBandwidth())
		if pIdx == activePlan && activeBitrate > 0 {
			bw = int64(activeBitrate)
		}
		b.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%s\n", bw, plan.Video.Parsed.Resolution))
		b.WriteString(fmt.Sprintf("v%d/video/video.m3u8\n", vIdx))
	}
	return []byte(b.String()), true
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

func (m *Muxer) PlaceholderVideoPlaylist(job *model.MuxJob) ([]byte, bool) {
	return m.synchronizedPlaceholderPlaylist(job, true)
}

func (m *Muxer) PlaceholderAudioPlaylist(job *model.MuxJob) ([]byte, bool) {
	return m.synchronizedPlaceholderPlaylist(job, false)
}

func (m *Muxer) synchronizedPlaceholderPlaylist(job *model.MuxJob, video bool) ([]byte, bool) {
	state := m.lookupState(job.ID)
	if state == nil {
		return nil, false
	}
	state.mu.Lock()
	if state.active != nil || state.placeholder == nil {
		state.mu.Unlock()
		return nil, false
	}
	placeholder := state.placeholder
	state.lastAccess = time.Now()
	state.mu.Unlock()

	videoPlaylist, err := readLivePlaylist(generationVideoPlaylistPath(placeholder))
	if err != nil {
		return nil, false
	}
	audioPlaylist, err := readLivePlaylist(generationAudioPlaylistPath(placeholder))
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
	if first < 0 || last < first {
		return nil, false
	}
	playlist := videoPlaylist
	if !video {
		playlist = audioPlaylist
	}
	return playlist.render(first, last), true
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

func (m *Muxer) PaddedVideoPlaylist(job *model.MuxJob) ([]byte, bool) {
	state := m.lookupState(job.ID)
	if state == nil {
		return nil, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.lastAccess = time.Now()
	if state.active == nil || state.filmDuration <= 0 {
		return nil, false
	}
	return buildVodPlaylist(state.filmDuration, state.filmSequence)
}

func (m *Muxer) PaddedAudioPlaylist(job *model.MuxJob) ([]byte, bool) {
	state := m.lookupState(job.ID)
	if state == nil {
		return nil, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.lastAccess = time.Now()
	if state.active == nil || state.filmDuration <= 0 {
		return nil, false
	}
	return buildVodPlaylist(state.filmDuration, state.filmSequence)
}

func (m *Muxer) AudioSegmentPath(job *model.MuxJob, segment int) string {
	state := m.lookupState(job.ID)
	if state == nil {
		return ""
	}
	state.mu.Lock()
	state.lastAccess = time.Now()
	if state.active != nil {
		physical := segment - state.filmSequence
		active := state.active
		retired := state.retiredPlaceholder
		duration := state.filmDuration
		state.mu.Unlock()
		if physical < 0 {
			if retired != nil {
				if path := generationAudioSegmentPath(retired, segment); fileExists(path) {
					return path
				}
			}
			return ""
		}
		if physical >= vodSegmentCount(duration) {
			return ""
		}
		if path := generationAudioSegmentPath(active, physical); fileExists(path) {
			return path
		}
		return ""
	}
	placeholder := state.placeholder
	if placeholder != nil {
		path := generationAudioSegmentPath(placeholder, segment)
		if fileExists(path) {
			state.mu.Unlock()
			return path
		}
	}
	state.mu.Unlock()
	return ""
}

func (m *Muxer) SegmentPath(job *model.MuxJob, segment int) string {
	state := m.lookupState(job.ID)
	if state == nil {
		return ""
	}
	state.mu.Lock()
	state.lastAccess = time.Now()
	if state.active != nil {
		physical := segment - state.filmSequence
		active := state.active
		retired := state.retiredPlaceholder
		duration := state.filmDuration
		state.mu.Unlock()
		if physical < 0 {
			if retired != nil {
				if path := generationSegmentPath(retired, segment); fileExists(path) {
					return path
				}
			}
			return ""
		}
		if physical >= vodSegmentCount(duration) {
			return ""
		}
		if path := generationSegmentPath(active, physical); fileExists(path) {
			return path
		}
		return ""
	}
	placeholder := state.placeholder
	if placeholder != nil {
		path := generationSegmentPath(placeholder, segment)
		if fileExists(path) {
			state.mu.Unlock()
			return path
		}
	}
	state.mu.Unlock()
	return ""
}

// isForwardSeek reports whether a segment request is a real user seek rather
// than pre-buffering. Pre-buffering (ExoPlayer) requests segments
// incrementally, so the max requested grows by ~1 each time; a real seek jumps
// far ahead (e.g. 5 -> 600). We require a large jump beyond the previous max
// to avoid restarting the session on every buffered segment after handoff.
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
		physical := segment - state.filmSequence
		active := state.active
		placeholderActive := state.placeholder != nil && active == nil
		if !placeholderActive {
			state.lastRequested = physical
			if physical > state.maxRequested {
				state.maxRequested = physical
			}
		}
		recovering := state.recovering
		recoveryWait := state.recoveryWait
		nextPlan := state.nextPlan
		maxReq := state.maxRequested
		state.mu.Unlock()

		if placeholderActive {
			select {
			case <-deadlineCtx.Done():
				return "", fmt.Errorf("timeout waiting for placeholder segment %d: %w", segment, deadlineCtx.Err())
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		if physical < 0 {
			return "", fmt.Errorf("segment %d precedes film sequence", segment)
		}
		if active == nil {
			if !recovering {
				m.ensureRecovery(job, state, physical, nextPlan, "no active session")
			}
		} else {
			highest := highestCompleteSegment(active.dir)
			select {
			case <-active.session.Done():
				if !recovering {
					m.ensureRecovery(job, state, physical, nextPlan, "session ended")
				}
			default:
				if isForwardSeek(maxReq, physical, highest, active.startSegment) && !recovering {
					m.ensureRecovery(job, state, physical, active.planIndex, "forward seek")
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
		physical := segment - state.filmSequence
		active := state.active
		placeholderActive := state.placeholder != nil && active == nil
		if !placeholderActive {
			state.lastRequested = physical
			if physical > state.maxRequested {
				state.maxRequested = physical
			}
		}
		recovering := state.recovering
		recoveryWait := state.recoveryWait
		nextPlan := state.nextPlan
		state.mu.Unlock()

		if placeholderActive {
			select {
			case <-deadlineCtx.Done():
				return "", fmt.Errorf("timeout waiting for placeholder audio segment %d: %w", segment, deadlineCtx.Err())
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		if physical < 0 {
			return "", fmt.Errorf("audio segment %d precedes film sequence", segment)
		}
		if active == nil {
			if !recovering {
				m.ensureRecovery(job, state, physical, nextPlan, "no active session")
			}
		} else {
			select {
			case <-active.session.Done():
				if !recovering {
					m.ensureRecovery(job, state, physical, nextPlan, "session ended")
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

func buildVodPlaylist(filmDuration float64, sequence int) ([]byte, bool) {
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
	maxSeg := segs[0]
	for _, s := range segs[1:] {
		if s > maxSeg {
			maxSeg = s
		}
	}
	target := int(math.Ceil(maxSeg))
	if target < 1 {
		target = 1
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:6\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", target))
	b.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", sequence))
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	if sequence > 0 {
		b.WriteString("#EXT-X-DISCONTINUITY\n")
	}
	for i, d := range segs {
		b.WriteString(fmt.Sprintf("#EXTINF:%.6f,\n", d))
		b.WriteString(fmt.Sprintf("seg_%05d.ts\n", sequence+i))
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
