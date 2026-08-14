package muxer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

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
	resolver  *resolver.Resolver
	store     ports.MuxStore

	probeMu    sync.Mutex
	probeCache map[string]*ffmpeg.ProbeResult
}

type Result struct {
	Dubbed    *model.StremioStream
	Subtitled *model.StremioStream
}

func New(col *collector.Collector, an *analyzer.Analyzer, ff *ffmpeg.Muxer, res *resolver.Resolver, store ports.MuxStore) *Muxer {
	return &Muxer{
		collector:  col,
		analyzer:   an,
		ffmpeg:     ff,
		resolver:   res,
		store:      store,
		probeCache: make(map[string]*ffmpeg.ProbeResult),
	}
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

	streams := m.collector.CollectStreams(ctx, allAddons, contentType, contentID)
	if len(streams) == 0 {
		return &Result{}, nil
	}

	// Resolve unresolved torrent streams (infoHash only) via the user's debrid.
	streams = m.resolveTorrents(ctx, cfg, streams)

	result := &Result{}

	bestVideo, bestAudioDubbed, audioTrackIndex := m.selectMatchingPair(ctx, streams, cfg.Language)
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
			// Different streams: remux video from one, audio from the other.
			job := &model.MuxJob{
				VideoURL:        videoURL,
				AudioURL:        audioURL,
				AudioTrackIndex: audioTrackIndex,
				Title:           fmt.Sprintf("%s + %s", bestVideo.AddonName, bestAudioDubbed.AddonName),
			}
			jobID := m.store.Save(job)
			result.Dubbed = &model.StremioStream{
				Name:        fmt.Sprintf("🎬 Dublado — Vídeo %s %s + Áudio %s", bestVideo.Parsed.Resolution, bestVideo.Parsed.Quality, cfg.Language),
				Description: fmt.Sprintf("Vídeo: %s (%s) | Áudio: %s | Remux", bestVideo.AddonName, bestVideo.Parsed.Resolution, bestAudioDubbed.AddonName),
				URL:         fmt.Sprintf("/mux/%s", jobID),
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

// durationTolerance is the minimum ratio between two durations for them to be
// considered the same release (e.g. theatrical vs extended cuts differ by more
// than this, so they are not mixed).
const durationTolerance = 0.95

// maxMatchCandidates caps how many candidates of each kind are probed during
// duration matching. Probing remote URLs is slow, and beyond a few candidates
// the marginal value drops sharply.
const maxMatchCandidates = 3

// maxAudioCandidates caps how many dubbed audio candidates are probed for a
// track in the target language. The best dubbed audio is what matters; extra
// candidates only help when the top one's duration clashes.
const maxAudioCandidates = 2

// matchTimeout bounds the total time spent on duration matching. Each ffprobe
// of a remote URL can take several seconds, so we cap the whole matching pass
// to keep stream responses snappy; on timeout the best pair is used as-is.
const matchTimeout = 8 * time.Second

// selectMatchingPair picks a (video, audio) pair whose durations match, so the
// remux does not combine audio and video from different releases (e.g. an
// extended cut's video with a theatrical cut's audio).
//
// Candidates are considered in quality order, alternating between the next
// best video and the next best audio, always comparing a newly selected
// candidate against every candidate of the other kind already accepted. This
// finds the best-matching pair without an exhaustive O(n*m) search.
//
// If no pair matches, it falls back to the best video + best audio (a
// best-effort remux), and ultimately to the dubbed audio stream alone.
func (m *Muxer) selectMatchingPair(ctx context.Context, streams []model.CollectedStream, targetLanguage string) (*model.CollectedStream, *model.CollectedStream, int) {
	// Duration matching (including track probes) is bounded by a time budget so
	// slow ffprobe calls never stall the stream response. On timeout, fall back
	// to the best pair.
	matchCtx, cancel := context.WithTimeout(ctx, matchTimeout)
	defer cancel()

	// Video candidates with a direct URL, capped to keep the matching fast.
	videoRanked := m.analyzer.RankVideo(streams)
	audioRanked := m.analyzer.RankAudio(streams, targetLanguage)

	videoCandidates := make([]*model.CollectedStream, 0, maxMatchCandidates)
	for i := range videoRanked {
		if videoRanked[i].Stream.Stream.URL != "" {
			videoCandidates = append(videoCandidates, &videoRanked[i].Stream)
			if len(videoCandidates) >= maxMatchCandidates {
				break
			}
		}
	}

	// The best dubbed audio (requires a track in the target language). Only the
	// top candidate is probed upfront; additional audio candidates are probed
	// lazily by the duration matching if needed.
	var audioCandidates []*model.CollectedStream
	var audioTracks []int
	for i := range audioRanked {
		c := &audioRanked[i].Stream
		if c.Stream.URL == "" {
			continue
		}
		index, err := m.findAudioTrack(matchCtx, c.Stream.URL, targetLanguage)
		if err != nil || index < 0 {
			continue
		}
		audioCandidates = append(audioCandidates, c)
		audioTracks = append(audioTracks, index)
		if len(audioCandidates) >= maxAudioCandidates {
			break
		}
	}

	if len(videoCandidates) == 0 {
		if len(audioCandidates) == 0 {
			return nil, nil, 0
		}
		return nil, audioCandidates[0], audioTracks[0]
	}
	if len(audioCandidates) == 0 {
		return videoCandidates[0], nil, 0
	}

	bestVideo := videoCandidates[0]
	bestAudio := audioCandidates[0]
	bestTrack := audioTracks[0]

	// Fast path: the top candidates' durations match.
	if m.durationsMatch(matchCtx, bestVideo, bestAudio) {
		return bestVideo, bestAudio, bestTrack
	}

	// Progressive matching. Durations are probed lazily: only the candidates
	// actually examined are probed, so a quick match avoids probing the whole
	// list (each ffprobe on a remote URL costs seconds). Bounded by
	// matchTimeout; on timeout the best pair is used as-is.
	vi, ai := findMatchingPairLazy(m, matchCtx, videoCandidates, audioCandidates, audioTracks)
	if vi >= 0 && ai >= 0 {
		return videoCandidates[vi], audioCandidates[ai], audioTracks[ai]
	}

	// No duration match found (or budget exhausted) — fall back to the best
	// pair (best-effort remux).
	return bestVideo, bestAudio, bestTrack
}

// findMatchingPairLazy scans video and audio candidates in quality order,
// probing durations on demand, and returns the first matching pair's indices.
// It alternates between pulling the next best video and the next best audio,
// comparing each newly examined candidate against every already-accepted
// candidate of the other kind. A candidate whose duration cannot be probed is
// skipped. Returns (-1, -1) when no pair matches.
func findMatchingPairLazy(m *Muxer, ctx context.Context, videoCandidates, audioCandidates []*model.CollectedStream, audioTracks []int) (int, int) {
	nv, na := len(videoCandidates), len(audioCandidates)
	if nv == 0 || na == 0 {
		return -1, -1
	}

	type entry struct {
		idx int
		dur float64
	}
	videos := make([]entry, 0, nv)
	audios := make([]entry, 0, na)

	// Probe and seed the top candidates, checking them against each other.
	if d, ok := m.probeDuration(ctx, videoCandidates[0].Stream.URL); ok {
		videos = append(videos, entry{0, d})
	}
	if d, ok := m.probeDuration(ctx, audioCandidates[0].Stream.URL); ok {
		audios = append(audios, entry{0, d})
	}
	if len(videos) > 0 && len(audios) > 0 && durationsClose(videos[0].dur, audios[0].dur) {
		return 0, 0
	}

	vi, ai := 1, 1
	for vi < nv || ai < na {
		if vi < nv {
			if d, ok := m.probeDuration(ctx, videoCandidates[vi].Stream.URL); ok {
				for _, a := range audios {
					if durationsClose(d, a.dur) {
						return vi, a.idx
					}
				}
				videos = append(videos, entry{vi, d})
			}
			vi++
		}

		if ai < na {
			if d, ok := m.probeDuration(ctx, audioCandidates[ai].Stream.URL); ok {
				for _, v := range videos {
					if durationsClose(v.dur, d) {
						return v.idx, ai
					}
				}
				audios = append(audios, entry{ai, d})
			}
			ai++
		}
	}
	return -1, -1
}

// findMatchingPair returns the indices of the first (video, audio) pair whose
// durations match, considering candidates in quality order. It is the pure
// (testable) form of the matching logic, given precomputed durations. A
// duration of <= 0 (unknown/unprobeable) is skipped. Returns (-1, -1) when no
// pair matches.
func findMatchingPair(videoDurations, audioDurations []float64) (int, int) {
	nv, na := len(videoDurations), len(audioDurations)
	if nv == 0 || na == 0 {
		return -1, -1
	}

	type entry struct {
		idx int
		dur float64
	}
	videos := make([]entry, 0, nv)
	audios := make([]entry, 0, na)

	// Seed with the top candidates and check them against each other.
	if videoDurations[0] > 0 && audioDurations[0] > 0 {
		if durationsClose(videoDurations[0], audioDurations[0]) {
			return 0, 0
		}
		videos = append(videos, entry{0, videoDurations[0]})
		audios = append(audios, entry{0, audioDurations[0]})
	} else {
		if videoDurations[0] > 0 {
			videos = append(videos, entry{0, videoDurations[0]})
		}
		if audioDurations[0] > 0 {
			audios = append(audios, entry{0, audioDurations[0]})
		}
	}

	vi, ai := 1, 1
	for vi < nv || ai < na {
		if vi < nv {
			if videoDurations[vi] > 0 {
				for _, a := range audios {
					if durationsClose(videoDurations[vi], a.dur) {
						return vi, a.idx
					}
				}
				videos = append(videos, entry{vi, videoDurations[vi]})
			}
			vi++
		}

		if ai < na {
			if audioDurations[ai] > 0 {
				for _, v := range videos {
					if durationsClose(v.dur, audioDurations[ai]) {
						return v.idx, ai
					}
				}
				audios = append(audios, entry{ai, audioDurations[ai]})
			}
			ai++
		}
	}
	return -1, -1
}

// durationsMatch reports whether the video and audio candidates' durations are
// close enough to be the same release.
func (m *Muxer) durationsMatch(ctx context.Context, video, audio *model.CollectedStream) bool {
	vd, ok := m.probeDuration(ctx, video.Stream.URL)
	if !ok {
		return false
	}
	ad, ok := m.probeDuration(ctx, audio.Stream.URL)
	if !ok {
		return false
	}
	return durationsClose(vd, ad)
}

// durationsClose reports whether two durations (in seconds) are within the
// tolerance ratio.
func durationsClose(a, b float64) bool {
	if a <= 0 || b <= 0 {
		return false
	}
	min, max := a, b
	if min > max {
		min, max = max, min
	}
	return min/max >= durationTolerance
}

// probeURL returns a cached probe result for a URL, probing it once.
func (m *Muxer) probeURL(ctx context.Context, url string) *ffmpeg.ProbeResult {
	if url == "" {
		return nil
	}
	m.probeMu.Lock()
	if res, ok := m.probeCache[url]; ok {
		m.probeMu.Unlock()
		return res
	}
	m.probeMu.Unlock()

	res, err := m.ffmpeg.Probe(ctx, url)
	if err != nil {
		return nil
	}
	m.probeMu.Lock()
	m.probeCache[url] = res
	m.probeMu.Unlock()
	return res
}

// probeDuration returns the duration of a stream, using the unified probe cache.
func (m *Muxer) probeDuration(ctx context.Context, url string) (float64, bool) {
	res := m.probeURL(ctx, url)
	if res == nil || res.Duration <= 0 {
		return 0, false
	}
	return res.Duration, true
}

// findAudioTrack returns the index of the audio track matching the target
// language, or -1 when none matches. It uses the unified probe cache so the
// URL is probed only once for both duration and tracks.
func (m *Muxer) findAudioTrack(ctx context.Context, streamURL, targetLanguage string) (int, error) {
	res := m.probeURL(ctx, streamURL)
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

// resolveTorrents resolves streams that only have an infoHash (no direct URL)
// via the user's debrid services. Streams with a direct URL are left untouched.
func (m *Muxer) resolveTorrents(ctx context.Context, cfg *model.Config, streams []model.CollectedStream) []model.CollectedStream {
	out := make([]model.CollectedStream, 0, len(streams))
	for _, s := range streams {
		if s.Stream.URL == "" && s.Stream.InfoHash != "" {
			url, serviceID := m.resolver.Resolve(ctx, cfg, s.Stream.InfoHash, s.Stream.Title)
			if url != "" {
				s.Stream.URL = url
				s.AddonName = s.AddonName + " [" + serviceID + "]"
			}
		}
		out = append(out, s)
	}
	return out
}
