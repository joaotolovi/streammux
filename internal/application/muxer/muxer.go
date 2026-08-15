package muxer

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/streammux/streammux/internal/application/analyzer"
	"github.com/streammux/streammux/internal/application/collector"
	"github.com/streammux/streammux/internal/application/ffmpeg"
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

	// httpClient follows redirects to resolve addon URLs into their final CDN
	// URLs. Addon URLs redirect through a slow debrid proxy; the final CDN URL
	// supports Range and answers in milliseconds.
	httpClient *http.Client

	// playlistMu serializes playlist generation per job so concurrent requests
	// don't race on CacheDir/PlaylistReady.
	playlistMu sync.Mutex

	// resolveMu serializes URL resolution so a slow debrid redirect chain is
	// only walked once, even under concurrent segment requests.
	resolveMu sync.Mutex
}

type Result struct {
	Dubbed    *model.StremioStream
	Subtitled *model.StremioStream
}

func New(col *collector.Collector, an *analyzer.Analyzer, ff *ffmpeg.Muxer, _ *resolver.Resolver, store ports.MuxStore, baseURL string) *Muxer {
	return &Muxer{
		collector: col,
		analyzer:  an,
		ffmpeg:    ff,
		store:     store,
		baseURL:   strings.TrimSuffix(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
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

	// Collect and parse streams from all configured addons. This is the only
	// network work done at listing time — no ffprobe, no debrid resolution.
	// Those happen lazily when the user actually plays the muxed stream.
	streams := m.collector.CollectStreams(ctx, allAddons, contentType, contentID)
	if len(streams) == 0 {
		return &Result{}, nil
	}

	result := &Result{}

	bestVideo, bestAudioDubbed := m.selectPair(streams, cfg.Language)

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

	if bestAudioDubbed != nil && bestVideo != nil {
		videoURL := bestVideo.Stream.URL
		audioURL := bestAudioDubbed.Stream.URL

		if videoURL != "" && audioURL != "" {
			// Always schedule a remux so the player automatically gets the
			// user's target-language audio track (the source's default track
			// is usually English). Single-source when video and audio come
			// from the same URL (one HTTP connection), two-source otherwise.
			job := &model.MuxJob{
				VideoURL:         videoURL,
				AudioURL:         audioURL,
				TargetLanguage:   cfg.Language,
				Title:            fmt.Sprintf("%s + %s", bestVideo.AddonName, bestAudioDubbed.AddonName),
				AudioCandidates:  m.audioCandidates(streams, cfg.Language),
			}
			jobID := m.store.Save(job)
			result.Dubbed = &model.StremioStream{
				Name:        fmt.Sprintf("🎬 Dublado — %s %s + Áudio %s", bestVideo.Parsed.Resolution, bestVideo.Parsed.Quality, cfg.Language),
				Description: fmt.Sprintf("Vídeo: %s (%s) | Áudio: %s | Remux", bestVideo.AddonName, bestVideo.Parsed.Resolution, bestAudioDubbed.AddonName),
				URL:         m.muxURL(jobID),
				BehaviorHints: map[string]any{
					"notWebReady": true,
				},
			}
		} else {
			result.Dubbed = directStream(
				fmt.Sprintf("🔊 Dublado — %s %s", bestAudioDubbed.Parsed.Resolution, bestAudioDubbed.Parsed.Quality),
				fmt.Sprintf("Fonte: %s | Sem remux (já dublado)", bestAudioDubbed.AddonName),
				bestAudioDubbed.Stream,
			)
		}
	}

	return result, nil
}

// muxURL builds the absolute URL of the HLS playlist for a job.
func (m *Muxer) muxURL(jobID string) string {
	u := "/mux/" + jobID + "/playlist.m3u8"
	if m.baseURL != "" {
		u = m.baseURL + u
	}
	return u
}

// audioCandidates returns the ordered list of audio source URLs (best first)
// that match the target language, excluding the primary choice. Used as a
// fallback when a debrid source returns a broken/error video with no audio.
func (m *Muxer) audioCandidates(streams []model.CollectedStream, targetLanguage string) []string {
	ranked := m.analyzer.RankAudio(streams, targetLanguage)
	var out []string
	for i := range ranked {
		u := ranked[i].Stream.Stream.URL
		if u == "" {
			continue
		}
		log.Printf("mux: audio candidate %d: %s (size=%d)", i, truncate(u), ranked[i].Stream.Size)
		// Skip the primary (first) candidate — it's already job.AudioURL.
		if i == 0 {
			continue
		}
		out = append(out, u)
	}
	return out
}

func truncate(s string) string {
	if len(s) > 70 {
		return s[:70] + "..."
	}
	return s
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

// EnsurePlaylist makes sure the job has a cache dir and a static .m3u8 playlist.
// The duration is probed once and cached on the job. The playlist is generated
// by code (not FFmpeg) with #EXT-X-ENDLIST, so the player shows the full seek
// bar immediately.
func (m *Muxer) EnsurePlaylist(ctx context.Context, job *model.MuxJob) error {
	if job.PlaylistReady {
		return nil
	}

	m.playlistMu.Lock()
	defer m.playlistMu.Unlock()

	// Re-check after acquiring the lock (another request may have finished).
	if job.PlaylistReady {
		return nil
	}

	if job.CacheDir == "" {
		dir, err := os.MkdirTemp("", "streammux-*")
		if err != nil {
			return fmt.Errorf("create cache dir: %w", err)
		}
		job.CacheDir = dir
	}

	if job.Duration == 0 {
		dur, err := m.ffmpeg.ProbeDuration(ctx, m.resolvedURL(ctx, job, "video"))
		if err != nil {
			log.Printf("mux: probe duration failed, using 7200s: %v", err)
			dur = 7200
		}
		job.Duration = dur
	}

	segDur := ffmpeg.SegDuration()
	count := int(job.Duration / segDur)
	if job.Duration > float64(count)*segDur {
		count++
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int(segDur)))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	for i := 0; i < count; i++ {
		end := float64(i+1) * segDur
		d := segDur
		if end > job.Duration {
			d = job.Duration - float64(i)*segDur
		}
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\nseg_%05d.ts\n", d, i))
	}
	b.WriteString("#EXT-X-ENDLIST\n")

	if err := os.WriteFile(filepath.Join(job.CacheDir, "playlist.m3u8"), []byte(b.String()), 0644); err != nil {
		return fmt.Errorf("write playlist: %w", err)
	}

	job.PlaylistReady = true
	return nil
}

// PlaylistPath returns the filesystem path to the playlist, or empty if not ready.
func (m *Muxer) PlaylistPath(job *model.MuxJob) string {
	if job.CacheDir == "" || !job.PlaylistReady {
		return ""
	}
	return filepath.Join(job.CacheDir, "playlist.m3u8")
}

// SegmentPath returns the filesystem path for a cached segment, or empty if
// not yet generated.
func (m *Muxer) SegmentPath(job *model.MuxJob, segIndex int) string {
	if job.CacheDir == "" {
		return ""
	}
	p := filepath.Join(job.CacheDir, fmt.Sprintf("seg_%05d.ts", segIndex))
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// GenerateSegment generates a single HLS segment on-demand by seeking directly
// into the source(s) with ffmpeg -ss (HTTP Range). It only produces ~4s of
// content regardless of where the user seeks, so seek cost is constant.
//
// If the language-based audio map fails, it retries once with the first audio
// track and remembers the outcome on the job.
func (m *Muxer) GenerateSegment(ctx context.Context, job *model.MuxJob, segIndex int, out *os.File) error {
	offset := float64(segIndex) * ffmpeg.SegDuration()

	lang := job.TargetLanguage
	if job.PlaylistReady && !job.LangOK {
		lang = ""
	}

	videoURL := m.resolvedURL(ctx, job, "video")

	// Try the primary audio source first, then fall back through the ordered
	// candidates. Debrid sources sometimes return a short error video with no
	// audio track; we skip to the next candidate until one yields audio.
	audioURLs := append([]string{m.resolvedURL(ctx, job, "audio")}, job.AudioCandidates...)

	var lastErr error
	for i, audioURL := range audioURLs {
		if audioURL == "" {
			continue
		}
		err := m.ffmpeg.GenerateSegment(ctx, videoURL, audioURL, lang, offset, out)
		if err == nil {
			// Success. If we had to fall back, remember the working source so
			// subsequent segments don't re-try the broken one.
			if i > 0 {
				job.AudioURL = audioURL
				job.AudioResolved = audioURL
				job.AudioCandidates = nil
				log.Printf("mux: seg %d fell back to audio candidate %d", segIndex, i)
			}
			return nil
		}
		lastErr = err
		log.Printf("mux: audio source %d failed for seg %d: %v", i, segIndex, err)
		if truncErr := resetFile(out); truncErr != nil {
			return fmt.Errorf("reset output: %w", truncErr)
		}
	}

	// All audio sources failed. If we were using a language map, retry the
	// primary with the first audio track as a last resort.
	if lang != "" {
		log.Printf("mux: all audio sources failed for seg %d, retrying first track: %v", segIndex, lastErr)
		if truncErr := resetFile(out); truncErr != nil {
			return fmt.Errorf("reset output: %w", truncErr)
		}
		err := m.ffmpeg.GenerateSegment(ctx, videoURL, m.resolvedURL(ctx, job, "audio"), "", offset, out)
		if err == nil {
			job.LangOK = false
			return nil
		}
		lastErr = err
	}

	return lastErr
}

// resolvedURL returns the final CDN URL for a job source, resolving the addon's
// redirect chain once and caching the result on the job. Addon URLs point at a
// debrid proxy that re-resolves the torrent on every request (slow, rate-
// limited); the final CDN URL supports HTTP Range and answers in milliseconds.
//
// If resolution fails, the original URL is returned so playback still works,
// just slower.
func (m *Muxer) resolvedURL(ctx context.Context, job *model.MuxJob, which string) string {
	var cached *string
	var original string
	switch which {
	case "video":
		cached, original = &job.VideoResolved, job.VideoURL
	case "audio":
		cached, original = &job.AudioResolved, job.AudioURL
	default:
		return ""
	}

	if *cached != "" {
		return *cached
	}

	m.resolveMu.Lock()
	defer m.resolveMu.Unlock()

	// Re-check under the lock: another request may have resolved it already.
	if *cached != "" {
		return *cached
	}

	final := m.followRedirects(ctx, original)
	if final == "" {
		return original
	}
	*cached = final
	return final
}

// followRedirects issues a range request and returns the final URL after
// following redirects, or "" on failure.
func (m *Muxer) followRedirects(ctx context.Context, url string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Range", "bytes=0-0")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1))

	if resp.Request == nil || resp.Request.URL == nil {
		return ""
	}
	return resp.Request.URL.String()
}

func resetFile(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err := f.Seek(0, 0)
	return err
}

// CleanupJob removes the temp cache directory for a job.
func (m *Muxer) CleanupJob(job *model.MuxJob) {
	if job.CacheDir != "" {
		os.RemoveAll(job.CacheDir)
	}
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