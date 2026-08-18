package muxer

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
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
}

type mediaEngine interface {
	Probe(context.Context, string) (*ffmpeg.ProbeResult, error)
	StartSession(context.Context, ffmpeg.SessionSpec) (*ffmpeg.Session, error)
	StartSinglePlaceholderSession(context.Context, string, string, bool) (*ffmpeg.Session, error) // bool: realtime pacing
	DetectAudioOffset(string, string, []ffmpeg.AudioTrack, int, float64) (time.Duration, int, float64, error)
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
	MinHandoffBuffer  time.Duration
	DurationTolerance float64
	// PlaceholderMinTime is how long the placeholder must play before the
	// film takes over, even when the film is ready sooner.
	PlaceholderMinTime time.Duration
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
		MinRealtime:        1.0,
		MinPublishedAhead:  12 * time.Second,
		MinHandoffBuffer:   4 * time.Second,
		DurationTolerance:  0.002,
		PlaceholderMinTime: 8 * time.Second,
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

// Process performs only addon collection and inexpensive metadata planning.
// Network resolution, probing and FFmpeg work remain lazy until the user plays.
func (m *Muxer) Process(ctx context.Context, cfg *model.Config, contentType, contentID string) (*Result, error) {
	addons := uniqueAddons(cfg.ValidAddons())
	if len(addons) == 0 {
		return &Result{}, nil
	}

	streams := m.collector.CollectStreams(ctx, addons, contentType, contentID)
	plans := m.planner.Build(streams, cfg.Language)
	if len(plans) == 0 {
		return &Result{}, nil
	}

	result := &Result{}
	if subtitle, ok := firstPlan(plans, func(plan model.PlaybackPlan) bool {
		return plan.Kind == model.PlanSubtitledFallback
	}); ok {
		language := subtitle.Video.Language
		if language == "" {
			language = "English"
		}
		result.Subtitled = directStream(
			fmt.Sprintf("🎞️ Legendado — %s %s", subtitle.Video.Parsed.Resolution, subtitle.Video.Parsed.Quality),
			fmt.Sprintf("Fonte: %s | %s", subtitle.Video.AddonName, language),
			subtitle.Video.Stream,
		)
	}

	primary, hasDubbed := firstPlan(plans, func(plan model.PlaybackPlan) bool {
		return plan.HasTargetAudio
	})
	if !hasDubbed {
		return result, nil
	}

	job := &model.MuxJob{
		TargetLanguage: cfg.Language,
		Title:          primary.Video.AddonName + " + " + primary.Audio.AddonName,
		Plans:          plans,
		Config:         *cfg,
	}
	jobID := m.store.Save(job)

	mode := "Remux"
	if primary.SingleSource() {
		mode = "Fonte única"
	}
	result.Dubbed = &model.StremioStream{
		Name: fmt.Sprintf(
			"🎬 Dublado — %s %s + Áudio %s",
			primary.Video.Parsed.Resolution,
			primary.Video.Parsed.Quality,
			cfg.Language,
		),
		Description: fmt.Sprintf(
			"Vídeo: %s (%s) | Áudio: %s | %s com fallback automático",
			primary.Video.AddonName,
			primary.Video.Parsed.Resolution,
			primary.Audio.AddonName,
			mode,
		),
		URL: m.muxURL(jobID),
		BehaviorHints: map[string]any{
			"notWebReady": true,
		},
	}

	return result, nil
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
