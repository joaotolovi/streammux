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

// SegmentLock returns the singleflight mutex for a (job, kind, segment) key so
// that concurrent requests for the same segment generate it only once.
func (m *Muxer) SegmentLock(key string) *sync.Mutex {
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

// muxURL builds the absolute URL of the master HLS playlist for a job.
func (m *Muxer) muxURL(jobID string) string {
	u := "/mux/" + jobID + "/master.m3u8"
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
	seen := map[string]bool{}
	for i := range ranked {
		u := ranked[i].Stream.Stream.URL
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		log.Printf("mux: audio candidate %d: %s (size=%d)", i, truncate(u), ranked[i].Stream.Size)
		// Skip the primary (first) candidate — it's already job.AudioURL.
		if i == 0 {
			continue
		}
		out = append(out, u)
	}
	return out
}

// videoCandidates returns the ordered list of video source URLs (best first),
// excluding the primary choice. Used as a fallback when the primary video is a
// broken debrid response (e.g. a short trailer instead of the movie).
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
	if job.Duration == 0 {
		videoURL := m.resolvedURL(ctx, job, "video")
		dur, chosen := m.probeValidDuration(ctx, videoURL, job.VideoCandidates)
		job.Duration = dur
		if chosen != "" && chosen != job.VideoURL {
			job.VideoURL = chosen
			job.VideoResolved = chosen
			log.Printf("mux: video switched to %s", truncate(chosen))
		}
	}

	// Resolve the target-language audio track index once, by probing the audio
	// source. This lets every segment map the audio by numeric index instead of
	// scanning language metadata, which is what keeps the time to first byte low.
	if job.AudioTrackIndex < 0 && job.AudioURL != "" {
		audioURL := m.resolvedURL(ctx, job, "audio")
		res, err := m.ffmpeg.Probe(ctx, audioURL)
		if err == nil && res != nil {
			code := ffmpeg.LanguageCode(job.TargetLanguage)
			idx := ffmpeg.AudioTrackIndexByLanguage(res.AudioTracks, code)
			if idx < 0 {
				// No matching track; fall back to the first audio track.
				idx = 0
			}
			job.AudioTrackIndex = idx
		} else {
			log.Printf("mux: audio probe failed for job %s, using first track: %v", job.ID, err)
			job.AudioTrackIndex = 0
		}
	}

	segDur := ffmpeg.SegDuration()
	count := int(job.Duration / segDur)
	if job.Duration > float64(count)*segDur {
		count++
	}

	// Generate three static playlists by code:
	//   master.m3u8 — references video.m3u8 and audio.m3u8 via #EXT-X-MEDIA so
	//                 the player muxes them itself (no server-side remux)
	//   video.m3u8  — the best-quality video segments (v_00000.ts …)
	//   audio.m3u8  — the target-language audio segments (a_00000.ts …)
	media := func(prefix string) string {
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
			b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n%s_%05d.ts\n", d, prefix, i))
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		return b.String()
	}

	videoPlaylist := media("v")
	audioPlaylist := media("a")

	var master strings.Builder
	master.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	master.WriteString("#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"Dublado\",DEFAULT=YES,AUTOSELECT=YES,URI=\"audio.m3u8\"\n")
	master.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=20000000,CODECS=\"avc1,mp4a\",AUDIO=\"aud\"\n"))
	master.WriteString("video.m3u8\n")

	files := map[string]string{
		"master.m3u8": master.String(),
		"video.m3u8":  videoPlaylist,
		"audio.m3u8":  audioPlaylist,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(job.CacheDir, name), []byte(content), 0644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	job.PlaylistReady = true
	return nil
}

// PlaylistPath returns the filesystem path to the master playlist, or empty if
// not ready.
func (m *Muxer) PlaylistPath(job *model.MuxJob) string {
	if job.CacheDir == "" || !job.PlaylistReady {
		return ""
	}
	return filepath.Join(job.CacheDir, "master.m3u8")
}

// VideoSegmentPath returns the filesystem path for a cached video segment.
func (m *Muxer) VideoSegmentPath(job *model.MuxJob, segIndex int) string {
	if job.CacheDir == "" {
		return ""
	}
	p := filepath.Join(job.CacheDir, fmt.Sprintf("v_%05d.ts", segIndex))
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// AudioSegmentPath returns the filesystem path for a cached audio segment.
func (m *Muxer) AudioSegmentPath(job *model.MuxJob, segIndex int) string {
	if job.CacheDir == "" {
		return ""
	}
	p := filepath.Join(job.CacheDir, fmt.Sprintf("a_%05d.ts", segIndex))
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// GenerateVideoSegment generates a single video HLS segment on-demand by
// seeking directly into the video source with ffmpeg -ss (HTTP Range). It only
// produces ~4s of content regardless of where the user seeks.
func (m *Muxer) GenerateVideoSegment(ctx context.Context, job *model.MuxJob, segIndex int, out *os.File) error {
	offset := float64(segIndex) * ffmpeg.SegDuration()

	videoURL := m.resolvedURL(ctx, job, "video")
	if videoURL == "" {
		return fmt.Errorf("no resolvable video source")
	}

	err := m.ffmpeg.GenerateVideoSegment(ctx, videoURL, offset, out)
	if err == nil {
		return nil
	}

	// Try fallback video candidates.
	for i, src := range job.VideoCandidates {
		if src == "" {
			continue
		}
		resolved := m.resolveOne(ctx, src)
		if resolved == "" {
			continue
		}
		if truncErr := resetFile(out); truncErr != nil {
			return fmt.Errorf("reset output: %w", truncErr)
		}
		err = m.ffmpeg.GenerateVideoSegment(ctx, resolved, offset, out)
		if err == nil {
			job.VideoURL = src
			job.VideoResolved = ""
			job.VideoCandidates = nil
			log.Printf("mux: video candidate %d ok for seg %d", i, segIndex)
			return nil
		}
	}
	return err
}

// GenerateAudioSegment generates a single audio HLS segment (target-language
// track) on-demand by seeking into the audio source. It falls back through the
// audio candidates if the primary is a broken debrid response.
func (m *Muxer) GenerateAudioSegment(ctx context.Context, job *model.MuxJob, segIndex int, out *os.File) error {
	offset := float64(segIndex) * ffmpeg.SegDuration()

	audioTrack := job.AudioTrackIndex
	if audioTrack < 0 {
		audioTrack = 0
	}

	audioURL := m.resolvedURL(ctx, job, "audio")
	if audioURL == "" {
		return fmt.Errorf("no resolvable audio source")
	}

	err := m.ffmpeg.GenerateAudioSegment(ctx, audioURL, audioTrack, offset, out)
	if err == nil {
		return nil
	}

	for i, src := range job.AudioCandidates {
		if src == "" {
			continue
		}
		resolved := m.resolveOne(ctx, src)
		if resolved == "" {
			continue
		}
		if truncErr := resetFile(out); truncErr != nil {
			return fmt.Errorf("reset output: %w", truncErr)
		}
		err = m.ffmpeg.GenerateAudioSegment(ctx, resolved, audioTrack, offset, out)
		if err == nil {
			job.AudioURL = src
			job.AudioResolved = ""
			job.AudioCandidates = nil
			log.Printf("mux: audio candidate %d ok for seg %d", i, segIndex)
			return nil
		}
	}
	return err
}

// probeValidDuration probes a video source for its duration, walking the
// fallback candidates when the primary is implausibly short (a debrid proxy
// sometimes returns a trailer/preview instead of the real file) or probing
// fails. Each candidate is resolved to its CDN URL before probing. It returns
// the duration and the URL that produced it ("" means the fallback was used).
func (m *Muxer) probeValidDuration(ctx context.Context, primary string, candidates []string) (float64, string) {
	const minDuration = 600 // 10 minutes; a real movie/series is far longer

	// primary is already resolved to its CDN URL by the caller; only fallback
	// candidates still need resolving.
	urls := append([]string{primary}, candidates...)
	for i, u := range urls {
		if u == "" {
			continue
		}
		resolved := u
		if i > 0 {
			resolved = m.resolveOne(ctx, u)
			if resolved == "" {
				log.Printf("mux: video source %d failed to resolve, skipping", i)
				continue
			}
		}
		res, err := m.ffmpeg.Probe(ctx, resolved)
		if err != nil {
			log.Printf("mux: video probe %d failed: %v", i, err)
			continue
		}
		if res.Duration >= minDuration {
			if i > 0 {
				log.Printf("mux: video fell back to candidate %d (duration %.0fs)", i, res.Duration)
			}
			return res.Duration, resolved
		}
		log.Printf("mux: video candidate %d too short (%.0fs), skipping", i, res.Duration)
	}

	log.Printf("mux: no valid video source, using 7200s fallback")
	return 7200, ""
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