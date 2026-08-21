package muxer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/streammux/streammux/internal/application/collector"
	"github.com/streammux/streammux/internal/application/ffmpeg"
	"github.com/streammux/streammux/internal/application/planner"
	"github.com/streammux/streammux/internal/application/resolver"
	"github.com/streammux/streammux/internal/domain/model"
	"github.com/streammux/streammux/internal/domain/ports"
)

type streamCollector interface {
	CollectStreams(context.Context, []model.Addon, string, string) []model.CollectedStream
}

type playbackPlanner interface {
	Build([]model.CollectedStream, string) []model.PlaybackPlan
	VideoCandidates([]model.CollectedStream, string) []model.CollectedStream
}

type mediaEngine interface {
	Probe(context.Context, string) (*ffmpeg.ProbeResult, error)
	StartSession(context.Context, ffmpeg.SessionSpec) (*ffmpeg.Session, error)
	StartSinglePlaceholderSession(context.Context, string, string, bool) (*ffmpeg.Session, error)        // bool: realtime pacing
	StartImagePlaceholderSession(context.Context, string, string, string, bool) (*ffmpeg.Session, error) // (video, posterImage, outputDir, realtime)
	DetectAudioOffset(string, string, []ffmpeg.AudioTrack, int, float64) (time.Duration, int, float64, error)
}

// dynamicPlaceholderEngine is optional so existing lightweight media fakes and
// alternate engines can keep using the original placeholder methods. The
// FFmpeg implementation supports cards and non-zero segment starts, which are
// required for an atomic late-poster replacement.
type dynamicPlaceholderEngine interface {
	StartPlaceholderSession(context.Context, ffmpeg.PlaceholderSpec) (*ffmpeg.Session, error)
}

// Policy groups the small number of operational deadlines used during startup
// and recovery. They are intentionally internal defaults rather than user-facing
// knobs until measurements from real Stremio clients justify configuration.
type Policy struct {
	StartupTimeout    time.Duration
	AttemptTimeout    time.Duration
	SegmentTimeout    time.Duration
	IdleTimeout       time.Duration
	HealthWindow      time.Duration
	RecoveryCooldown  time.Duration
	RetryCooldown     time.Duration
	MinRealtime       float64
	MinPublishedAhead time.Duration
	// MinHandoffBuffer is how much content the film must have ready before the
	// intro hands off, so playback doesn't stall on the first bandwidth dip.
	MinHandoffBuffer time.Duration
	// TierSwitchBuffer is the cushion required when spinning up a lazy ABR
	// tier switch: smaller than the startup cushion so the switch is fast,
	// but nonzero so the first segment is already out before the player
	// resumes.
	TierSwitchBuffer time.Duration
	// TierSwitchCooldown prevents the player from bouncing between virtual
	// renditions while its ABR estimate settles. During the cooldown the
	// active generation is served under the requested tier URL and no second
	// generation is started.
	TierSwitchCooldown time.Duration
	DurationTolerance  float64
	// PlaceholderMinTime is how long the placeholder must play before the
	// film takes over, even when the film is ready sooner.
	PlaceholderMinTime time.Duration
	// CacheMaxBytes caps all playback caches managed by this process.
	CacheMaxBytes int64
	// CacheMinFreeBytes keeps space available for the OS and Docker.
	CacheMinFreeBytes int64
	// SessionMaxBytes caps one active generation, regardless of bitrate.
	SessionMaxBytes int64
}

func defaultPolicy() Policy {
	return Policy{
		// The Stremio client tolerates roughly 60s before it gives up on
		// starting playback, so startup can use a generous window. Lenient uses
		// half of StartupTimeout and re-runs cached probes, so it stays fast.
		StartupTimeout:     50 * time.Second,
		AttemptTimeout:     25 * time.Second,
		SegmentTimeout:     30 * time.Second,
		IdleTimeout:        90 * time.Second,
		HealthWindow:       4 * time.Second,
		RecoveryCooldown:   10 * time.Second,
		RetryCooldown:      30 * time.Second,
		// Remote film inputs are intentionally paced at 1x. Allow normal FFmpeg
		// scheduling jitter around that limit; genuinely slow sources still fall
		// below this threshold across consecutive health windows.
		MinRealtime:        0.90,
		MinPublishedAhead:  12 * time.Second,
		MinHandoffBuffer:   20 * time.Second,
		TierSwitchBuffer:   8 * time.Second,
		TierSwitchCooldown: 30 * time.Second,
		DurationTolerance:  0.002,
		PlaceholderMinTime: 8 * time.Second,
		CacheMaxBytes:      8 * 1024 * 1024 * 1024,
		CacheMinFreeBytes:  3 * 1024 * 1024 * 1024,
		SessionMaxBytes:    512 * 1024 * 1024,
	}
}

type Muxer struct {
	collector streamCollector
	planner   playbackPlanner
	ffmpeg    mediaEngine
	resolver  *resolver.Resolver
	store     ports.MuxStore
	baseURL   string
	policy    Policy

	// placeholderPath plays instantly while sources prepare; errorPath is the
	// terminal "no source worked" video. Both optional.
	placeholderPath string
	errorPath       string

	httpClient *http.Client

	stateMu sync.Mutex
	states  map[string]*playbackState

	cacheMu        sync.Mutex
	resolved       map[string]resolvedEntry
	resolveFlights map[string]*resolveFlight
	probes         map[string]probeEntry
	probeFlights   map[string]*probeFlight

	offsetMu sync.Mutex
	offsets  map[string]time.Duration

	errSegOnce  sync.Once
	errSegCount int
}

type Result struct {
	Dubbed    *model.StremioStream
	Subtitled *model.StremioStream
}

func New(col *collector.Collector, pl *planner.Planner, ff *ffmpeg.Muxer, res *resolver.Resolver, store ports.MuxStore, baseURL string) *Muxer {
	return NewWithVideos(col, pl, ff, res, store, baseURL, "", "")
}

// NewWithVideos configures optional local placeholder and error videos.
func NewWithVideos(col *collector.Collector, pl *planner.Planner, ff *ffmpeg.Muxer, res *resolver.Resolver, store ports.MuxStore, baseURL, placeholderPath, errorPath string) *Muxer {
	m := &Muxer{
		collector:       col,
		planner:         pl,
		ffmpeg:          ff,
		resolver:        res,
		store:           store,
		baseURL:         strings.TrimSuffix(baseURL, "/"),
		policy:          defaultPolicy(),
		placeholderPath: placeholderPath,
		errorPath:       errorPath,
		httpClient:      &http.Client{Timeout: 15 * time.Second},
		states:          make(map[string]*playbackState),
		resolved:        make(map[string]resolvedEntry),
		resolveFlights:  make(map[string]*resolveFlight),
		probes:          make(map[string]probeEntry),
		probeFlights:    make(map[string]*probeFlight),
		offsets:         make(map[string]time.Duration),
	}
	go m.reapIdleSessions()
	return m
}

// SetPlaceholderMinTime overrides the minimum placeholder play time.
func (m *Muxer) SetPlaceholderMinTime(d time.Duration) {
	if d >= 0 {
		m.policy.PlaceholderMinTime = d
	}
}

// errorSegmentCount returns how many HLS segments the error video spans,
// probing the local file once.
func (m *Muxer) errorSegmentCount() int {
	m.errSegOnce.Do(func() {
		m.errSegCount = 8 // ~30s at 4s segments; used if the probe fails
		if m.errorPath == "" || m.ffmpeg == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if probe, err := m.ffmpeg.Probe(ctx, m.errorPath); err == nil && probe.Duration > 0 {
			segDur := ffmpeg.SegDuration()
			if segDur <= 0 {
				segDur = 4.0
			}
			if count := int(math.Ceil(probe.Duration / segDur)); count > 0 {
				m.errSegCount = count
			}
		}
	})
	return m.errSegCount
}

func directoryBytes(root string) int64 {
	var total int64
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func (m *Muxer) cacheUsage() int64 {
	m.stateMu.Lock()
	states := make([]*playbackState, 0, len(m.states))
	for _, state := range m.states {
		states = append(states, state)
	}
	m.stateMu.Unlock()
	var total int64
	for _, state := range states {
		state.mu.Lock()
		dir := state.cacheDir
		state.mu.Unlock()
		total += directoryBytes(dir)
	}
	return total
}

func (m *Muxer) hasDiskHeadroom(path string) bool {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return true
	}
	free := int64(stat.Bavail) * int64(stat.Bsize)
	return free >= m.policy.CacheMinFreeBytes
}

// enforceCacheBudget keeps active windows bounded by bytes and removes
// completed generations left behind by seeks, ABR switches, and recoveries.
// Active generations are never removed here; a missing old segment triggers a
// normal seek restart instead.
func (m *Muxer) enforceCacheBudget() {
	m.stateMu.Lock()
	states := make([]*playbackState, 0, len(m.states))
	for _, state := range m.states {
		states = append(states, state)
	}
	m.stateMu.Unlock()

	activeCount := 0
	for _, state := range states {
		state.mu.Lock()
		if state.active != nil {
			activeCount++
		}
		state.mu.Unlock()
	}
	sessionLimit := m.policy.SessionMaxBytes
	if activeCount > 0 && m.policy.CacheMaxBytes > 0 {
		fairShare := m.policy.CacheMaxBytes / int64(activeCount)
		if fairShare > 0 && fairShare < sessionLimit {
			sessionLimit = fairShare
		}
	}

	for _, state := range states {
		state.mu.Lock()
		active := state.active
		all := append([]*generation(nil), state.all...)
		state.mu.Unlock()
		if active != nil {
			pruneGenerationBytes(active.dir, sessionLimit)
		}
		for _, generation := range all {
			if generation == nil || generation == active || generation.session == nil {
				continue
			}
			select {
			case <-generation.session.Done():
				_ = os.RemoveAll(generation.dir)
			default:
			}
		}
	}

	usage := m.cacheUsage()
	if usage <= m.policy.CacheMaxBytes && m.hasDiskHeadroom(os.TempDir()) {
		return
	}
	// Under pressure, cancel retired sessions that have not exited yet. The
	// active generation and placeholder are protected because they serve the
	// current player timeline.
	for _, state := range states {
		state.mu.Lock()
		active := state.active
		placeholder := state.placeholder
		retired := make([]*generation, 0)
		for _, generation := range state.all {
			if generation != nil && generation != active && generation != placeholder && generation.session != nil {
				retired = append(retired, generation)
			}
		}
		state.mu.Unlock()
		for _, generation := range retired {
			generation.session.Cancel()
			m.removeGenerationWhenStopped(generation)
		}
	}
}

type cacheSegment struct {
	index int
	paths []string
	size  int64
}

func pruneGenerationBytes(dir string, maxBytes int64) {
	if dir == "" || maxBytes <= 0 {
		return
	}
	segments := map[int]*cacheSegment{}
	for _, media := range []string{"video", "audio"} {
		entries, err := os.ReadDir(filepath.Join(dir, media))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			var index int
			if _, err := fmt.Sscanf(entry.Name(), "seg_%05d.ts", &index); err != nil {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			segment := segments[index]
			if segment == nil {
				segment = &cacheSegment{index: index}
				segments[index] = segment
			}
			segment.paths = append(segment.paths, filepath.Join(dir, media, entry.Name()))
			segment.size += info.Size()
		}
	}
	var total int64
	ordered := make([]*cacheSegment, 0, len(segments))
	for _, segment := range segments {
		total += segment.size
		ordered = append(ordered, segment)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })
	for _, segment := range ordered {
		if total <= maxBytes {
			break
		}
		for _, path := range segment.paths {
			_ = os.Remove(path)
		}
		total -= segment.size
	}
}

// Process returns the public stream entry after a short Cinemeta lookup.
// Addon collection/planning continues in the job lifecycle, while source
// resolution, probing and FFmpeg work remain lazy until the user plays.
func (m *Muxer) Process(ctx context.Context, cfg *model.Config, contentType, contentID string) (*Result, error) {
	if cfg == nil {
		return &Result{}, fmt.Errorf("missing config")
	}
	addons := uniqueAddons(cfg.ValidAddons())
	if len(addons) == 0 {
		return &Result{}, nil
	}

	job := &model.MuxJob{
		TargetLanguage: cfg.Language,
		Config:         *cfg,
		ContentType:    contentType,
		ContentID:      contentID,
		TierMetas:      placeholderTierMetas(),
	}
	jobID := m.store.Save(job)
	if jobID == "" {
		return &Result{}, fmt.Errorf("store returned empty job id")
	}

	posterDir, err := os.MkdirTemp("", "streammux-poster-"+jobID+"-*")
	if err == nil {
		job.CacheDir = posterDir
	} else {
		log.Printf("mux: cannot create poster cache: %v", err)
	}

	state, err := m.stateFor(job)
	if err != nil {
		if posterDir != "" {
			_ = os.RemoveAll(posterDir)
		}
		return &Result{}, err
	}

	metadata := contentMetadata{}
	// Keep the identity lookup bounded more tightly than addon collection: a
	// slow Cinemeta response must not make autoplay feel like it is waiting for
	// the old synchronous pipeline.
	metadataCtx, metadataCancel := context.WithTimeout(ctx, 750*time.Millisecond)
	if fetched, fetchErr := fetchCinemetaMetadata(metadataCtx, m.httpClient, contentType, contentID, cfg.Language); fetchErr == nil {
		metadata = fetched
	} else {
		log.Printf("mux: Cinemeta metadata unavailable: %v", fetchErr)
	}
	metadataCancel()

	state.mu.Lock()
	state.metadata = metadata
	state.backgroundPath = filepath.Join(state.cacheDir, "placeholder-background.jpg")
	state.cardPath = filepath.Join(state.cacheDir, "placeholder-card.txt")
	state.metadataCardPath = filepath.Join(state.cacheDir, "placeholder-metadata.txt")
	state.detailsCardPath = filepath.Join(state.cacheDir, "placeholder-details.txt")
	state.preparationWait = make(chan struct{})
	state.tierMetas = append([]model.TierMeta(nil), job.TierMetas...)
	state.mu.Unlock()
	job.Title = contentDisplayTitle(metadata)
	if err := writePlaceholderCard(state.cardPath, renderPlaceholderCard(metadata, nil, cfg.Language)); err != nil {
		log.Printf("mux: cannot initialize placeholder card: %v", err)
	}
	if err := writePlaceholderCard(state.metadataCardPath, renderPlaceholderMetadata(metadata)); err != nil {
		log.Printf("mux: cannot initialize placeholder metadata: %v", err)
	}
	if err := writePlaceholderCard(state.detailsCardPath, renderPlaceholderDetails(metadata, nil, cfg.Language)); err != nil {
		log.Printf("mux: cannot initialize placeholder details: %v", err)
	}

	if metadata.LogoURL != "" && state.posterPath != "" {
		m.prefetchPosterURL(state.ctx, metadata.LogoURL, state.posterPath)
	}
	if metadata.BackgroundURL != "" && state.backgroundPath != "" {
		m.prefetchPosterURL(state.ctx, metadata.BackgroundURL, state.backgroundPath)
	}
	if metadata.LogoURL == "" || metadata.BackgroundURL == "" {
		// If the short synchronous metadata budget expired, make one best-effort
		// background attempt so the poster can still arrive during playback.
		m.prefetchImagesForContent(state.ctx, contentType, contentID, state.posterPath, state.backgroundPath)
	}
	go m.prepareJob(job, state, addons, cfg.Language)

	name, description := streamPresentation(metadata)
	return &Result{Dubbed: &model.StremioStream{
		Name:        name,
		Description: description,
		URL:         m.muxURL(jobID),
		BehaviorHints: map[string]any{
			"notWebReady": true,
		},
	}}, nil
}

func streamPresentation(metadata contentMetadata) (string, string) {
	name := "StreamMux MultiAudio"
	if title := contentDisplayTitle(metadata); title != "StreamMux MultiAudio" {
		name = fmt.Sprintf("%s • StreamMux MultiAudio", title)
	}

	var facts []string
	if metadata.Year != "" {
		facts = append(facts, metadata.Year)
	}
	if metadata.Rating != "" {
		facts = append(facts, "★ "+metadata.Rating)
	}
	lines := make([]string, 0, 2)
	if len(facts) > 0 {
		lines = append(lines, strings.Join(facts, " • "))
	}
	lines = append(lines, "Áudio no seu idioma • qualidade máxima • remux automático")
	return name, strings.Join(lines, "\n")
}

// prepareJob is deliberately tied to state.ctx. The HTTP request is allowed
// to finish immediately, but addon calls remain alive until playback is
// canceled or the store expires the job.
func (m *Muxer) prepareJob(job *model.MuxJob, state *playbackState, addons []model.Addon, language string) {
	streams := m.collector.CollectStreams(state.ctx, addons, job.ContentType, job.ContentID)
	plans := m.planner.Build(streams, language)
	candidates := m.planner.VideoCandidates(streams, language)

	state.mu.Lock()
	if state.closed {
		state.mu.Unlock()
		return
	}
	job.Plans = plans
	job.VideoCandidates = candidates
	metas := tierMetasFromPlans(plans)
	if len(metas) == 0 {
		metas = placeholderTierMetas()
	}
	job.TierMetas = metas
	state.tierMetas = append([]model.TierMeta(nil), metas...)
	state.prepared = len(plans) > 0
	if !state.prepared {
		state.preparationErr = errors.New("addons returned no playable streams")
	}
	wait := state.preparationWait
	state.mu.Unlock()

	if len(plans) > 0 {
		_ = writePlaceholderCard(state.cardPath, renderPlaceholderCard(state.metadata, plans, language))
		_ = writePlaceholderCard(state.metadataCardPath, renderPlaceholderMetadata(state.metadata))
		_ = writePlaceholderCard(state.detailsCardPath, renderPlaceholderDetails(state.metadata, plans, language))
	}
	if wait != nil {
		close(wait)
	}
}

func uniqueAddons(addons []model.Addon) []model.Addon {
	seen := make(map[string]struct{}, len(addons))
	out := make([]model.Addon, 0, len(addons))
	for _, addon := range addons {
		key := addon.ID + "\x00" + addon.ManifestURL
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, addon)
	}
	return out
}

func firstPlan(plans []model.PlaybackPlan, matches func(model.PlaybackPlan) bool) (model.PlaybackPlan, bool) {
	for _, plan := range plans {
		if matches(plan) {
			return plan, true
		}
	}
	return model.PlaybackPlan{}, false
}

func (m *Muxer) muxURL(jobID string) string {
	path := "/mux/" + jobID + "/playlist.m3u8"
	if m.baseURL == "" {
		return path
	}
	return m.baseURL + path
}

// DirectFallbackError tells the HTTP layer that every bounded HLS startup
// attempt failed, but a direct source remains available as the last-resort path.
type DirectFallbackError struct {
	URL string
	Err error
}

func (e *DirectFallbackError) Error() string {
	if e.Err == nil {
		return "direct playback fallback required"
	}
	return "direct playback fallback required: " + e.Err.Error()
}

func (e *DirectFallbackError) Unwrap() error { return e.Err }

func directStream(name, description string, src model.Stream) *model.StremioStream {
	out := &model.StremioStream{Name: name, Description: description}
	if src.URL != "" {
		out.URL = src.URL
	} else {
		out.InfoHash = src.InfoHash
		out.FileIdx = src.FileIdx
	}
	return out
}
