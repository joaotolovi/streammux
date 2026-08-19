package muxer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/streammux/streammux/internal/application/analyzer"
	"github.com/streammux/streammux/internal/application/ffmpeg"
	"github.com/streammux/streammux/internal/domain/model"
)

// ErrBeyondEnd reports a segment request past the end of the film.
var ErrBeyondEnd = errors.New("segment beyond end of film")

// posterFileName is the local filename of the cached poster image in a job's
// cache directory. It is downloaded from Cinemeta while sources are collected.
const posterFileName = "poster.jpg"

// imagePlaceholderWait is how long runPlaceholder waits for a poster download
// before falling back to the plain placeholder. It is short enough to keep
// playback instant but gives fast Cinemeta responses time to land.
const imagePlaceholderWait = 500 * time.Millisecond

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

	cacheDir   string
	posterPath string // absolute path to poster.jpg, set by stateFor from Process's poster dir

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

	// composer assembles source compositions dynamically; created lazily.
	composer *composer

	// ABR downgrade ladder: activeTier is the tier the player is currently
	// consuming (0 = primary), tier0Prepared retains the primary plan for
	// transcode strategies, tierBudgets are per-tier bitrate ceilings, and
	// tierDisc marks per-tier video discontinuities (strategy switches).
	activeTier    int
	tierBusy      bool
	tierWait      chan struct{}
	tierErr       error
	tier0Prepared *preparedPlan
	tierBudgets   [tierCount]int64
	tierDisc      [tierCount][]int

	duration      float64 // film duration in seconds (from probe)
	lastRequested int
	maxRequested  int
	lastAccess    time.Time
	lastRecovery  time.Time
	closed        bool

	deliveries []deliverySample

	// audioRenditions are lazy audio-only sessions. The main rendition is
	// always produced by the active video generation; alternates are created
	// only after the player requests them.
	audioRenditions map[string]*audioRendition
	activeAudioID   string
}

type audioRendition struct {
	id        string
	language  string
	title     string
	altTarget bool
	prepared  *preparedPlan
	dir       string
	session   *ffmpeg.Session
	start     int
	starting  bool
	wait      chan struct{}
}

type generation struct {
	id           uint64
	planIndex    int
	plan         model.PlaybackPlan
	prepared     *preparedPlan
	dir          string
	session      *ffmpeg.Session
	startSegment int // first public segment number this generation writes
	tier         int // ABR tier namespace this generation serves (0 = primary)
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

	// Preserve the poster path that Process set up (job.CacheDir was the
	// poster dir at that point) before we overwrite it with the playback
	// cache dir.
	posterPath := ""
	if job.CacheDir != "" {
		posterPath = filepath.Join(job.CacheDir, posterFileName)
	}

	ctx, cancel := context.WithCancel(context.Background())
	state := &playbackState{
		ctx:             ctx,
		cancel:          cancel,
		cacheDir:        dir,
		posterPath:      posterPath,
		lastRequested:   -1,
		maxRequested:    -1,
		lastAccess:      time.Now(),
		audioRenditions: make(map[string]*audioRendition),
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
	// Allow one retry per cooldown window. Reset the composer so the retry
	// re-evaluates every source from scratch — a source that failed on the
	// first attempt may work on the second (debrid CDN may have warmed up).
	if state.startErr != nil && time.Since(state.lastStart) > m.policy.RetryCooldown {
		state.starting = false
		state.startErr = nil
		if state.composer != nil {
			state.composer.reset()
		}
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

	// Prefer a composed placeholder with the film poster, but only when the
	// poster is already available or arrives within a very short window. We
	// never delay playback for the poster: Cinemeta is best-effort.
	posterPath := state.posterPath
	usePoster := posterPath != "" && fileExists(posterPath) && job.ContentType != "" && job.ContentID != ""
	if !usePoster && posterPath != "" && job.ContentType != "" && job.ContentID != "" {
		deadline := time.After(imagePlaceholderWait)
	waitLoop:
		for !fileExists(posterPath) {
			select {
			case <-state.ctx.Done():
				break waitLoop
			case <-deadline:
				break waitLoop
			default:
				time.Sleep(50 * time.Millisecond)
			}
		}
		usePoster = fileExists(posterPath)
	}

	var session *ffmpeg.Session
	var err error

	if usePoster {
		log.Printf("mux: starting image placeholder with poster %s", posterPath)
		session, err = m.ffmpeg.StartImagePlaceholderSession(state.ctx, m.placeholderPath, posterPath, dir, true)
		if err != nil {
			log.Printf("mux: image placeholder failed, falling back to plain placeholder: %v", err)
			_ = os.RemoveAll(dir)
			dir = filepath.Join(state.cacheDir, fmt.Sprintf("generation-%06d", generationID))
			usePoster = false
		}
	}

	if !usePoster {
		log.Printf("mux: starting plain placeholder")
		session, err = m.ffmpeg.StartSinglePlaceholderSession(state.ctx, m.placeholderPath, dir, true)
	}

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
		fail("failed to start any placeholder session: %v", err)
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

// runStartup walks the composer's source compositions until one launches.
// With a placeholder playing, the film is numbered from the placeholder's
// last common segment so the handoff is a single DISCONTINUITY on an
// otherwise static timeline.
func (m *Muxer) runStartup(job *model.MuxJob, state *playbackState) {
	deadline := time.Now().Add(m.policy.StartupTimeout)

	// If a placeholder is configured, wait for it to be playing before
	// preparing film sources — otherwise the film can be ready before the
	// placeholder even starts, and the user never sees the intro.
	if m.placeholderPath != "" {
		state.mu.Lock()
		phWait := state.placeholderWait
		state.mu.Unlock()
		if phWait != nil {
			select {
			case <-phWait:
			case <-state.ctx.Done():
			}
		}
	}

	comp := m.composerFor(job, state)

	var winner *generation
	var lastErr error
	for {
		state.mu.Lock()
		candidate := comp.acquire()
		state.mu.Unlock()
		if candidate == nil {
			lastErr = errors.New("all source combinations exhausted")
			break
		}

		prepCtx, prepCancel := context.WithTimeout(state.ctx, time.Until(deadline))
		prepared, cls, err := m.prepareComposition(prepCtx, job, candidate)
		prepCancel()
		if err != nil {
			lastErr = err
			log.Printf("mux: composition %d (video#%d audio#%d) failed: %v", candidate.ordinal, candidate.video.videoPos, candidate.audio.audioPos, err)
			state.mu.Lock()
			comp.fail(candidate, cls, err)
			state.mu.Unlock()
			if time.Now().After(deadline) {
				break
			}
			continue
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

		gen, err := m.launchGeneration(job, state, candidate.ordinal, prepared, 0, base, 0, m.policy.MinHandoffBuffer)
		if err == nil {
			winner = gen
			break
		}
		lastErr = err
		log.Printf("mux: composition %d launch failed: %v", candidate.ordinal, err)
		state.mu.Lock()
		comp.fail(candidate, failLaunch, err)
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
	state.activeTier = 0
	state.tier0Prepared = winner.prepared
	state.tierBudgets = tierTargets(streamBandwidth(winner.plan.Video))
	state.all = append(state.all, winner)
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
	log.Printf("mux: startup selected video#%d audio#%d %s (%s) at segment %d", winner.prepared.videoIdx, winner.prepared.audioIdx, winner.plan.Kind, winner.plan.Video.Parsed.Resolution, base)
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
	log.Printf("mux: startup failed (retry after cooldown resumes from remaining sources): %v", cause)
}

// startErrorGeneration launches the local error video. The error video is
// short VOD content, so it always numbers from segment 0 — start_number > its
// content length would produce no segments at all. The placeholder (if any)
// is retired and the public timeline resets with a DISCONTINUITY.
func (m *Muxer) startErrorGeneration(state *playbackState, atSeg int) *generation {
	if m.errorPath == "" {
		return nil
	}

	state.mu.Lock()
	state.nextGeneration++
	generationID := state.nextGeneration
	ph := state.placeholder
	state.mu.Unlock()

	if ph != nil {
		ph.session.Cancel()
		state.mu.Lock()
		state.placeholder = nil
		state.retiredPlaceholder = ph
		state.mu.Unlock()
	}
	start := 0
	_ = atSeg // accepted for API symmetry; the error video resets the timeline

	var gen *generation
	for attempt := 0; attempt < 2; attempt++ {
		dir := filepath.Join(state.cacheDir, fmt.Sprintf("generation-%06d", generationID))
		session, err := m.ffmpeg.StartSinglePlaceholderSession(state.ctx, m.errorPath, dir, false)
		if err != nil {
			log.Printf("mux: error video start (attempt %d): %v", attempt+1, err)
			_ = os.RemoveAll(dir)
			state.mu.Lock()
			state.nextGeneration++
			generationID = state.nextGeneration
			state.mu.Unlock()
			continue
		}
		candidate := &generation{
			id:           generationID,
			dir:          dir,
			session:      session,
			startSegment: start,
			startedAt:    time.Now(),
			isLocal:      true,
			isError:      true,
		}

		ticker := time.NewTicker(75 * time.Millisecond)
		deadline := time.After(20 * time.Second)
		produced := false
	waitLoop:
		for {
			if fileExists(generationSegmentPath(candidate, start)) && fileExists(generationAudioSegmentPath(candidate, start)) {
				produced = true
				break waitLoop
			}
			select {
			case <-session.Done():
				log.Printf("mux: error video ended before first segment (attempt %d): %v; stderr tail: %s", attempt+1, session.Err(), session.StderrTail())
				break waitLoop
			case <-deadline:
				session.Cancel()
				log.Printf("mux: error video timed out before first segment (attempt %d, start %d, dir %s); stderr tail: %s", attempt+1, start, dir, session.StderrTail())
				break waitLoop
			case <-ticker.C:
			}
		}
		ticker.Stop()
		if produced {
			gen = candidate
			break
		}
		_ = os.RemoveAll(dir)
		state.mu.Lock()
		state.nextGeneration++
		generationID = state.nextGeneration
		state.mu.Unlock()
	}
	if gen == nil {
		return nil
	}

	state.mu.Lock()
	state.all = append(state.all, gen)
	state.errorStart = start
	state.mu.Unlock()
	return gen
}

// launchGeneration starts the ffmpeg session for a prepared plan and waits
// for its first segment. startNumber is the first public segment written;
// startTime is the content offset in seconds (0 for a fresh start). tier is
// the ABR namespace the generation serves; minBuffer is how much content must
// be ready before returning (the initial handoff wants a deep cushion, lazy
// tier switches and seeks want speed).
func (m *Muxer) launchGeneration(job *model.MuxJob, state *playbackState, planIndex int, prepared *preparedPlan, tier, startNumber int, startTime float64, minBuffer time.Duration) (*generation, error) {
	// Seeks do a remote input seek on a large file (MKV cues, byte-range
	// request to the debrid), which can take much longer than a fresh start
	// before the first segment appears.
	attemptBudget := m.policy.AttemptTimeout
	if startTime > 0 {
		attemptBudget = m.policy.StartupTimeout
	}
	attemptCtx, cancel := context.WithTimeout(state.ctx, attemptBudget)
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
		Transcode:       prepared.transcode,
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
		tier:         tier,
		startedAt:    time.Now(),
	}

	segmentPath := generationSegmentPath(generation, startNumber)
	audioSegmentPath := generationAudioSegmentPath(generation, startNumber)
	ticker := time.NewTicker(75 * time.Millisecond)
	defer ticker.Stop()

	for {
		if fileExists(segmentPath) && fileExists(audioSegmentPath) {
			break
		}
		select {
		case <-attemptCtx.Done():
			session.Cancel()
			go cleanupFailedGeneration(generation)
			return nil, fmt.Errorf("first segment deadline: %w", attemptCtx.Err())
		case <-session.Done():
			if fileExists(segmentPath) && fileExists(audioSegmentPath) {
				break
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

	// Wait for a minimum buffer before handing off, so the first bandwidth
	// dip doesn't immediately stall playback.
	segDur := ffmpeg.SegDuration()
	if segDur <= 0 {
		segDur = 4.0
	}
	if minBuffer < 0 {
		minBuffer = 0
	}
	minSegs := int(math.Ceil(minBuffer.Seconds() / segDur))
	for produced := 1; produced < minSegs; {
		highest := highestCompleteSegment(generation.dir)
		if highest >= 0 && highest-startNumber+1 >= minSegs {
			break
		}
		select {
		case <-attemptCtx.Done():
			// Timeout while buffering: proceed with what we have rather than
			// killing the session — some content is better than none.
			return generation, nil
		case <-session.Done():
			// Session ended while buffering: proceed with what we have.
			return generation, nil
		case <-ticker.C:
		}
	}

	return generation, nil
}

// coordinateRecovery walks the composer until one composition launches.
// prefer (when set) reuses the previous generation's prepared sources first —
// a seek keeps the same sources, only the offset changes.
func (m *Muxer) coordinateRecovery(job *model.MuxJob, state *playbackState, prefer *generation, startSegment int, timeout time.Duration) (*generation, error) {
	deadline := time.Now().Add(timeout)

	state.mu.Lock()
	tier := state.activeTier
	budget := int64(0)
	if tier > 0 && tier < tierCount {
		budget = state.tierBudgets[tier]
	}
	state.mu.Unlock()

	if prefer != nil && prefer.prepared != nil {
		state.mu.Lock()
		base := state.filmBase
		state.mu.Unlock()
		startTime := float64(startSegment-base) * ffmpeg.SegDuration()
		if startTime < 0 {
			startTime = 0
		}
		if gen, err := m.launchGeneration(job, state, prefer.planIndex, prefer.prepared, prefer.tier, startSegment, startTime, m.policy.MinHandoffBuffer); err == nil {
			return gen, nil
		} else {
			log.Printf("mux: preferred sources failed to relaunch: %v", err)
			// A deadline is usually the slow remote input seek, not a dead
			// source: keep the source available for other pairings.
			if !errors.Is(err, context.DeadlineExceeded) {
				m.markComposerFailed(state, prefer.prepared.plan.Video.SourceKey())
			}
		}
	}

	comp := m.composerFor(job, state)
	var failures []error
	for {
		if time.Now().After(deadline) {
			break
		}
		state.mu.Lock()
		candidate := comp.acquireWithin(budget)
		state.mu.Unlock()
		if candidate == nil {
			break
		}

		prepCtx, prepCancel := context.WithTimeout(state.ctx, time.Until(deadline))
		prepared, cls, err := m.prepareComposition(prepCtx, job, candidate)
		prepCancel()
		if err != nil {
			failures = append(failures, fmt.Errorf("video#%d/audio#%d: %w", candidate.video.videoPos, candidate.audio.audioPos, err))
			log.Printf("mux: composition %d (video#%d audio#%d) failed: %v", candidate.ordinal, candidate.video.videoPos, candidate.audio.audioPos, err)
			state.mu.Lock()
			comp.fail(candidate, cls, err)
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
		gen, err := m.launchGeneration(job, state, candidate.ordinal, prepared, tier, startSegment, startTime, m.policy.MinHandoffBuffer)
		if err == nil {
			return gen, nil
		}
		failures = append(failures, fmt.Errorf("video#%d/audio#%d launch: %w", candidate.video.videoPos, candidate.audio.audioPos, err))
		log.Printf("mux: composition %d launch failed: %v", candidate.ordinal, err)
		state.mu.Lock()
		comp.fail(candidate, failLaunch, err)
		state.mu.Unlock()
	}
	if len(failures) == 0 {
		return nil, fmt.Errorf("no playback sources remain")
	}
	return nil, errors.Join(failures...)
}

// markComposerFailed flags a source as unusable in the composer ledger.
func (m *Muxer) markComposerFailed(state *playbackState, sourceKey string) {
	if sourceKey == "" {
		return
	}
	state.mu.Lock()
	if state.composer != nil {
		state.composer.markFailed(sourceKey)
	}
	state.mu.Unlock()
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
// every film plays silently. When ABR tiers are available (the film is live and
// the ladder was computed at Process time), the master advertises one variant
// per tier so the player can switch down automatically on network dips and the
// user can switch manually on decode issues.
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
	tiers := len(job.TierMetas)
	state.mu.Unlock()

	if active != nil && !active.isError && active.prepared != nil && duration > 0 {
		if tiers > 1 {
			// Override tier 0 with the REAL probed bitrate/dimensions of
			// the active generation — the estimate from Process time is
			// only a placeholder until ffprobe measures the source.
			metas := make([]model.TierMeta, len(job.TierMetas))
			copy(metas, job.TierMetas)
			bitrate := active.prepared.videoBitrate
			if bitrate <= 0 {
				bitrate = float64(active.plan.EstimatedBandwidth())
			}
			bw := int64(bitrate * 1.2)
			if bw <= 0 {
				bw = 8_000_000
			}
			metas[0] = model.TierMeta{
				Bandwidth: bw,
				Width:     active.prepared.videoWidth,
				Height:    active.prepared.videoHeight,
			}
			return m.renderMasterABR(job, metas, job.TargetLanguage), true
		}
		bitrate := active.prepared.videoBitrate
		if bitrate <= 0 {
			bitrate = float64(active.plan.EstimatedBandwidth())
		}
		bandwidth := int64(bitrate * 1.2)
		if bandwidth <= 0 {
			bandwidth = 8_000_000
		}
		return m.renderMaster(job, bandwidth, active.prepared.videoWidth, active.prepared.videoHeight, job.TargetLanguage), true
	}
	if placeholder != nil || (active != nil && active.isError) {
		// Small placeholder/error rendition.
		return m.renderMaster(job, 3_000_000, 0, 0, job.TargetLanguage), true
	}
	return nil, false
}

type audioRenditionMeta struct {
	id       string
	code     string
	name     string
	uri      string
	defaultY bool
	auto     bool
}

// audioRenditionMedia advertises the requested language first, its alternative
// second, then languages inferred from addon metadata. These are optimistic
// capabilities: only the selected rendition is resolved and validated.
func audioRenditionMedia(job *model.MuxJob, targetLanguage string) []string {
	seen := map[string]bool{}
	metas := make([]audioRenditionMeta, 0, 4)
	targetCode := ffmpeg.LanguageCode(targetLanguage)
	if targetCode == "" {
		targetCode = "eng"
	}
	targetName := targetLanguage
	if targetName == "" {
		targetName = "Audio"
	}
	metas = append(metas,
		audioRenditionMeta{id: "main", code: targetCode, name: targetName, uri: "audio/audio.m3u8", defaultY: true, auto: true},
		audioRenditionMeta{id: targetCode + "-alt", code: targetCode, name: targetName + " (alternativa)", uri: "audio/" + targetCode + "-alt/audio.m3u8"},
	)
	seen[targetCode] = true
	if job != nil {
		for _, plan := range job.Plans {
			for _, stream := range []model.CollectedStream{plan.Audio, plan.Video} {
				for _, raw := range append(append([]string{}, stream.Parsed.Languages...), stream.AddonLanguage, stream.Language) {
					code, name := knownAudioLanguage(raw)
					if code == "" || seen[code] || !audioLanguageAllowed(job, code, targetCode) {
						continue
					}
					seen[code] = true
					metas = append(metas, audioRenditionMeta{id: code, code: code, name: name, uri: "audio/" + code + "/audio.m3u8", auto: true})
				}
			}
		}
	}
	lines := make([]string, 0, len(metas))
	for _, meta := range metas {
		auto := "NO"
		if meta.auto {
			auto = "YES"
		}
		defaultValue := "NO"
		if meta.defaultY {
			defaultValue = "YES"
		}
		line := fmt.Sprintf("#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=%q,DEFAULT=%s,AUTOSELECT=%s,LANGUAGE=\"%s\",URI=\"%s\"\n", meta.name, defaultValue, auto, meta.code, meta.uri)
		lines = append(lines, line)
	}
	return lines
}

func audioLanguageAllowed(job *model.MuxJob, code, targetCode string) bool {
	if code == targetCode {
		return true
	}
	if job == nil {
		return false
	}
	for _, addon := range job.Config.Addons {
		if !addon.Enabled {
			continue
		}
		if addon.ShowAllAudioLanguages {
			return true
		}
		for _, raw := range addon.AudioLanguages {
			if allowed, _ := knownAudioLanguage(raw); allowed == code {
				return true
			}
		}
	}
	return false
}

func knownAudioLanguage(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "Dual Audio" || raw == "Multi" || raw == "Dubbed" || raw == "Latino" {
		return "", ""
	}
	code := ffmpeg.LanguageCode(raw)
	switch code {
	case "por":
		return code, "Português"
	case "eng":
		return code, "English"
	case "spa":
		return code, "Español"
	case "fra":
		return code, "Français"
	case "deu":
		return code, "Deutsch"
	case "ita":
		return code, "Italiano"
	case "jpn":
		return code, "日本語"
	case "kor":
		return code, "한국어"
	case "hin":
		return code, "हिन्दी"
	case "rus":
		return code, "Русский"
	case "ara":
		return code, "العربية"
	case "zho":
		return code, "中文"
	default:
		return "", ""
	}
}

// renderMasterABR renders the master with one #EXT-X-STREAM-INF per tier. The
// audio rendition group is shared: the audio playlist stays the same across
// tiers (the dub is preserved). Each variant points at video/v{tier}.m3u8 so
// the HTTP layer can route tier-specific playlist and segment requests.
func (m *Muxer) renderMasterABR(job *model.MuxJob, metas []model.TierMeta, targetLanguage string) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:6\n")
	for _, media := range audioRenditionMedia(job, targetLanguage) {
		b.WriteString(media)
	}

	for i, meta := range metas {
		bandwidth := meta.Bandwidth
		if bandwidth <= 0 {
			bandwidth = 8_000_000
		}
		streamInf := fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d", bandwidth)
		if meta.Width > 0 && meta.Height > 0 {
			streamInf += fmt.Sprintf(",RESOLUTION=%dx%d", meta.Width, meta.Height)
		}
		streamInf += ",AUDIO=\"aud\"\n"
		b.WriteString(streamInf)
		if i == 0 {
			b.WriteString("video/video.m3u8\n")
		} else {
			b.WriteString(fmt.Sprintf("video/v%d.m3u8\n", i))
		}
	}
	return []byte(b.String())
}

func (m *Muxer) renderMaster(job *model.MuxJob, bandwidth int64, width, height int, targetLanguage string) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:6\n")
	for _, media := range audioRenditionMedia(job, targetLanguage) {
		b.WriteString(media)
	}

	streamInf := "#EXT-X-STREAM-INF:BANDWIDTH=" + fmt.Sprint(bandwidth)
	if width > 0 && height > 0 {
		streamInf += fmt.Sprintf(",RESOLUTION=%dx%d", width, height)
	}
	streamInf += ",AUDIO=\"aud\"\n"
	b.WriteString(streamInf)
	b.WriteString("video/video.m3u8\n")
	return []byte(b.String())
}

// VideoPlaylist renders the video media playlist for the requested ABR tier.
// tier 0 (the default, served at video/video.m3u8) uses the shared timeline;
// higher tiers are served at video/v{tier}.m3u8 and trigger a lazy downgrade-
// ladder spin-up on first request. Audio is shared across tiers.
func (m *Muxer) VideoPlaylist(job *model.MuxJob, tier int) ([]byte, bool) {
	return m.renderMediaPlaylist(job, tier)
}

func audioRenditionExists(job *model.MuxJob, id string) bool {
	if job == nil {
		return false
	}
	for _, line := range audioRenditionMedia(job, job.TargetLanguage) {
		if strings.Contains(line, `URI="audio/`+id+`/audio.m3u8"`) {
			return true
		}
	}
	return false
}

func audioRenditionLanguage(job *model.MuxJob, id string) string {
	if id == "" || id == "main" || strings.HasSuffix(id, "-alt") {
		return job.TargetLanguage
	}
	return languageNameForCode(id)
}

func languageNameForCode(code string) string {
	if _, name := knownAudioLanguage(code); name != "" {
		return name
	}
	return code
}

// prepareAudioRendition reuses the active video and ranks only audio sources
// for the requested language. It deliberately probes candidates in order, so
// addon metadata only advertises a possibility; ffprobe remains authoritative.
func (m *Muxer) prepareAudioRendition(ctx context.Context, job *model.MuxJob, state *playbackState, id string) (*preparedPlan, error) {
	state.mu.Lock()
	active := state.active
	filmBase := state.filmBase
	state.mu.Unlock()
	if active == nil || active.prepared == nil || active.isError {
		return nil, fmt.Errorf("no active video for audio rendition")
	}
	language := audioRenditionLanguage(job, id)
	if language == "" {
		return nil, fmt.Errorf("unknown audio rendition %q", id)
	}
	skipKey := ""
	if strings.HasSuffix(id, "-alt") {
		skipKey = active.prepared.plan.Audio.SourceKey()
		if skipKey == "" {
			skipKey = active.prepared.plan.Video.SourceKey()
		}
	}

	candidates := make([]model.CollectedStream, 0, len(job.Plans)*2+1)
	seen := map[string]bool{}
	activeVideoKey := active.prepared.plan.Video.SourceKey()
	activeAudioKey := active.prepared.plan.Audio.SourceKey()
	appendCandidate := func(stream model.CollectedStream, force bool) {
		key := stream.SourceKey()
		if key == "" || seen[key] || (!force && !analyzer.MatchesLanguage(stream, language)) {
			return
		}
		seen[key] = true
		candidates = append(candidates, stream)
	}
	forceActive := language == job.TargetLanguage
	appendCandidate(active.prepared.plan.Video, forceActive)
	appendCandidate(active.prepared.plan.Audio, forceActive)
	for _, plan := range job.Plans {
		appendCandidate(plan.Audio, false)
		appendCandidate(plan.Video, false)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		rank := func(s model.CollectedStream) int {
			if s.SourceKey() == activeVideoKey {
				return 0
			}
			if s.SourceKey() == activeAudioKey {
				return 1
			}
			return 2
		}
		ri, rj := rank(candidates[i]), rank(candidates[j])
		if ri != rj {
			return ri < rj
		}
		return analyzer.AudioScore(candidates[i]) > analyzer.AudioScore(candidates[j])
	})

	for _, candidate := range candidates {
		if skipKey != "" && candidate.SourceKey() == skipKey {
			continue
		}
		prepared, err := m.prepareAudioCandidate(ctx, job, active, candidate, language, filmBase, false)
		if err == nil {
			return prepared, nil
		}
		log.Printf("mux: audio rendition %s candidate %s rejected: %v", id, candidate.SourceKey(), err)
	}
	// Undefined tracks are a last resort, consistent with the primary composer.
	for _, candidate := range candidates {
		if skipKey != "" && candidate.SourceKey() == skipKey {
			continue
		}
		if prepared, err := m.prepareAudioCandidate(ctx, job, active, candidate, language, filmBase, true); err == nil {
			return prepared, nil
		}
	}
	return nil, fmt.Errorf("no verified %s audio source", language)
}

func (m *Muxer) prepareAudioCandidate(ctx context.Context, job *model.MuxJob, active *generation, candidate model.CollectedStream, language string, filmBase int, lenient bool) (*preparedPlan, error) {
	video := active.prepared.plan.Video
	audioURL := ""
	var audioProbe *ffmpeg.ProbeResult
	if candidate.SourceKey() == active.prepared.plan.Video.SourceKey() {
		audioURL = active.prepared.videoURL
		audioProbe = &ffmpeg.ProbeResult{Duration: active.prepared.duration, AudioTracks: active.prepared.videoAudioTracks}
	} else if candidate.SourceKey() == active.prepared.plan.Audio.SourceKey() && active.prepared.audioURL != "" {
		audioURL = active.prepared.audioURL
		audioProbe, _ = m.probeSource(ctx, audioURL)
	} else {
		var err error
		audioURL, err = m.resolveSource(ctx, job, candidate)
		if err != nil {
			return nil, err
		}
		audioProbe, err = m.probeSource(ctx, audioURL)
		if err != nil {
			return nil, err
		}
	}
	if audioProbe == nil {
		return nil, fmt.Errorf("audio probe unavailable")
	}
	track := targetAudioTrackStrict(audioProbe.AudioTracks, language, candidate, lenient)
	if track < 0 {
		return nil, fmt.Errorf("no confirmed %s track", language)
	}
	if active.prepared.duration <= 0 || audioProbe.Duration <= 0 {
		return nil, fmt.Errorf("unknown duration")
	}
	tolerance := active.prepared.duration * m.policy.DurationTolerance
	if tolerance < 1 {
		tolerance = 1
	}
	if tolerance > 15 {
		tolerance = 15
	}
	if delta := math.Abs(active.prepared.duration - audioProbe.Duration); delta > tolerance {
		return nil, fmt.Errorf("duration mismatch: %.3fs exceeds %.3fs tolerance", delta, tolerance)
	}
	plan := model.PlaybackPlan{Kind: model.PlanDualSource, Video: video, Audio: candidate, HasTargetAudio: true}
	if candidate.SourceKey() == video.SourceKey() {
		plan.Kind = model.PlanSingleSource
	}
	prepared := &preparedPlan{
		plan: plan, videoURL: active.prepared.videoURL, audioURL: audioURL,
		videoTrackIndex: active.prepared.videoTrackIndex, audioTrackIndex: track,
		audioMode: compatibleAudioMode(audioTrackCodec(audioProbe.AudioTracks, track)),
		duration:  active.prepared.duration, videoBitrate: active.prepared.videoBitrate,
		videoWidth: active.prepared.videoWidth, videoHeight: active.prepared.videoHeight,
		videoAudioTracks: active.prepared.videoAudioTracks,
	}
	if candidate.SourceKey() != video.SourceKey() && len(prepared.videoAudioTracks) > 0 {
		if offset, ok := m.audioOffsetFor(plan); ok {
			_ = offset
		} else {
			lag, _, confidence, err := m.detectOffset(prepared)
			if err == nil && confidence >= ffmpeg.SyncMinConfidence {
				m.cacheAudioOffset(plan, -lag)
			}
		}
	}
	return prepared, nil
}

func audioTrackCodec(tracks []ffmpeg.AudioTrack, index int) string {
	for _, track := range tracks {
		if track.Index == index {
			return track.Codec
		}
	}
	return ""
}

func audioOffsetForPrepared(m *Muxer, prepared *preparedPlan) time.Duration {
	if prepared == nil {
		return 0
	}
	offset, _ := m.audioOffsetFor(prepared.plan)
	return offset
}

// AudioPlaylist renders the audio media playlist for the current phase.
func (m *Muxer) AudioPlaylist(job *model.MuxJob) ([]byte, bool) {
	m.activateAudioRendition(job, "")
	return m.renderMediaPlaylist(job, 0)
}

// AudioPlaylistRendition renders the shared public timeline for an audio
// rendition. The actual alternate audio session starts only when its segment
// is requested.
func (m *Muxer) AudioPlaylistRendition(job *model.MuxJob, id string) ([]byte, bool) {
	if id == "" || id == "main" {
		return m.AudioPlaylist(job)
	}
	if !audioRenditionExists(job, id) {
		return m.AudioPlaylist(job)
	}
	m.activateAudioRendition(job, id)
	return m.renderMediaPlaylist(job, 0)
}

func (m *Muxer) activateAudioRendition(job *model.MuxJob, id string) {
	state := m.lookupState(job.ID)
	if state == nil {
		return
	}
	state.mu.Lock()
	if state.activeAudioID == id {
		state.mu.Unlock()
		return
	}
	old := state.audioRenditions[state.activeAudioID]
	state.activeAudioID = id
	state.mu.Unlock()
	if old != nil && old.session != nil {
		old.session.Cancel()
	}
}

func (m *Muxer) renderMediaPlaylist(job *model.MuxJob, tier int) ([]byte, bool) {
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
	if tier > 0 && tier < tierCount {
		disc = append(disc, state.tierDisc[tier]...)
	}
	errGen := state.errorGeneration
	errStart := state.errorStart
	activeTier := state.activeTier
	tierBusy := state.tierBusy
	lastRequested := state.lastRequested
	state.mu.Unlock()

	// A tier > 0 that is not yet active: ensure the downgrade ladder spins up
	// at the player's position. The playlist this call returns is the same
	// shared VOD timeline (the ladder's first segment carries a
	// DISCONTINUITY); the player does not see the tier namespace.
	if tier > 0 && tier != activeTier && !tierBusy && lastRequested >= 0 {
		m.ensureTier(job, state, tier, lastRequested+1)
	}

	_ = retired
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
// encoder has not reached. maxRequested < 0 (no requests since the film
// started) counts as unknown, not as zero.
const seekJumpThreshold = 20

func isForwardSeek(maxRequested, physical, highest, startSegment int) bool {
	if physical <= highest+8 {
		return false
	}
	if maxRequested < 0 {
		return highest >= startSegment && physical > startSegment+seekJumpThreshold
	}
	return physical > maxRequested+seekJumpThreshold
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
// segment for the given ABR tier. tier 0 is the primary; tiers > 0 trigger a
// lazy downgrade-ladder spin-up on first request. Backward requests hit the
// on-disk cache; forward requests beyond what the encoder produced restart
// the session at that offset.
func (m *Muxer) EnsureSegment(ctx context.Context, job *model.MuxJob, segment, tier int) (string, error) {
	return m.ensureMediaSegment(ctx, job, segment, tier, false)
}

// EnsureAudioSegment is EnsureSegment for the audio rendition (tier-agnostic:
// audio is shared across tiers).
func (m *Muxer) EnsureAudioSegment(ctx context.Context, job *model.MuxJob, segment int) (string, error) {
	if state := m.lookupState(job.ID); state != nil {
		state.mu.Lock()
		id := state.activeAudioID
		if id != "" {
			state.activeAudioID = ""
		}
		old := state.audioRenditions[id]
		state.mu.Unlock()
		if old != nil && old.session != nil {
			old.session.Cancel()
		}
	}
	return m.ensureMediaSegment(ctx, job, segment, 0, true)
}

// AudioSegmentPathRendition resolves an alternate rendition only while it is
// active. A stale request after a switch falls back to the already validated
// primary audio instead of reviving the old session.
func (m *Muxer) AudioSegmentPathRendition(job *model.MuxJob, id string, segment int) string {
	if id == "" || id == "main" {
		return m.AudioSegmentPath(job, segment)
	}
	state := m.lookupState(job.ID)
	if state == nil {
		return ""
	}
	state.mu.Lock()
	active := state.activeAudioID == id
	r := state.audioRenditions[id]
	state.mu.Unlock()
	if !active || r == nil {
		return ""
	}
	return audioRenditionSegmentPath(r, segment)
}

// EnsureAudioSegmentRendition lazily starts one audio-only session. Failed or
// unverified alternatives fall back to the primary rendition.
func (m *Muxer) EnsureAudioSegmentRendition(ctx context.Context, job *model.MuxJob, id string, segment int) (string, error) {
	if id == "" || id == "main" {
		return m.EnsureAudioSegment(ctx, job, segment)
	}
	if !audioRenditionExists(job, id) {
		return m.EnsureAudioSegment(ctx, job, segment)
	}
	state := m.lookupState(job.ID)
	if state == nil {
		return "", fmt.Errorf("playback state not found")
	}
	state.mu.Lock()
	if state.activeAudioID != id {
		state.mu.Unlock()
		return m.EnsureAudioSegment(ctx, job, segment)
	}
	state.mu.Unlock()
	for {
		if path := m.AudioSegmentPathRendition(job, id, segment); path != "" {
			return path, nil
		}
		state.mu.Lock()
		r := state.audioRenditions[id]
		if r == nil {
			r = &audioRendition{id: id}
			state.audioRenditions[id] = r
		}
		if state.activeAudioID != id {
			state.mu.Unlock()
			return m.EnsureAudioSegment(ctx, job, segment)
		}
		if r.starting {
			wait := r.wait
			state.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-wait:
			}
			continue
		}
		if r.session != nil {
			session := r.session
			state.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-session.Done():
				state.mu.Lock()
				if r.session == session {
					r.session = nil
				}
				state.mu.Unlock()
				continue
			case <-time.After(75 * time.Millisecond):
				continue
			}
		}
		r.starting = true
		r.wait = make(chan struct{})
		prepared := r.prepared
		state.mu.Unlock()

		if prepared == nil {
			var err error
			prepared, err = m.prepareAudioRendition(ctx, job, state, id)
			if err != nil {
				state.mu.Lock()
				r.starting = false
				close(r.wait)
				state.mu.Unlock()
				return m.EnsureAudioSegment(ctx, job, segment)
			}
		}
		starter, ok := m.ffmpeg.(interface {
			StartAudioSession(context.Context, ffmpeg.AudioSessionSpec) (*ffmpeg.Session, error)
		})
		if !ok {
			state.mu.Lock()
			r.starting = false
			close(r.wait)
			state.mu.Unlock()
			return m.EnsureAudioSegment(ctx, job, segment)
		}
		dir := filepath.Join(state.cacheDir, "audio-renditions", id)
		if err := os.MkdirAll(filepath.Join(dir, "audio"), 0755); err != nil {
			return m.EnsureAudioSegment(ctx, job, segment)
		}
		session, err := starter.StartAudioSession(state.ctx, ffmpeg.AudioSessionSpec{
			AudioURL: prepared.audioURL, AudioTrackIndex: prepared.audioTrackIndex,
			StartSegment: segment, StartTime: float64(segment-state.filmBase) * ffmpeg.SegDuration(),
			OutputDir: dir, AudioMode: prepared.audioMode, AudioLanguage: id,
			AudioTitle: id, UserAgent: browserUA, AudioOffset: audioOffsetForPrepared(m, prepared),
		})
		state.mu.Lock()
		r.starting = false
		if err == nil {
			r.prepared, r.dir, r.session, r.start = prepared, dir, session, segment
		}
		close(r.wait)
		state.mu.Unlock()
		if err != nil {
			return m.EnsureAudioSegment(ctx, job, segment)
		}
		deadline := time.NewTimer(m.policy.SegmentTimeout)
		defer deadline.Stop()
		for {
			if path := audioRenditionSegmentPath(r, segment); path != "" {
				return path, nil
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-deadline.C:
				return m.EnsureAudioSegment(ctx, job, segment)
			case <-session.Done():
				return m.EnsureAudioSegment(ctx, job, segment)
			case <-time.After(75 * time.Millisecond):
			}
		}
	}
}

func audioRenditionSegmentPath(r *audioRendition, segment int) string {
	if r == nil || r.dir == "" {
		return ""
	}
	path := filepath.Join(r.dir, "audio", fmt.Sprintf("seg_%05d.ts", segment))
	if fileExists(path) {
		return path
	}
	return ""
}

func (m *Muxer) ensureMediaSegment(ctx context.Context, job *model.MuxJob, segment, tier int, audio bool) (string, error) {
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

	// A tier > 0 not yet active: spin up the downgrade ladder at the
	// player's position before falling through to the normal wait/seek
	// logic. The ladder's first segment carries a DISCONTINUITY.
	if !audio && tier > 0 {
		state.mu.Lock()
		busy := state.tierBusy
		activeTier := state.activeTier
		lastRequested := state.lastRequested
		state.mu.Unlock()
		if !busy && activeTier != tier {
			at := lastRequested + 1
			if at < 0 {
				at = segment
			}
			m.ensureTier(job, state, tier, at)
		}
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
		// Capture the max BEFORE including this request: a real seek jumps
		// far beyond it, and comparing against a max that already contains
		// the request itself never detects anything.
		prevMax := state.maxRequested
		if !placeholderActive {
			state.lastRequested = segment
			if segment > state.maxRequested {
				state.maxRequested = segment
			}
		}
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
				m.ensureRecovery(job, state, segment, "no active session")
			}
		} else if !active.isError {
			highest := highestCompleteSegment(active.dir)
			select {
			case <-active.session.Done():
				if !recovering {
					m.ensureRecovery(job, state, segment, "session ended")
				}
			default:
				if (segment < active.startSegment || isForwardSeek(prevMax, segment, highest, active.startSegment)) && !recovering {
					m.fastSeek(job, state, active, segment)
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

// fastSeek relaunches the SAME sources at the requested offset — no composer
// walk, no revalidation, no sync re-estimation. The CDN URL supports range
// requests, so a fresh ffmpeg with -ss starts producing at the target in
// seconds. Falls back to a full recovery if the relaunch fails. The new
// generation is registered as active immediately (after its first segment),
// so pending and future requests for the target segment find it.
func (m *Muxer) fastSeek(job *model.MuxJob, state *playbackState, old *generation, targetSegment int) {
	state.mu.Lock()
	if state.recovering || state.closed || state.active != old {
		state.mu.Unlock()
		return
	}
	state.recovering = true
	state.recoveryWait = make(chan struct{})
	state.recoveryErr = nil
	// Hold requests at the target; the seeker session is coming.
	state.lastRequested = targetSegment
	if targetSegment > state.maxRequested {
		state.maxRequested = targetSegment
	}
	state.mu.Unlock()

	go func() {
		base := 0
		state.mu.Lock()
		base = state.filmBase
		state.mu.Unlock()
		startTime := float64(targetSegment-base) * ffmpeg.SegDuration()
		if startTime < 0 {
			startTime = 0
		}

		log.Printf("mux: fast seek to segment %d (%.0fs) with the active sources", targetSegment, startTime)
		gen, err := m.launchGeneration(job, state, old.planIndex, old.prepared, old.tier, targetSegment, startTime, 0)

		state.mu.Lock()
		wait := state.recoveryWait
		if err == nil && !state.closed {
			if old != gen && old.planIndex != gen.planIndex {
				state.discontinuities = append(state.discontinuities, targetSegment)
			}
			state.active = gen
			state.all = append(state.all, gen)
			state.lastRecovery = time.Now()
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
			log.Printf("mux: fast seek failed (%v); falling back to full recovery", err)
			m.ensureRecovery(job, state, targetSegment, "seek fallback")
			return
		}
		if old != gen {
			old.session.Cancel()
		}
		log.Printf("mux: fast seek landed on segment %d", targetSegment)
		go m.monitorGeneration(job, state, gen)
	}()
}

func (m *Muxer) ensureRecovery(job *model.MuxJob, state *playbackState, startSegment int, reason string) {
	state.mu.Lock()
	if state.closed || state.recovering || state.errorGeneration != nil {
		state.mu.Unlock()
		return
	}
	state.recovering = true
	state.recoveryWait = make(chan struct{})
	state.recoveryErr = nil
	state.mu.Unlock()
	go m.runRecovery(job, state, startSegment, reason)
}

func (m *Muxer) runRecovery(job *model.MuxJob, state *playbackState, startSegment int, reason string) {
	// A seek keeps the same sources (only the offset changes); a dead or
	// unsustainable session must reconsider its sources.
	state.mu.Lock()
	prefer := state.active
	if prefer != nil && prefer.isLocal {
		prefer = nil
	}
	state.mu.Unlock()
	if prefer != nil && reason != "seek" {
		m.markComposerFailed(state, prefer.prepared.plan.Video.SourceKey())
	}
	if reason == "player throughput" {
		// The bottleneck is the player's link, not the server: relaunching
		// the same heavy source would always succeed (server-side
		// production is healthy) and the downgrade would be a no-op. Drop
		// the preference so the composer picks the next lighter video.
		prefer = nil
	}
	if reason == "no active session" {
		state.mu.Lock()
		if state.composer != nil {
			state.composer.reset()
		}
		state.mu.Unlock()
	}

	winner, err := m.coordinateRecovery(job, state, prefer, startSegment, m.policy.StartupTimeout)

	state.mu.Lock()
	wait := state.recoveryWait
	old := state.active
	if err == nil && !state.closed {
		state.active = winner
		state.all = append(state.all, winner)
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
		// Everything failed mid-playback: serve the error video from here.
		if m.errorPath != "" && state.composer != nil {
			state.mu.Lock()
			exhausted := state.composer.exhausted()
			state.mu.Unlock()
			if exhausted {
				if errGen := m.startErrorGeneration(state, startSegment); errGen != nil {
					state.mu.Lock()
					state.active = errGen
					state.errorGeneration = errGen
					state.recoveryErr = nil
					state.mu.Unlock()
					log.Printf("mux: recovery exhausted after %s; serving error video at segment %d", reason, startSegment)
				}
			}
		}
		return
	}
	if old != nil && old != winner {
		old.session.Cancel()
	}
	log.Printf("mux: switched at segment %d to video#%d audio#%d (%s) after %s", startSegment, winner.prepared.videoIdx, winner.prepared.audioIdx, winner.plan.Kind, reason)
	go m.monitorGeneration(job, state, winner)
}

// ActiveTier returns the ABR tier currently serving the job (0 = primary).
// Used by the segment handler to spin up the correct tier when a segment is
// not yet produced. It returns 0 before playback starts.
func (m *Muxer) ActiveTier(job *model.MuxJob) int {
	state := m.lookupState(job.ID)
	if state == nil {
		return 0
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.activeTier
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
			// Never reap while a recovery/seek is in flight (the player may be
			// quiet during a long seek) or the placeholder is still live.
			if (state.active != nil || state.placeholder != nil) && now.Sub(state.lastAccess) > m.policy.IdleTimeout && !state.recovering {
				if state.active != nil {
					active := state.active
					state.active = nil
					state.mu.Unlock()
					active.session.Cancel()
					continue
				}
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
