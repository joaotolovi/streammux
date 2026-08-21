package muxer

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/streammux/streammux/internal/application/analyzer"
	"github.com/streammux/streammux/internal/application/ffmpeg"
	"github.com/streammux/streammux/internal/domain/model"
)

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

const (
	resolvedTTL       = 10 * time.Minute
	probeTTL          = 6 * time.Hour
	negativeSourceTTL = 20 * time.Second
)

type resolvedEntry struct {
	url       string
	expiresAt time.Time
	err       error
}

type resolveFlight struct {
	done chan struct{}
	url  string
	err  error
}

type probeEntry struct {
	result    *ffmpeg.ProbeResult
	expiresAt time.Time
}

type probeFlight struct {
	done   chan struct{}
	result *ffmpeg.ProbeResult
	err    error
}

type preparedPlan struct {
	plan            model.PlaybackPlan
	videoURL        string
	audioURL        string
	videoTrackIndex int
	audioTrackIndex int
	audioMode       ffmpeg.AudioMode
	duration        float64
	// videoIdx/audioIdx identify the composer candidates used, for logging.
	videoIdx int
	audioIdx int
	// videoBitrate is the measured peak bitrate of the video source in bits/s
	// (from ffprobe). Zero when the source does not report it.
	videoBitrate float64
	videoWidth   int
	videoHeight  int
	videoCodec   string
	// videoAudioTracks are the audio tracks of the video source, used for A/V
	// offset estimation on dual-source plans.
	videoAudioTracks []ffmpeg.AudioTrack
	// transcode, when non-nil, re-encodes the video on the fly instead of
	// stream-copying it. Set by ABR downgrade-ladder strategies.
	transcode *ffmpeg.TranscodeSpec
}

func (m *Muxer) preparePlan(ctx context.Context, job *model.MuxJob, plan model.PlaybackPlan) (*preparedPlan, error) {
	return m.preparePlanMode(ctx, job, plan, false)
}

// audioOffsetKey identifies a source pair so the A/V offset estimate is reused
// across attempts and recoveries.
func audioOffsetKey(plan model.PlaybackPlan) string {
	return plan.Video.SourceKey() + "\x00" + plan.Audio.SourceKey()
}

// audioOffsetFor returns the cached audio offset for the plan's source pair.
func (m *Muxer) audioOffsetFor(plan model.PlaybackPlan) (time.Duration, bool) {
	m.offsetMu.Lock()
	defer m.offsetMu.Unlock()
	offset, ok := m.offsets[audioOffsetKey(plan)]
	return offset, ok
}

func (m *Muxer) cacheAudioOffset(plan model.PlaybackPlan, offset time.Duration) {
	m.offsetMu.Lock()
	defer m.offsetMu.Unlock()
	m.offsets[audioOffsetKey(plan)] = offset
}

// detectOffset finds the audio alignment between the dub and the video source.
// All video candidate tracks are decoded in a single fetch (see
// ffmpeg.ExtractPCMMulti) and correlated against the dub; the track with the
// sharpest, most isolated correlation peak provides the offset.
func (m *Muxer) detectOffset(prepared *preparedPlan) (time.Duration, int, float64, error) {
	if len(prepared.videoAudioTracks) == 0 {
		return 0, -1, 0, fmt.Errorf("no audio tracks on video source")
	}
	return m.ffmpeg.DetectAudioOffset(
		prepared.videoURL,
		prepared.audioURL,
		prepared.videoAudioTracks,
		prepared.audioTrackIndex,
		ffmpeg.SyncSeconds,
	)
}

func (m *Muxer) preparePlanMode(ctx context.Context, job *model.MuxJob, plan model.PlaybackPlan, lenient bool) (*preparedPlan, error) {
	videoURL, audioURL, err := m.resolvePlanSources(ctx, job, plan)
	if err != nil {
		return nil, err
	}

	prepared := &preparedPlan{
		plan:            plan,
		videoURL:        videoURL,
		audioURL:        audioURL,
		videoTrackIndex: 0,
		audioTrackIndex: 0,
		audioMode:       ffmpeg.AudioModeCopy,
	}

	// The final subtitled fallback intentionally skips ffprobe. It is the cheapest
	// path and lets FFmpeg attempt the normal first video/audio streams directly.
	if !plan.HasTargetAudio {
		return prepared, nil
	}

	videoProbe, audioProbe, err := m.probePlanSources(ctx, plan, videoURL, audioURL)
	if err != nil {
		return nil, err
	}
	if len(videoProbe.VideoStreams) == 0 {
		return nil, fmt.Errorf("source has no video stream")
	}

	trackIndex := targetAudioTrackStrict(audioProbe.AudioTracks, job.TargetLanguage, plan.Audio, lenient)
	if trackIndex < 0 {
		return nil, fmt.Errorf("source has no confirmed %s audio track", job.TargetLanguage)
	}

	if !plan.SingleSource() {
		if err := compatibleReleases(plan, videoProbe, audioProbe, m.policy.DurationTolerance); err != nil {
			return nil, err
		}
	}

	prepared.videoTrackIndex = videoProbe.VideoStreams[0].Index
	prepared.audioTrackIndex = trackIndex
	prepared.duration = videoProbe.Duration
	prepared.videoBitrate = videoProbe.VideoBitrate
	if len(videoProbe.VideoStreams) > 0 {
		prepared.videoWidth = videoProbe.VideoStreams[0].Width
		prepared.videoHeight = videoProbe.VideoStreams[0].Height
		prepared.videoCodec = videoProbe.VideoStreams[0].Codec
	}
	prepared.videoAudioTracks = videoProbe.AudioTracks
	for _, track := range audioProbe.AudioTracks {
		if track.Index == trackIndex {
			prepared.audioMode = compatibleAudioMode(track.Codec)
			break
		}
	}
	return prepared, nil
}

func (m *Muxer) resolvePlanSources(ctx context.Context, job *model.MuxJob, plan model.PlaybackPlan) (string, string, error) {
	type result struct {
		kind string
		url  string
		err  error
	}

	results := make(chan result, 2)
	go func() {
		url, err := m.resolveSource(ctx, job, plan.Video)
		results <- result{kind: "video", url: url, err: err}
	}()

	if plan.SingleSource() {
		item := <-results
		if item.err != nil {
			return "", "", fmt.Errorf("resolve video: %w", item.err)
		}
		return item.url, item.url, nil
	}

	go func() {
		url, err := m.resolveSource(ctx, job, plan.Audio)
		results <- result{kind: "audio", url: url, err: err}
	}()

	var videoURL, audioURL string
	for range 2 {
		item := <-results
		if item.err != nil {
			return "", "", fmt.Errorf("resolve %s: %w", item.kind, item.err)
		}
		if item.kind == "video" {
			videoURL = item.url
		} else {
			audioURL = item.url
		}
	}
	return videoURL, audioURL, nil
}

func (m *Muxer) probePlanSources(ctx context.Context, plan model.PlaybackPlan, videoURL, audioURL string) (*ffmpeg.ProbeResult, *ffmpeg.ProbeResult, error) {
	if plan.SingleSource() {
		result, err := m.probeSource(ctx, videoURL)
		if err != nil {
			return nil, nil, fmt.Errorf("probe source: %w", err)
		}
		return result, result, nil
	}

	type result struct {
		kind  string
		probe *ffmpeg.ProbeResult
		err   error
	}
	results := make(chan result, 2)
	go func() {
		probe, err := m.probeSource(ctx, videoURL)
		results <- result{kind: "video", probe: probe, err: err}
	}()
	go func() {
		probe, err := m.probeSource(ctx, audioURL)
		results <- result{kind: "audio", probe: probe, err: err}
	}()

	var videoProbe, audioProbe *ffmpeg.ProbeResult
	for range 2 {
		item := <-results
		if item.err != nil {
			return nil, nil, fmt.Errorf("probe %s: %w", item.kind, item.err)
		}
		if item.kind == "video" {
			videoProbe = item.probe
		} else {
			audioProbe = item.probe
		}
	}
	return videoProbe, audioProbe, nil
}

func (m *Muxer) resolveSource(ctx context.Context, job *model.MuxJob, source model.CollectedStream) (string, error) {
	key := source.SourceKey()
	now := time.Now()

	m.cacheMu.Lock()
	if entry, ok := m.resolved[key]; ok && now.Before(entry.expiresAt) {
		m.cacheMu.Unlock()
		return entry.url, entry.err
	}
	if flight, ok := m.resolveFlights[key]; ok {
		m.cacheMu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-flight.done:
			return flight.url, flight.err
		}
	}
	flight := &resolveFlight{done: make(chan struct{})}
	m.resolveFlights[key] = flight
	m.cacheMu.Unlock()

	url, err := m.resolveSourceUncached(ctx, job, source)
	ttl := resolvedTTL
	if err != nil {
		ttl = negativeSourceTTL
	}

	m.cacheMu.Lock()
	flight.url = url
	flight.err = err
	m.resolved[key] = resolvedEntry{url: url, err: err, expiresAt: time.Now().Add(ttl)}
	delete(m.resolveFlights, key)
	close(flight.done)
	m.cacheMu.Unlock()
	return url, err
}

func (m *Muxer) resolveSourceUncached(ctx context.Context, job *model.MuxJob, source model.CollectedStream) (string, error) {
	original := strings.TrimSpace(source.Stream.URL)
	if original == "" && source.Stream.InfoHash != "" {
		if m.resolver == nil {
			return "", fmt.Errorf("no resolver configured for torrent source")
		}
		filename := source.Stream.Name
		if filename == "" {
			filename = source.Stream.Title
		}
		resolved, _ := m.resolver.Resolve(ctx, &job.Config, source.Stream.InfoHash, filename)
		original = resolved
	}
	if original == "" {
		return "", fmt.Errorf("source has no resolvable URL")
	}
	return m.followRedirects(ctx, original)
}

func (m *Muxer) followRedirects(ctx context.Context, url string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Range", "bytes=0-0")
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("User-Agent", browserUA)

		resp, err := m.httpClient.Do(req)
		if err != nil {
			lastErr = err
		} else {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent {
				return resp.Request.URL.String(), nil
			}
			lastErr = fmt.Errorf("source returned HTTP %d", resp.StatusCode)
			if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				break
			}
		}

		if attempt == 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	return "", lastErr
}

func (m *Muxer) probeSource(ctx context.Context, url string) (*ffmpeg.ProbeResult, error) {
	now := time.Now()
	m.cacheMu.Lock()
	if entry, ok := m.probes[url]; ok && now.Before(entry.expiresAt) {
		m.cacheMu.Unlock()
		return entry.result, nil
	}
	if flight, ok := m.probeFlights[url]; ok {
		m.cacheMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-flight.done:
			return flight.result, flight.err
		}
	}
	flight := &probeFlight{done: make(chan struct{})}
	m.probeFlights[url] = flight
	m.cacheMu.Unlock()

	result, err := m.ffmpeg.Probe(ctx, url)

	m.cacheMu.Lock()
	flight.result = result
	flight.err = err
	if err == nil {
		m.probes[url] = probeEntry{result: result, expiresAt: time.Now().Add(probeTTL)}
	}
	delete(m.probeFlights, url)
	close(flight.done)
	m.cacheMu.Unlock()
	return result, err
}

func targetAudioTrack(tracks []ffmpeg.AudioTrack, targetLanguage string, source model.CollectedStream) int {
	return targetAudioTrackStrict(tracks, targetLanguage, source, false)
}

// targetAudioTrackStrict picks the audio track for a dubbed source.
//
// Strict mode (lenient=false) only accepts a track it can be confident about:
// an explicit language tag, a title mentioning the language, or a single-track
// source whose track tag does not contradict the target language.
//
// Lenient mode (lenient=true) is the end of the queue: it also accepts an
// undefined or untagged track from a dubbed source, since many "DUAL" remuxes
// tag the dubbed audio as `und` or leave it empty. It is only used after every
// strict plan has failed.
func targetAudioTrackStrict(tracks []ffmpeg.AudioTrack, targetLanguage string, source model.CollectedStream, lenient bool) int {
	code := ffmpeg.LanguageCode(targetLanguage)
	if index := ffmpeg.AudioTrackIndexByLanguage(tracks, code); index >= 0 {
		return index
	}

	for _, track := range tracks {
		if titleMatchesLanguage(track.Title, targetLanguage) {
			return track.Index
		}
	}

	// A source identified as dubbed with a single audio track. Accept the
	// track only when its own tag does not contradict the target language:
	//   - an explicit foreign-language tag (e.g. "eng" on a source flagged as
	//     Portuguese) is authoritative — the addon flag is wrong and the source
	//     is rejected rather than played in the wrong language;
	//   - an untagged/und track is not rejected, merely deferred: strict mode
	//     returns -1 so the composer tries confirmed sources first, and the
	//     lenient pass (the end of the queue) picks it up last.
	if len(tracks) == 1 && analyzer.MatchesLanguage(source, targetLanguage) {
		lang := strings.TrimSpace(tracks[0].Language)
		if lang == "" || lang == "und" {
			if lenient {
				return tracks[0].Index
			}
			return -1
		}
		if ffmpeg.LanguageCode(lang) == code {
			return tracks[0].Index
		}
		return -1
	}

	// Lenient: accept an undefined/empty track from a dubbed multiaudio source.
	if lenient && analyzer.MatchesLanguage(source, targetLanguage) {
		for _, track := range tracks {
			lang := strings.TrimSpace(track.Language)
			if lang == "" || lang == "und" {
				return track.Index
			}
		}
	}
	return -1
}

func titleMatchesLanguage(title, target string) bool {
	title = strings.ToLower(title)
	target = strings.ToLower(target)
	switch {
	case strings.Contains(target, "portug"):
		return strings.Contains(title, "portug") || strings.Contains(title, "brazil") || strings.Contains(title, "brasil") || strings.Contains(title, "pt-br")
	case strings.Contains(target, "english"):
		return strings.Contains(title, "english") || strings.Contains(title, "original")
	case strings.Contains(target, "spanish"):
		return strings.Contains(title, "spanish") || strings.Contains(title, "español")
	case strings.Contains(target, "french"):
		return strings.Contains(title, "french") || strings.Contains(title, "français")
	default:
		return target != "" && strings.Contains(title, target)
	}
}

func compatibleReleases(plan model.PlaybackPlan, video, audio *ffmpeg.ProbeResult, ratio float64) error {
	if plan.Video.Parsed.Edition != "" && plan.Audio.Parsed.Edition != "" && plan.Video.Parsed.Edition != plan.Audio.Parsed.Edition {
		return fmt.Errorf("edition mismatch: %s vs %s", plan.Video.Parsed.Edition, plan.Audio.Parsed.Edition)
	}
	if video.Duration <= 0 || audio.Duration <= 0 {
		return fmt.Errorf("duration is unknown for a dual-source plan")
	}

	tolerance := video.Duration * ratio
	if tolerance < 1 {
		tolerance = 1
	}
	if tolerance > 15 {
		tolerance = 15
	}
	if delta := math.Abs(video.Duration - audio.Duration); delta > tolerance {
		return fmt.Errorf("duration mismatch: %.3fs exceeds %.3fs tolerance", delta, tolerance)
	}

	if len(video.VideoStreams) > 0 && len(audio.VideoStreams) > 0 {
		left := video.VideoStreams[0].FrameRate
		right := audio.VideoStreams[0].FrameRate
		if left > 0 && right > 0 && math.Abs(left-right) > 0.02 {
			return fmt.Errorf("frame-rate mismatch: %.3f vs %.3f", left, right)
		}
	}
	return nil
}

// compatibleAudioMode always copies the original audio stream. Re-encoding was
// tried for codecs that are awkward in MPEG-TS (TrueHD, DTS, FLAC), but the
// resulting AAC was rejected by the Android ExoPlayer decoder. Copying the
// original track is what the source provides; if a particular codec is not
// playable, the health monitor or the plan fallback selects another source.
func compatibleAudioMode(codec string) ffmpeg.AudioMode {
	return ffmpeg.AudioModeCopy
}

func (m *Muxer) invalidatePlanSources(plan model.PlaybackPlan) {
	m.cacheMu.Lock()
	delete(m.resolved, plan.Video.SourceKey())
	delete(m.resolved, plan.Audio.SourceKey())
	m.cacheMu.Unlock()
}
