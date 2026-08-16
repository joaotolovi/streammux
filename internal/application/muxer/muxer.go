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

	// segMu guards segLocks.
	segMu sync.Mutex

	// segLocks are per-(job, segment) mutexes used as singleflight: concurrent
	// requests for the same segment serialize on the same lock, so only one
	// ffmpeg writes the .tmp file. The others wait, then serve the cached file.
	segLocks map[string]*sync.Mutex
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
		segLocks: make(map[string]*sync.Mutex),
	}
}

// SegmentLock returns the singleflight mutex for a (job, segment) pair so that
// concurrent requests for the same segment generate it only once.
func (m *Muxer) SegmentLock(jobID string, segIndex int) *sync.Mutex {
	key := fmt.Sprintf("%s:%05d", jobID, segIndex)
	m.segMu.Lock()
	defer m.segMu.Unlock()
	l, ok := m.segLocks[key]
	if !ok {
		l = &sync.Mutex{}
		m.segLocks[key] = l
	}
	return l
}

// ReleaseSegmentLocks drops the singleflight locks for a job (called on job
// cleanup to avoid unbounded growth).
func (m *Muxer) ReleaseSegmentLocks(jobID string) {
	prefix := jobID + ":"
	m.segMu.Lock()
	for k := range m.segLocks {
		if strings.HasPrefix(k, prefix) {
			delete(m.segLocks, k)
		}
	}
	m.segMu.Unlock()
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
				VideoCandidates:  m.videoCandidates(streams, videoURL),
				AudioCandidates:  m.audioCandidates(streams, cfg.Language),
				AudioTrackIndex:  -1,
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
// Limited to a few candidates to avoid hammering the debrid proxy with a probe
// per stream (which is what triggered rate-limits).
func (m *Muxer) audioCandidates(streams []model.CollectedStream, targetLanguage string) []string {
	ranked := m.analyzer.RankAudio(streams, targetLanguage)
	var out []string
	seen := map[string]bool{}
	for i := range ranked {
		u := ranked[i].Stream.Stream.URL
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		// Skip the primary (first) candidate — it's already job.AudioURL.
		if i == 0 {
			continue
		}
		out = append(out, u)
		if len(out) >= 2 {
			break
		}
	}
	return out
}

// videoCandidates returns the ordered list of video source URLs (best first),
// excluding the primary choice. Used as a fallback when the primary video is a
// broken debrid response (e.g. a short trailer instead of the movie).
// Limited to a few candidates to avoid hammering the debrid proxy with a probe
// per stream (which is what triggered rate-limits).
func (m *Muxer) videoCandidates(streams []model.CollectedStream, primaryURL string) []string {
	ranked := m.analyzer.RankVideo(streams)
	var out []string
	seen := map[string]bool{}
	for i := range ranked {
		u := ranked[i].Stream.Stream.URL
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		if u == primaryURL {
			continue
		}
		out = append(out, u)
		if len(out) >= 2 {
			break
		}
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

	// Pick a valid video source and probe its duration once. Debrid proxies
	// sometimes return a short trailer/preview instead of the real file; if the
	// probed duration is implausibly short for a movie/series (or probing
	// fails), fall back through the ordered video candidates. Each candidate is
	// resolved to its CDN URL before probing.
	//
	// We also check whether the source's sustained throughput can keep up with
	// the chosen video's bitrate. If not, we walk down the quality list to the
	// best source that the connection can actually sustain — a 4K REMUX that a
	// slow CDN can't deliver in real time would otherwise freeze playback.
	if job.Duration == 0 {
		dur, chosen := m.pickVideoSource(ctx, job)
		job.Duration = dur
		if chosen != "" && chosen != job.VideoURL {
			job.VideoURL = chosen
			job.VideoResolved = ""
			log.Printf("mux: video switched to %s", truncate(chosen))
		}
	}

	// Resolve the target-language audio source once, by probing each candidate
	// and picking the best one that is a real file (not a broken debrid
	// response like static/500.mp4) and yields an audio track. Validating here
	// (not during every segment) avoids stalling generation on a broken source.
	if job.AudioTrackIndex < 0 && job.AudioURL != "" {
		chosen, trackIdx := m.pickAudioSource(ctx, job)
		if chosen != "" {
			job.AudioURL = chosen
			job.AudioResolved = ""
			job.AudioTrackIndex = trackIdx
			log.Printf("mux: audio source chosen for job %s", truncate(chosen))
		} else {
			log.Printf("mux: no valid audio source for job %s, using primary", job.ID)
			job.AudioTrackIndex = 0
		}
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
// The audio track index is resolved once at playlist time; segments map it by
// numeric index, which avoids scanning language metadata and keeps the time to
// first byte low. If the primary audio source is a broken debrid response (no
// audio track), the segment falls back through the ordered candidates.
func (m *Muxer) GenerateSegment(ctx context.Context, job *model.MuxJob, segIndex int, out *os.File) error {
	offset := float64(segIndex) * ffmpeg.SegDuration()

	audioTrack := job.AudioTrackIndex
	if audioTrack < 0 {
		audioTrack = 0
	}

	// Build the list of video sources to try: the primary plus fallback
	// candidates, each resolved to its CDN URL.
	videoSources := append([]string{job.VideoURL}, job.VideoCandidates...)
	var videoURLs []string
	for i, src := range videoSources {
		if src == "" {
			continue
		}
		var resolved string
		if i == 0 {
			resolved = m.resolvedURL(ctx, job, "video")
		} else {
			resolved = m.resolveOne(ctx, src)
		}
		if resolved != "" {
			videoURLs = append(videoURLs, resolved)
		}
	}

	// Build the list of audio sources to try: the primary plus fallback
	// candidates, each resolved to its CDN URL. The primary is pre-validated at
	// playlist time, but it can still die mid-playback (token expiry, CDN drop),
	// so we fall back through the candidates if needed.
	audioSources := append([]string{job.AudioURL}, job.AudioCandidates...)
	var audioURLs []string
	for i, src := range audioSources {
		if src == "" {
			continue
		}
		var resolved string
		if i == 0 {
			resolved = m.resolvedURL(ctx, job, "audio")
		} else {
			resolved = m.resolveOne(ctx, src)
		}
		if resolved != "" {
			audioURLs = append(audioURLs, resolved)
		}
	}

	if len(videoURLs) == 0 || len(audioURLs) == 0 {
		return fmt.Errorf("no resolvable video (%d) or audio (%d) sources", len(videoURLs), len(audioURLs))
	}

	log.Printf("mux: seg %d video=%s audioTrack=%d audioSrc=%d", segIndex, truncate(videoURLs[0]), audioTrack, len(audioURLs))

	var lastErr error
	// Try each video source against each audio source (best first).
	for vi, videoURL := range videoURLs {
		for ai, audioURL := range audioURLs {
			err := m.ffmpeg.GenerateSegment(ctx, videoURL, audioURL, audioTrack, offset, out)
			if err == nil {
				if vi > 0 {
					job.VideoURL = videoSources[vi]
					job.VideoResolved = ""
					job.VideoCandidates = nil
					log.Printf("mux: seg %d fell back to video candidate %d", segIndex, vi)
				}
				if ai > 0 {
					job.AudioURL = audioSources[ai]
					job.AudioResolved = ""
					job.AudioCandidates = nil
					log.Printf("mux: seg %d fell back to audio candidate %d", segIndex, ai)
				}
				return nil
			}
			lastErr = err
			log.Printf("mux: (v%d,a%d) failed for seg %d: %v", vi, ai, segIndex, err)
			if truncErr := resetFile(out); truncErr != nil {
				return fmt.Errorf("reset output: %w", truncErr)
			}
		}
	}

	return lastErr
}

// pickAudioSource walks the audio candidates and returns the first source that
// is a real file (probe succeeds) and contains an audio track. It also returns
// the numeric index of the target-language track (falling back to 0). Debrid
// proxies sometimes return a broken response (e.g. static/500.mp4) that would
// otherwise stall every segment's ffmpeg run until timeout.
func (m *Muxer) pickAudioSource(ctx context.Context, job *model.MuxJob) (string, int) {
	code := ffmpeg.LanguageCode(job.TargetLanguage)

	sources := append([]string{job.AudioURL}, job.AudioCandidates...)
	for i, u := range sources {
		if u == "" {
			continue
		}
		var resolved string
		if i == 0 {
			resolved = m.resolvedURL(ctx, job, "audio")
		} else {
			resolved = m.resolveOne(ctx, u)
		}
		if resolved == "" {
			log.Printf("mux: audio source %d failed to resolve, skipping", i)
			continue
		}

		res, err := m.ffmpeg.Probe(ctx, resolved)
		if err != nil {
			log.Printf("mux: audio probe %d failed: %v", i, err)
			continue
		}
		if len(res.AudioTracks) == 0 {
			log.Printf("mux: audio source %d has no audio track, skipping", i)
			continue
		}

		idx := ffmpeg.AudioTrackIndexByLanguage(res.AudioTracks, code)
		if idx < 0 {
			idx = 0
		}
		if i > 0 {
			log.Printf("mux: audio fell back to candidate %d (track %d)", i, idx)
		}
		return u, idx
	}
	return "", -1
}

// pickVideoSource walks the video quality list (primary first, then candidates
// in ranked order) and returns the duration and best source URL that is both a
// real file (duration plausible) and sustainable by the source's throughput.
//
// Debrid proxies sometimes return a short trailer instead of the movie, and a
// 4K REMUX may need more bandwidth than a slow CDN can deliver in real time —
// both would freeze playback. So we skip broken/short sources and drop down the
// quality ladder until the chosen stream fits the measured throughput. If no
// source is sustainable we still return the best valid one (better to play than
// nothing), falling back to 7200s duration when nothing probes.
func (m *Muxer) pickVideoSource(ctx context.Context, job *model.MuxJob) (float64, string) {
	const minDuration = 600 // 10 minutes; a real movie/series is far longer
	const sustainFactor = 1.3

	sources := append([]string{job.VideoURL}, job.VideoCandidates...)

	best := struct {
		dur    float64
		resolved string
	}{}

	// Throughput is measured once (on the first resolvable source) and reused
	// for every candidate — measuring it per candidate multiplied the number of
	// debrid requests and triggered rate-limits.
	throughput := 0.0

	for i, u := range sources {
		if u == "" {
			continue
		}
		var resolved string
		if i == 0 {
			resolved = m.resolvedURL(ctx, job, "video")
		} else {
			resolved = m.resolveOne(ctx, u)
		}
		if resolved == "" {
			log.Printf("mux: video source %d failed to resolve, skipping", i)
			continue
		}

		res, err := m.ffmpeg.Probe(ctx, resolved)
		if err != nil {
			log.Printf("mux: video probe %d failed: %v", i, err)
			continue
		}
		if res.Duration < minDuration {
			log.Printf("mux: video candidate %d too short (%.0fs), skipping", i, res.Duration)
			continue
		}

		// Remember the best valid source in case none is sustainable.
		if best.resolved == "" {
			best.dur = res.Duration
			best.resolved = resolved
		}

		if throughput == 0 {
			throughput = m.measureThroughput(ctx, resolved)
			log.Printf("mux: source throughput %.1f Mbps", throughput/1e6)
		}

		if res.VideoBitrate > 0 && throughput > 0 && throughput >= res.VideoBitrate*sustainFactor {
			log.Printf("mux: video candidate %d sustainable (%.1f Mbps > %.1f Mbps)", i, throughput/1e6, res.VideoBitrate/1e6)
			return res.Duration, resolved
		}
		if throughput > 0 {
			log.Printf("mux: video candidate %d too fast for source (%.1f Mbps needed, %.1f Mbps available), trying next", i, res.VideoBitrate/1e6, throughput/1e6)
		}
	}

	if best.resolved != "" {
		log.Printf("mux: no sustainable video, using best valid source")
		return best.dur, best.resolved
	}

	log.Printf("mux: no valid video source, using 7200s fallback")
	return 7200, ""
}

// measureThroughput downloads a small sample from the source and returns the
// sustained rate in bytes/second, or 0 on failure.
func (m *Muxer) measureThroughput(ctx context.Context, url string) float64 {
	if url == "" {
		return 0
	}
	const sampleBytes = 2 * 1024 * 1024

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", sampleBytes-1))

	start := time.Now()
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, sampleBytes))
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 || n == 0 {
		return 0
	}
	return float64(n) / elapsed
}

// resolveOne follows the redirect chain of a single URL and returns the final
// CDN URL, or "" on failure. No caching — callers decide whether to cache.
func (m *Muxer) resolveOne(ctx context.Context, url string) string {
	if url == "" {
		return ""
	}
	return m.followRedirects(ctx, url)
}

// resolvedURL returns the final CDN URL for a job source, resolving the addon's
// redirect chain once and caching the result on the job. Addon URLs point at a
// debrid proxy that re-resolves the torrent on every request (slow, rate-
// limited); the final CDN URL supports HTTP Range and answers in milliseconds.
//
// On failure it returns "" (never the raw addon URL) so callers skip the source
// or fail fast instead of handing ffmpeg a URL that will stall on the redirect
// chain.
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
		return ""
	}
	*cached = final
	return final
}

// followRedirects issues a range request and returns the final URL after
// following redirects, or "" on failure. Transient errors (429/5xx) are
// retried a few times with a short backoff, since debrid proxies are flaky.
func (m *Muxer) followRedirects(ctx context.Context, url string) string {
	const attempts = 3
	for i := 0; i < attempts; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return ""
		}
		req.Header.Set("Range", "bytes=0-0")

		resp, err := m.httpClient.Do(req)
		if err != nil {
			if i < attempts-1 {
				if !sleepCtx(ctx, time.Duration(i+1)*time.Second) {
					return ""
				}
				continue
			}
			return ""
		}

		io.Copy(io.Discard, io.LimitReader(resp.Body, 1))
		resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			if i < attempts-1 {
				if !sleepCtx(ctx, time.Duration(i+1)*time.Second) {
					return ""
				}
				continue
			}
			return ""
		}

		if resp.Request == nil || resp.Request.URL == nil {
			return ""
		}
		return resp.Request.URL.String()
	}
	return ""
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
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
	m.ReleaseSegmentLocks(job.ID)
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