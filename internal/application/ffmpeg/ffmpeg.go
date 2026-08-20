package ffmpeg

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// segDuration is the length of each HLS segment in seconds.
const segDuration = 4.0

// hlsWindowSegments bounds every on-disk rendition to roughly 48 seconds.
// Sessions read at playback speed, so this window stays ahead of the player
// without allowing a full film to accumulate in the server's temporary disk.
const hlsWindowSegments = 12

const stderrTailSize = 32 * 1024

// AudioMode controls how the selected audio stream is written to HLS.
type AudioMode string

const (
	AudioModeCopy AudioMode = "copy"
	AudioModeAAC  AudioMode = "aac"
)

// TranscodeSpec re-encodes the video on the fly instead of copying it. It is
// used by the ABR downgrade ladder when no lighter source exists: the same
// source is decoded once and re-encoded to a decode-friendly H.264 rendition
// (8-bit, capped bitrate) at the segment the player is about to watch.
type TranscodeSpec struct {
	// Height is the target height in pixels (width follows the aspect).
	Height int
	// MaxRateKbps caps the peak bitrate; CRF+maxrate bounds both quality and
	// bandwidth. BufSize defaults to 2x MaxRate.
	MaxRateKbps int
	// Preset is the x264 preset; veryfast sustains 4-8x realtime for 1080p
	// on a modern desktop CPU.
	Preset string
}

// SessionSpec describes one continuous FFmpeg HLS session. AudioURL may be
// empty (or equal to VideoURL) to select both streams from a single input.
// Stream indexes are relative to their media type, as used by FFmpeg maps such
// as 0:v:1 and 0:a:2.
type SessionSpec struct {
	VideoURL        string
	AudioURL        string
	VideoTrackIndex int
	AudioTrackIndex int
	// StartSegment is the first HLS segment number written (start_number).
	StartSegment int
	// StartTime is the content offset in seconds (-ss). It is independent of
	// StartSegment: the placeholder handoff numbers film segments from N while
	// still playing content from 0:00.
	StartTime     float64
	OutputDir     string
	AudioMode     AudioMode
	AudioLanguage string
	AudioTitle    string
	UserAgent     string
	// AudioOffset shifts the dubbed audio relative to the video, in seconds.
	// Positive delays the audio (moves it later); negative advances it. It is
	// only applied for dual-source sessions and is derived from a cross-
	// correlation of the first seconds of both audio tracks (or set manually).
	AudioOffset time.Duration
	// Transcode, when non-nil, re-encodes the video (see TranscodeSpec)
	// instead of stream-copying it.
	Transcode *TranscodeSpec
}

// AudioSessionSpec describes a lazy audio-only HLS session aligned to the
// public film segment timeline.
type AudioSessionSpec struct {
	AudioURL        string
	AudioTrackIndex int
	StartSegment    int
	StartTime       float64
	OutputDir       string
	AudioMode       AudioMode
	AudioLanguage   string
	AudioTitle      string
	UserAgent       string
	AudioOffset     time.Duration
}

// PlaceholderSpec describes a local placeholder HLS session. StartSegment is
// useful when a replacement joins an already-public timeline; CardPath is
// watched by drawtext with reload=1 so metadata can change without restarting
// the session.
type PlaceholderSpec struct {
	VideoPath    string
	ImagePath    string
	CardPath     string
	MetadataPath string
	DetailsPath  string
	OutputDir    string
	Realtime     bool
	StartSegment int
}

// Session is a single continuous FFmpeg run that produces HLS segments.
type Session struct {
	cancel     context.CancelFunc
	cancelOnce sync.Once
	done       chan struct{}
	progress   chan ProgressSample
	startN     int
	// stderrTail retains the last ffmpeg stderr bytes for diagnostics.
	stderrTail *tailBuffer

	mu  sync.RWMutex
	err error
}

// InitDone initialises the internal done channel. It exists for tests that
// construct a Session directly and never run FFmpeg.
func (s *Session) InitDone() {
	if s.done == nil {
		s.done = make(chan struct{})
	}
}

// StderrTail returns the last ffmpeg stderr output (diagnostics only).
func (s *Session) StderrTail() string {
	if s == nil || s.stderrTail == nil {
		return ""
	}
	return s.stderrTail.String()
}

// Muxer launches FFmpeg and ffprobe processes.
type Muxer struct {
	binaryPath string
}

func New(binaryPath string) *Muxer {
	if binaryPath == "" {
		binaryPath = "ffmpeg"
	}
	return &Muxer{binaryPath: binaryPath}
}

// StartSession launches FFmpeg using the supplied session specification.
func (m *Muxer) StartSession(ctx context.Context, spec SessionSpec) (*Session, error) {
	args, err := buildSessionArgs(spec)
	if err != nil {
		return nil, err
	}

	sessCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(sessCtx, m.binaryPath, args...)
	stderr := newTailBuffer(stderrTailSize)
	cmd.Stderr = stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("ffmpeg progress pipe: %w", err)
	}

	s := &Session{
		cancel:     cancel,
		done:       make(chan struct{}),
		progress:   make(chan ProgressSample, 1),
		startN:     spec.StartSegment,
		stderrTail: stderr,
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("ffmpeg start: %w", err)
	}

	go func() {
		parseErr := parseProgress(stdout, s.progress, time.Now)
		if parseErr != nil {
			// Keep draining even if malformed output makes the parser stop. FFmpeg
			// must never block because its progress pipe filled up.
			_, _ = io.Copy(io.Discard, stdout)
		}

		waitErr := cmd.Wait()
		if waitErr != nil {
			if ctxErr := sessCtx.Err(); ctxErr != nil {
				waitErr = ctxErr
			}
			s.setErr(ffmpegRunError(waitErr, stderr.String()))
		} else if parseErr != nil {
			s.setErr(ffmpegRunError(fmt.Errorf("read progress: %w", parseErr), stderr.String()))
		}

		cancel()
		close(s.progress)
		close(s.done)
	}()

	return s, nil
}

// StartAudioSession launches FFmpeg for one audio rendition only.
func (m *Muxer) StartAudioSession(ctx context.Context, spec AudioSessionSpec) (*Session, error) {
	args, err := buildAudioSessionArgs(spec)
	if err != nil {
		return nil, err
	}
	sessCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(sessCtx, m.binaryPath, args...)
	stderr := newTailBuffer(stderrTailSize)
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("ffmpeg audio progress pipe: %w", err)
	}
	s := &Session{cancel: cancel, done: make(chan struct{}), progress: make(chan ProgressSample, 1), startN: spec.StartSegment, stderrTail: stderr}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("ffmpeg audio start: %w", err)
	}
	go func() {
		parseErr := parseProgress(stdout, s.progress, time.Now)
		if parseErr != nil {
			_, _ = io.Copy(io.Discard, stdout)
		}
		waitErr := cmd.Wait()
		if waitErr != nil {
			if ctxErr := sessCtx.Err(); ctxErr != nil {
				waitErr = ctxErr
			}
			s.setErr(ffmpegRunError(waitErr, stderr.String()))
		} else if parseErr != nil {
			s.setErr(ffmpegRunError(fmt.Errorf("read progress: %w", parseErr), stderr.String()))
		}
		cancel()
		close(s.progress)
		close(s.done)
	}()
	return s, nil
}

func buildAudioSessionArgs(spec AudioSessionSpec) ([]string, error) {
	if strings.TrimSpace(spec.AudioURL) == "" {
		return nil, fmt.Errorf("ffmpeg audio session: audio URL is required")
	}
	if strings.TrimSpace(spec.OutputDir) == "" {
		return nil, fmt.Errorf("ffmpeg audio session: output directory is required")
	}
	if spec.AudioTrackIndex < 0 || spec.StartSegment < 0 {
		return nil, fmt.Errorf("ffmpeg audio session: invalid track or segment")
	}
	audioMode := AudioMode(strings.ToLower(strings.TrimSpace(string(spec.AudioMode))))
	if audioMode == "" {
		audioMode = AudioModeCopy
	}
	if audioMode != AudioModeCopy && audioMode != AudioModeAAC {
		return nil, fmt.Errorf("ffmpeg audio session: unsupported audio mode %q", spec.AudioMode)
	}
	offset := spec.StartTime
	if offset < 0 {
		offset = 0
	}
	args := []string{"-nostdin", "-hide_banner", "-nostats", "-stats_period", "1", "-progress", "pipe:1", "-y"}
	if spec.AudioOffset != 0 {
		args = append(args, "-itsoffset", fmtDuration(spec.AudioOffset.Seconds()))
	}
	args = append(args, "-ss", fmtDuration(offset))
	if ua := strings.TrimSpace(spec.UserAgent); ua != "" {
		args = append(args, "-user_agent", ua)
	}
	args = append(args, "-i", spec.AudioURL, "-map", fmt.Sprintf("0:a:%d", spec.AudioTrackIndex), "-c:a", string(audioMode))
	if language := normalizeLanguage(spec.AudioLanguage); language != "" {
		args = append(args, "-metadata:s:a:0", "language="+language, "-disposition:a:0", "default")
	}
	if title := strings.TrimSpace(spec.AudioTitle); title != "" {
		args = append(args, "-metadata:s:a:0", "title="+title)
	}
	args = append(args, "-f", "hls", "-hls_time", fmtDuration(segDuration), "-hls_list_size", strconv.Itoa(hlsWindowSegments), "-hls_flags", "independent_segments+temp_file+split_by_time+delete_segments", "-hls_segment_filename", filepath.Join(spec.OutputDir, "audio", "seg_%05d.ts"), "-start_number", strconv.Itoa(spec.StartSegment), filepath.Join(spec.OutputDir, "audio", "audio.m3u8"))
	return args, nil
}

func buildSessionArgs(spec SessionSpec) ([]string, error) {
	if strings.TrimSpace(spec.VideoURL) == "" {
		return nil, fmt.Errorf("ffmpeg session: video URL is required")
	}
	if strings.TrimSpace(spec.OutputDir) == "" {
		return nil, fmt.Errorf("ffmpeg session: output directory is required")
	}
	if spec.VideoTrackIndex < 0 {
		return nil, fmt.Errorf("ffmpeg session: video track index must not be negative")
	}
	if spec.AudioTrackIndex < 0 {
		return nil, fmt.Errorf("ffmpeg session: audio track index must not be negative")
	}
	if spec.StartSegment < 0 {
		return nil, fmt.Errorf("ffmpeg session: start segment must not be negative")
	}

	audioMode := AudioMode(strings.ToLower(strings.TrimSpace(string(spec.AudioMode))))
	if audioMode == "" {
		audioMode = AudioModeCopy
	}
	if audioMode != AudioModeCopy && audioMode != AudioModeAAC {
		return nil, fmt.Errorf("ffmpeg session: unsupported audio mode %q", spec.AudioMode)
	}

	offset := spec.StartTime
	if offset < 0 {
		offset = 0
	}
	args := []string{
		"-nostdin",
		"-hide_banner",
		"-nostats",
		"-stats_period", "1",
		"-progress", "pipe:1",
		"-y",
		"-ss", fmtDuration(offset),
	}
	if userAgent := strings.TrimSpace(spec.UserAgent); userAgent != "" {
		args = append(args, "-user_agent", userAgent)
	}
	args = append(args, "-i", spec.VideoURL)

	dualSource := strings.TrimSpace(spec.AudioURL) != "" && spec.AudioURL != spec.VideoURL
	if dualSource {
		if spec.AudioOffset != 0 {
			// -itsoffset shifts the timestamps of the following -i input:
			// positive delays it, negative advances it. Applied to the dubbed
			// audio to re-align it with the video content.
			args = append(args, "-itsoffset", fmtDuration(spec.AudioOffset.Seconds()))
		}
		args = append(args, "-ss", fmtDuration(offset))
		if userAgent := strings.TrimSpace(spec.UserAgent); userAgent != "" {
			args = append(args, "-user_agent", userAgent)
		}
		args = append(args, "-i", spec.AudioURL)
	}

	audioInput := 0
	if dualSource {
		audioInput = 1
	}

	// Video-only rendition. After a seek (-ss > 0) the video is split by time
	// as well: with stream copy the first segment would otherwise stretch to
	// the next keyframe (a whole GOP, ~10s) while audio splits exactly at
	// hls_time, permanently misaligning the two renditions. split_by_time
	// keeps both renditions on the same 4s grid; the first post-seek segment
	// may start mid-GOP (players decode from its first keyframe).
	videoFlags := "independent_segments+temp_file+delete_segments"
	if spec.StartTime > 0 {
		videoFlags = "temp_file+split_by_time+discont_start+delete_segments"
	} else if spec.StartSegment > 0 {
		videoFlags += "+discont_start"
	}
	args = append(args,
		"-map", fmt.Sprintf("0:v:%d", spec.VideoTrackIndex),
	)
	if tc := spec.Transcode; tc != nil {
		// Downgrade-ladder transcode: decode once, re-encode to a capped,
		// decode-friendly H.264 rendition. Keyframes forced on the 4s grid so
		// HLS segmentation stays aligned without split_by_time.
		preset := strings.TrimSpace(tc.Preset)
		if preset == "" {
			preset = "veryfast"
		}
		bufKbps := tc.MaxRateKbps * 2
		args = append(args,
			"-c:v", "libx264",
			"-preset", preset,
			"-pix_fmt", "yuv420p",
			"-vf", fmt.Sprintf("scale=-2:%d", tc.Height),
			"-crf", "20",
			"-maxrate", fmt.Sprintf("%dk", tc.MaxRateKbps),
			"-bufsize", fmt.Sprintf("%dk", bufKbps),
			"-sc_threshold", "0",
			"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%.0f)", segDuration),
		)
	} else {
		args = append(args, "-c:v", "copy")
	}
	args = append(args,
		"-f", "hls",
		"-hls_time", fmtDuration(segDuration),
		"-hls_list_size", strconv.Itoa(hlsWindowSegments),
		"-hls_flags", videoFlags,
		"-hls_segment_filename", filepath.Join(spec.OutputDir, "video", "seg_%05d.ts"),
		"-start_number", strconv.Itoa(spec.StartSegment),
		filepath.Join(spec.OutputDir, "video", "video.m3u8"),
	)

	// Audio-only rendition: split_by_time keeps it on the exact 4s grid
	// (audio frames are dense, cutting anywhere is safe).
	audioFlags := "independent_segments+temp_file+split_by_time+delete_segments"
	if spec.StartSegment > 0 {
		audioFlags += "+discont_start"
	}
	args = append(args,
		"-map", fmt.Sprintf("%d:a:%d", audioInput, spec.AudioTrackIndex),
		"-c:a", string(audioMode),
	)
	if language := normalizeLanguage(spec.AudioLanguage); language != "" {
		args = append(args,
			"-metadata:s:a:0", "language="+language,
			"-disposition:a:0", "default",
		)
	}
	if title := strings.TrimSpace(spec.AudioTitle); title != "" {
		args = append(args, "-metadata:s:a:0", "title="+title)
	}
	args = append(args,
		"-f", "hls",
		"-hls_time", fmtDuration(segDuration),
		"-hls_list_size", strconv.Itoa(hlsWindowSegments),
		"-hls_flags", audioFlags,
		"-hls_segment_filename", filepath.Join(spec.OutputDir, "audio", "seg_%05d.ts"),
		"-start_number", strconv.Itoa(spec.StartSegment),
		filepath.Join(spec.OutputDir, "audio", "audio.m3u8"),
	)
	return args, nil
}

// StartN returns the segment number this session starts generating at.
func (s *Session) StartN() int {
	if s == nil {
		return 0
	}
	return s.startN
}

// Done returns a channel closed after FFmpeg exits and Err is populated.
func (s *Session) Done() <-chan struct{} {
	if s == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return s.done
}

// Progress returns a latest-wins stream of FFmpeg progress samples. The channel
// is closed before Done is closed.
func (s *Session) Progress() <-chan ProgressSample {
	if s == nil {
		ch := make(chan ProgressSample)
		close(ch)
		return ch
	}
	return s.progress
}

// Cancel requests process termination. Repeated calls are safe.
func (s *Session) Cancel() {
	if s == nil {
		return
	}
	s.cancelOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
}

// Err returns the terminal session error. It is safe to call after Done closes;
// before then it returns the error observed so far, normally nil.
func (s *Session) Err() error {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.err
}

func (s *Session) setErr(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

func ffmpegRunError(cause error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return fmt.Errorf("ffmpeg session: %w", cause)
	}
	return fmt.Errorf("ffmpeg session: %w; stderr tail: %s", cause, stderr)
}

// SegDuration returns the length of each HLS segment in seconds.
func SegDuration() float64 { return segDuration }

func fmtDuration(seconds float64) string {
	return strconv.FormatFloat(seconds, 'f', -1, 64)
}

// tailBuffer retains only the last cap bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	cap int
}

func newTailBuffer(capacity int) *tailBuffer {
	if capacity < 0 {
		capacity = 0
	}
	return &tailBuffer{buf: make([]byte, 0, capacity), cap: capacity}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if written == 0 || b.cap == 0 {
		return written, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if len(p) >= b.cap {
		b.buf = append(b.buf[:0], p[len(p)-b.cap:]...)
		return written, nil
	}
	if excess := len(b.buf) + len(p) - b.cap; excess > 0 {
		copy(b.buf, b.buf[excess:])
		b.buf = b.buf[:len(b.buf)-excess]
	}
	b.buf = append(b.buf, p...)
	return written, nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// buildImagePlaceholderArgs encodes the local placeholder video composed with
// a static poster image. The video shifts -140px to the left while a 256x384
// poster slides in from x=1280 to x=947 between t=2.5s and t=3.3s. The poster
// has rounded corners and a subtle gray border. The output is an HLS live
// window identical to buildPlaceholderArgs, so the film handoff is unchanged.
func buildImagePlaceholderArgs(path, imagePath, outputDir string, realtime bool) []string {
	return buildImagePlaceholderArgsWithOptions(path, imagePath, outputDir, realtime, 0, "")
}

func buildImagePlaceholderArgsWithOptions(path, imagePath, outputDir string, realtime bool, startSegment int, cardPath string) []string {
	return buildImagePlaceholderArgsWithCards(path, imagePath, outputDir, realtime, startSegment, cardPath, "", "")
}

func buildImagePlaceholderArgsWithCards(path, imagePath, outputDir string, realtime bool, startSegment int, cardPath, metadataPath, detailsPath string) []string {
	const (
		startT       = 2.5
		duration     = 0.8
		posterW      = 256 // 320 * 0.8
		posterH      = 384 // 480 * 0.8
		posterX      = 1280 - posterW - 77
		posterY      = (720 - posterH) / 2
		shiftLeft    = 140
		videoSpeed   = float64(shiftLeft) / duration
		posterSpeedX = 1280 - posterX
		posterSpeed  = float64(posterSpeedX) / duration
	)

	filter := fmt.Sprintf(
		"color=c=black:s=1280x720[base];"+
			"[1:v]scale=%d:%d,format=yuva420p[poster_raw];"+
			"[2:v]scale=%d:%d[mask_scaled];"+
			"[3:v]scale=%d:%d[border_scaled];"+
			"[poster_raw][mask_scaled]alphamerge[rounded];"+
			"[rounded]fade=t=in:st=%.2f:d=%.2f:alpha=1,setpts=PTS-STARTPTS[poster];"+
			"[base][0:v]overlay=x='%s':y=0[shifted];"+
			"[shifted][poster]overlay=x='%s':y=%d:enable='gt(t,%.2f)'[withposter];"+
			"[withposter][border_scaled]overlay=x='%s':y=%d:enable='gt(t,%.2f)'[v]",
		posterW, posterH,
		posterW, posterH,
		posterW, posterH,
		startT, duration,
		shiftExpr(startT, duration, -shiftLeft, videoSpeed, false, -shiftLeft),
		shiftExpr(startT, duration, 1280-posterX, posterSpeed, true, posterX),
		posterY, startT,
		shiftExpr(startT, duration, 1280-posterX, posterSpeed, true, posterX),
		posterY, startT,
	)
	outputLabel := "v"
	if cardPath != "" {
		filter += ";[v]" + placeholderDrawtextFilter(cardPath, metadataPath, detailsPath) + "[card]"
		outputLabel = "card"
	}

	preset := "veryfast"
	if !realtime {
		preset = "ultrafast"
	}
	videoFlags := "independent_segments+temp_file"
	audioFlags := "independent_segments+temp_file+split_by_time"
	if realtime {
		videoFlags += "+omit_endlist"
		audioFlags += "+omit_endlist"
	}

	args := []string{"-nostdin", "-hide_banner", "-nostats"}
	if realtime {
		args = append(args, "-readrate", "1", "-readrate_initial_burst", fmtDuration(segDuration))
	}
	args = append(args,
		"-i", path,
		"-loop", "1", "-i", imagePath,
		"-loop", "1", "-i", findAsset(path, "poster_round_mask.png"),
		"-loop", "1", "-i", findAsset(path, "poster_round_border.png"),
	)
	args = append(args,
		"-filter_complex", filter,
		"-shortest",
		"-map", "["+outputLabel+"]",
		"-c:v", "libx264", "-preset", preset, "-crf", "23",
		"-g", "96", "-keyint_min", "96", "-sc_threshold", "0",
		"-force_key_frames", "expr:gte(t,n_forced*4)",
		"-f", "hls",
		"-hls_time", fmtDuration(segDuration),
		"-hls_list_size", "3",
		"-hls_allow_cache", "0",
		"-hls_flags", videoFlags,
		"-hls_segment_filename", filepath.Join(outputDir, "video", "seg_%05d.ts"),
		"-start_number", strconv.Itoa(startSegment),
		filepath.Join(outputDir, "video", "video.m3u8"),
		"-map", "0:a:0",
		"-c:a", "aac", "-b:a", "128k",
		"-f", "hls",
		"-hls_time", fmtDuration(segDuration),
		"-hls_list_size", "3",
		"-hls_allow_cache", "0",
		"-hls_flags", audioFlags,
		"-hls_segment_filename", filepath.Join(outputDir, "audio", "seg_%05d.ts"),
		"-start_number", strconv.Itoa(startSegment),
		filepath.Join(outputDir, "audio", "audio.m3u8"),
	)
	return args
}

// findAsset resolves the path to an embedded overlay asset. When the placeholder
// path is inside a temp directory, the asset files are extracted next to it.
func findAsset(placeholderPath, name string) string {
	dir := filepath.Dir(placeholderPath)
	candidate := filepath.Join(dir, name)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return name
}

// shiftExpr builds an expression that moves from startX to endX over the
// animation interval [startT, startT+duration]. The returned string is
// suitable for an overlay x= parameter. When reverse is true the value
// decreases from right (1280) to endX; otherwise it decreases from 0 to endX
// (a negative value, i.e. shifting left).
func shiftExpr(startT, duration float64, distance, speed float64, reverse bool, endX int) string {
	_ = distance
	startX := 1280
	if reverse {
		// Poster: starts at 1280 (off-screen right), slides left to endX,
		// then clamps at endX via max() so it never goes past endX to the
		// left. max(947, 1280)=1280 at start, max(947, 947)=947 at end.
		return fmt.Sprintf("if(gt(t,%.2f),max(%d,%d-(t-%.2f)*%.0f),%d)", startT, endX, startX, startT, speed, startX)
	}
	// Video: starts at 0, shifts left to endX (negative), then clamps via max().
	return fmt.Sprintf("if(gt(t,%.2f),max(%d,-(t-%.2f)*%.0f),0)", startT, endX, startT, speed)
}

// buildPlaceholderArgs encodes a local video as a live-looking HLS window.
// realtime=true paces the placeholder at 1x with a sliding window and no
// ENDLIST (the film takes over the timeline). realtime=false encodes the
// terminal error video as fast as possible — it is short VOD content and must
// have all its segments ready in seconds, so no pacing and a natural ENDLIST.
func buildPlaceholderArgs(path, outputDir string, realtime bool) []string {
	return buildPlaceholderArgsWithOptions(path, outputDir, realtime, 0, "")
}

func buildPlaceholderArgsWithOptions(path, outputDir string, realtime bool, startSegment int, cardPath string) []string {
	return buildPlaceholderArgsWithCards(path, outputDir, realtime, startSegment, cardPath, "", "")
}

func buildPlaceholderArgsWithCards(path, outputDir string, realtime bool, startSegment int, cardPath, metadataPath, detailsPath string) []string {
	preset := "veryfast"
	if !realtime {
		preset = "ultrafast"
	}
	videoFlags := "independent_segments+temp_file"
	audioFlags := "independent_segments+temp_file+split_by_time"
	if realtime {
		videoFlags += "+omit_endlist"
		audioFlags += "+omit_endlist"
	}
	args := []string{"-nostdin", "-hide_banner", "-nostats"}
	if realtime {
		args = append(args, "-readrate", "1", "-readrate_initial_burst", fmtDuration(segDuration))
	}
	args = append(args, "-i", path)
	if cardPath != "" {
		args = append(args, "-vf", placeholderDrawtextFilter(cardPath, metadataPath, detailsPath))
	}
	args = append(args,
		"-map", "0:v:0",
		"-c:v", "libx264", "-preset", preset, "-crf", "23",
		"-g", "96", "-keyint_min", "96", "-sc_threshold", "0",
		"-force_key_frames", "expr:gte(t,n_forced*4)",
		"-f", "hls",
		"-hls_time", fmtDuration(segDuration),
		"-hls_list_size", "3",
		"-hls_allow_cache", "0",
		"-hls_flags", videoFlags,
		"-hls_segment_filename", filepath.Join(outputDir, "video", "seg_%05d.ts"),
		"-start_number", strconv.Itoa(startSegment),
		filepath.Join(outputDir, "video", "video.m3u8"),
		"-map", "0:a:0",
		"-c:a", "aac", "-b:a", "128k",
		"-f", "hls",
		"-hls_time", fmtDuration(segDuration),
		"-hls_list_size", "3",
		"-hls_allow_cache", "0",
		"-hls_flags", audioFlags,
		"-hls_segment_filename", filepath.Join(outputDir, "audio", "seg_%05d.ts"),
		"-start_number", strconv.Itoa(startSegment),
		filepath.Join(outputDir, "audio", "audio.m3u8"),
	)
	return args
}

func placeholderDrawtextFilter(cardPath string, metadataPath ...string) string {
	filters := []string{"fade=t=in:st=0:d=0.7"}
	if len(metadataPath) > 0 && metadataPath[0] != "" {
		filters = append(filters, fmt.Sprintf("drawtext=textfile='%s':reload=1:fontcolor=white:fontsize=22:line_spacing=5:box=1:boxcolor=black@0.48:boxborderw=10:x=947:y=570", escapeFilterPath(metadataPath[0])))
	}
	if cardPath != "" {
		filters = append(filters, fmt.Sprintf("drawtext=textfile='%s':reload=1:fontcolor=white:fontsize=24:box=1:boxcolor=black@0.42:boxborderw=10:x=48:y=48:alpha='if(lt(t,0.8),t/0.8,1)'", escapeFilterPath(cardPath)))
	}
	if len(metadataPath) > 1 && metadataPath[1] != "" {
		filters = append(filters, fmt.Sprintf("drawtext=textfile='%s':reload=1:fontcolor=white:fontsize=24:box=1:boxcolor=black@0.42:boxborderw=10:x=48:y=86:alpha='if(lt(t,1.1),0,if(lt(t,1.8),(t-1.1)/0.7,1))'", escapeFilterPath(metadataPath[1])))
	}
	return strings.Join(filters, ",")
}

func escapeFilterPath(path string) string {
	path = strings.ReplaceAll(path, `\`, `\\`)
	path = strings.ReplaceAll(path, `'`, `\'`)
	return strings.ReplaceAll(path, ":", `\:`)
}

// StartSinglePlaceholderSession launches a local video as a live-window HLS
// session. realtime=true paces the placeholder at 1x keeping the timeline
// open (film handoff); false encodes the terminal error video as fast as
// possible with a natural ENDLIST.
func (m *Muxer) StartSinglePlaceholderSession(ctx context.Context, path, outputDir string, realtime bool) (*Session, error) {
	return m.StartPlaceholderSession(ctx, PlaceholderSpec{VideoPath: path, OutputDir: outputDir, Realtime: realtime})
}

// StartImagePlaceholderSession launches the local placeholder video composed
// with a static poster image. The poster slides in from the right after a
// short delay while the placeholder video slides slightly to the left. The
// output is identical to a regular placeholder session, so the film handoff
// works unchanged.
func (m *Muxer) StartImagePlaceholderSession(ctx context.Context, path, imagePath, outputDir string, realtime bool) (*Session, error) {
	return m.StartPlaceholderSession(ctx, PlaceholderSpec{VideoPath: path, ImagePath: imagePath, OutputDir: outputDir, Realtime: realtime})
}

// StartPlaceholderSession launches a placeholder with optional poster/card
// composition and an explicit public segment start.
func (m *Muxer) StartPlaceholderSession(ctx context.Context, spec PlaceholderSpec) (*Session, error) {
	if strings.TrimSpace(spec.VideoPath) == "" {
		return nil, fmt.Errorf("placeholder session: no video provided")
	}
	if strings.TrimSpace(spec.OutputDir) == "" {
		return nil, fmt.Errorf("placeholder session: output directory is required")
	}
	if spec.StartSegment < 0 {
		return nil, fmt.Errorf("placeholder session: invalid start segment")
	}
	if err := os.MkdirAll(filepath.Join(spec.OutputDir, "video"), 0755); err != nil {
		return nil, fmt.Errorf("placeholder session: video dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(spec.OutputDir, "audio"), 0755); err != nil {
		return nil, fmt.Errorf("placeholder session: audio dir: %w", err)
	}

	var args []string
	if spec.ImagePath != "" {
		args = buildImagePlaceholderArgsWithCards(spec.VideoPath, spec.ImagePath, spec.OutputDir, spec.Realtime, spec.StartSegment, spec.CardPath, spec.MetadataPath, spec.DetailsPath)
	} else {
		args = buildPlaceholderArgsWithCards(spec.VideoPath, spec.OutputDir, spec.Realtime, spec.StartSegment, spec.CardPath, spec.MetadataPath, spec.DetailsPath)
	}

	sessCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(sessCtx, m.binaryPath, args...)
	stderr := newTailBuffer(stderrTailSize)
	cmd.Stderr = stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("placeholder progress pipe: %w", err)
	}

	s := &Session{
		cancel:     cancel,
		done:       make(chan struct{}),
		progress:   make(chan ProgressSample, 1),
		startN:     spec.StartSegment,
		stderrTail: stderr,
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("placeholder start: %w", err)
	}

	go func() {
		parseErr := parseProgress(stdout, s.progress, time.Now)
		if parseErr != nil {
			_, _ = io.Copy(io.Discard, stdout)
		}
		waitErr := cmd.Wait()
		if waitErr != nil {
			if ctxErr := sessCtx.Err(); ctxErr != nil {
				waitErr = ctxErr
			}
			s.setErr(ffmpegRunError(waitErr, stderr.String()))
		} else if parseErr != nil {
			s.setErr(ffmpegRunError(fmt.Errorf("read progress: %w", parseErr), stderr.String()))
		}
		cancel()
		close(s.progress)
		close(s.done)
	}()

	return s, nil
}
