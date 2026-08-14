package muxer

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/streammux/streammux/internal/application/analyzer"
	"github.com/streammux/streammux/internal/application/collector"
	"github.com/streammux/streammux/internal/application/ffmpeg"
	"github.com/streammux/streammux/internal/application/parser"
	"github.com/streammux/streammux/internal/application/resolver"
	"github.com/streammux/streammux/internal/domain/constants"
	"github.com/streammux/streammux/internal/domain/model"
	"github.com/streammux/streammux/internal/domain/ports"
)

type Muxer struct {
	collector *collector.Collector
	analyzer  *analyzer.Analyzer
	ffmpeg    *ffmpeg.Muxer
	store     ports.MuxStore

	// baseURL is the public origin (e.g. https://mystreamux.joaotolovi.com)
	// used to build absolute URLs for muxed streams so players resolve them
	// reliably. Empty falls back to a path-relative URL.
	baseURL string

	// probeFn is the underlying probe implementation, defaulting to
	// ffmpeg.Probe but swappable in tests to avoid real network calls.
	probeFn func(ctx context.Context, url string) (*ffmpeg.ProbeResult, error)
}

type Result struct {
	Dubbed    *model.StremioStream
	Subtitled *model.StremioStream
}

func New(col *collector.Collector, an *analyzer.Analyzer, ff *ffmpeg.Muxer, _ *resolver.Resolver, store ports.MuxStore, baseURL string) *Muxer {
	m := &Muxer{
		collector: col,
		analyzer:  an,
		ffmpeg:    ff,
		store:     store,
		baseURL:   strings.TrimSuffix(baseURL, "/"),
	}
	m.probeFn = ff.Probe
	return m
}

func (m *Muxer) Process(ctx context.Context, cfg *model.Config, contentType, contentID string) (*Result, error) {
	videoAddons := cfg.AddonsByRole(constants.RoleVideo)
	audioAddons := cfg.AddonsByRole(constants.RoleAudio)

	var allAddons []model.Addon
	allAddons = append(allAddons, videoAddons...)
	allAddons = append(allAddons, audioAddons...)

	if len(allAddons) == 0 {
		return &Result{}, nil
	}

	// Collect and parse streams from all configured addons. This is the only
	// network work done at listing time — no ffprobe, no debrid resolution.
	// Those happen lazily when the user actually plays the muxed stream.
	streams := m.collector.CollectStreams(ctx, allAddons, contentType, contentID)
	if len(streams) == 0 {
		return &Result{}, nil
	}

	result := &Result{}

	// Pick the best video and best dubbed audio using filename parsing alone
	// (fast, no network). Duration/track verification is deferred to the mux
	// endpoint.
	bestVideo, bestAudioDubbed := m.selectPair(streams, cfg.Language)

	if bestAudioDubbed != nil && bestVideo != nil {
		videoURL := bestVideo.Stream.URL
		audioURL := bestAudioDubbed.Stream.URL

		switch {
		case audioURL != "" && videoURL == audioURL:
			// Same stream: audio and video already together, no remux needed.
			result.Dubbed = directStream(
				fmt.Sprintf("🔊 Dublado — %s %s", bestAudioDubbed.Parsed.Resolution, bestAudioDubbed.Parsed.Quality),
				fmt.Sprintf("Fonte: %s | Sem remux (já dublado)", bestAudioDubbed.AddonName),
				bestAudioDubbed.Stream,
			)
		case audioURL != "" && videoURL != "":
			// Different streams: schedule a remux. The job carries the target
			// language so the mux endpoint can pick the correct audio track
			// (and verify durations) at playback time.
			job := &model.MuxJob{
				VideoURL:       videoURL,
				AudioURL:       audioURL,
				TargetLanguage: cfg.Language,
				Title:          fmt.Sprintf("%s + %s", bestVideo.AddonName, bestAudioDubbed.AddonName),
			}
			jobID := m.store.Save(job)
			muxURL := "/mux/" + jobID
			if m.baseURL != "" {
				muxURL = m.baseURL + muxURL
			}
			result.Dubbed = &model.StremioStream{
				Name:        fmt.Sprintf("🎬 Dublado — Vídeo %s %s + Áudio %s", bestVideo.Parsed.Resolution, bestVideo.Parsed.Quality, cfg.Language),
				Description: fmt.Sprintf("Vídeo: %s (%s) | Áudio: %s | Remux", bestVideo.AddonName, bestVideo.Parsed.Resolution, bestAudioDubbed.AddonName),
				URL:         muxURL,
				BehaviorHints: map[string]any{
					"notWebReady": true,
				},
			}
		default:
			// No direct URL for either side; fall back to the dubbed stream's
			// own infoHash so Stremio can still try to resolve it.
			result.Dubbed = directStream(
				fmt.Sprintf("🔊 Dublado — %s %s", bestAudioDubbed.Parsed.Resolution, bestAudioDubbed.Parsed.Quality),
				fmt.Sprintf("Fonte: %s | Sem remux (já dublado)", bestAudioDubbed.AddonName),
				bestAudioDubbed.Stream,
			)
		}
	}

	if bestVideo != nil {
		videoLang := bestVideo.Language
		if videoLang == "" {
			videoLang = "English"
		}
		result.Subtitled = directStream(
			fmt.Sprintf("🎞️ Legendado — %s %s", bestVideo.Parsed.Resolution, bestVideo.Parsed.Quality),
			fmt.Sprintf("Fonte: %s | %s", bestVideo.AddonName, videoLang),
			bestVideo.Stream,
		)
	}

	return result, nil
}

// selectPair picks the best video and best dubbed audio using filename parsing
// only (no ffprobe). It filters candidates by addon role and, for audio, by the
// target language.
func (m *Muxer) selectPair(streams []model.CollectedStream, targetLanguage string) (*model.CollectedStream, *model.CollectedStream) {
	var bestVideo, bestAudio *model.CollectedStream

	videoRanked := m.analyzer.RankVideo(streams)
	for i := range videoRanked {
		if videoRanked[i].Stream.Stream.URL != "" {
			bestVideo = &videoRanked[i].Stream
			break
		}
	}

	audioRanked := m.analyzer.RankAudio(streams, targetLanguage)
	for i := range audioRanked {
		if audioRanked[i].Stream.Stream.URL != "" {
			bestAudio = &audioRanked[i].Stream
			break
		}
	}

	return bestVideo, bestAudio
}

// selectAudioTrack probes the audio stream and returns the index of the track
// matching the target language. This runs at playback time (in the mux
// endpoint), not at listing time.
func (m *Muxer) selectAudioTrack(ctx context.Context, audioURL, targetLanguage string) (int, error) {
	res := m.probeURL(ctx, audioURL)
	if res == nil {
		return -1, fmt.Errorf("probe failed")
	}
	if len(res.AudioTracks) == 0 {
		return -1, nil
	}
	targetCode := parser.LanguageCode(targetLanguage)
	for _, t := range res.AudioTracks {
		if strings.EqualFold(t.Language, targetCode) {
			return t.Index, nil
		}
		// Untagged audio is assumed to be the default language (English).
		if t.Language == "" && targetLanguage == "English" {
			return t.Index, nil
		}
	}
	return -1, nil
}

// ResolveMuxJob performs the heavy work at playback time: it probes the audio
// source to find the target-language track, then streams the remuxed MKV.
func (m *Muxer) ResolveMuxJob(ctx context.Context, job *model.MuxJob, out io.Writer) error {
	track, err := m.selectAudioTrack(ctx, job.AudioURL, job.TargetLanguage)
	if err != nil {
		return err
	}
	if track < 0 {
		// No matching track found; fall back to the first audio track so the
		// user still gets something to play.
		track = 0
	}
	return m.ffmpeg.Remux(ctx, job.VideoURL, job.AudioURL, track, out)
}

// directStream builds a StremioStream pointing at the source stream. When the
// source has a direct URL it is used; otherwise the infoHash is preserved so
// Stremio can resolve it itself.
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

// probeURL probes a URL (duration + audio tracks). No caching: debrid URLs
// rotate on every request, so a cached result would be stale immediately.
func (m *Muxer) probeURL(ctx context.Context, url string) *ffmpeg.ProbeResult {
	if url == "" {
		return nil
	}
	res, err := m.probeFn(ctx, url)
	if err != nil {
		return nil
	}
	return res
}
