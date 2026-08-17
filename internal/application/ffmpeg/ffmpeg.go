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

const stderrTailSize = 32 * 1024

// AudioMode controls how the selected audio stream is written to HLS.
type AudioMode string

const (
	AudioModeCopy AudioMode = "copy"
	AudioModeAAC  AudioMode = "aac"
)

// SessionSpec describes one continuous FFmpeg HLS session. AudioURL may be
// empty (or equal to VideoURL) to select both streams from a single input.
// Stream indexes are relative to their media type, as used by FFmpeg maps such
// as 0:v:1 and 0:a:2.
type SessionSpec struct {
	VideoURL        string
	AudioURL        string
	VideoTrackIndex int
	AudioTrackIndex int
	StartSegment    int
	OutputDir       string
	AudioMode       AudioMode
	AudioLanguage   string
	AudioTitle      string
	UserAgent       string
	// AudioOffset shifts the dubbed audio relative to the video, in seconds.
	// Positive delays the audio (moves it later); negative advances it. It is
	// only applied for dual-source sessions and is derived from a cross-
	// correlation of the first seconds of both audio tracks (or set manually).
	AudioOffset time.Duration
}

// Session is a single continuous FFmpeg run that produces HLS segments.
type Session struct {
	cancel     context.CancelFunc
	cancelOnce sync.Once
	done       chan struct{}
	progress   chan ProgressSample
	startN     int

	mu  sync.RWMutex
	err error
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

	// Write the master playlist that exposes the audio as an independent
	// rendition. This is what lets the player's native audio-delay control
	// seek/offset the audio track on its own.
	if err := writeMasterPlaylist(spec.OutputDir); err != nil {
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
		cancel:   cancel,
		done:     make(chan struct{}),
		progress: make(chan ProgressSample, 1),
		startN:   spec.StartSegment,
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

	offset := float64(spec.StartSegment) * segDuration
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

	hlsFlags := "independent_segments+temp_file"
	if spec.StartSegment > 0 {
		hlsFlags += "+discont_start"
	}

	// Video-only rendition.
	args = append(args,
		"-map", fmt.Sprintf("0:v:%d", spec.VideoTrackIndex),
		"-c:v", "copy",
		"-f", "hls",
		"-hls_time", fmtDuration(segDuration),
		"-hls_playlist_type", "event",
		"-hls_flags", hlsFlags,
		"-hls_segment_filename", filepath.Join(spec.OutputDir, "video", "seg_%05d.ts"),
		"-start_number", strconv.Itoa(spec.StartSegment),
		filepath.Join(spec.OutputDir, "video", "video.m3u8"),
	)

	// Audio-only rendition.
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
		"-hls_playlist_type", "event",
		"-hls_flags", hlsFlags,
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

// writeMasterPlaylist writes the master playlist that exposes the audio as an
// independent rendition (video-only + audio-only), so the player's native
// audio-delay control can seek/offset the audio track on its own.
func writeMasterPlaylist(outputDir string) error {
	master := "#EXTM3U\n" +
		"#EXT-X-VERSION:6\n" +
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"main\",DEFAULT=YES,AUTOSELECT=YES,URI=\"audio/audio.m3u8\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=8000000,AUDIO=\"aud\"\n" +
		"video/video.m3u8\n"
	return os.WriteFile(filepath.Join(outputDir, "master.m3u8"), []byte(master), 0644)
}

// StartPlaceholderSession launches FFmpeg to produce a looping HLS from local
// placeholder videos. introPath plays once, then loopPath loops forever
// (-stream_loop -1), so the player gets immediate playback while the real film
// is being prepared in the background. Either path may be empty; if both are
// empty the session is not started.
func (m *Muxer) StartPlaceholderSession(ctx context.Context, introPath, loopPath, outputDir string) (*Session, error) {
	if strings.TrimSpace(introPath) == "" && strings.TrimSpace(loopPath) == "" {
		return nil, fmt.Errorf("placeholder session: no placeholder video provided")
	}
	if strings.TrimSpace(outputDir) == "" {
		return nil, fmt.Errorf("placeholder session: output directory is required")
	}
	if err := os.MkdirAll(filepath.Join(outputDir, "video"), 0755); err != nil {
		return nil, fmt.Errorf("placeholder session: video dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(outputDir, "audio"), 0755); err != nil {
		return nil, fmt.Errorf("placeholder session: audio dir: %w", err)
	}
	if err := writeMasterPlaylist(outputDir); err != nil {
		return nil, err
	}

	// Placeholder is a finite 120s video: intro (once) + loop repeated to
	// fill the remaining time. No infinite loop — the playlist is a fixed
	// 120s (~30 segments of 4s) so the player sees a normal VOD timeline
	// that stalls at the end instead of looping forever. This keeps the
	// stitched handoff bounded and simple.
	const placeholderDuration = 120.0
	args := []string{
		"-nostdin",
		"-hide_banner",
		"-nostats",
	}
	inputs := []string{}
	filter := ""
	if introPath != "" {
		args = append(args, "-i", introPath)
		inputs = append(inputs, "0")
	}
	if loopPath != "" {
		loopDur := fileDuration(loopPath)
		if loopDur <= 0 {
			loopDur = 8.0
		}
		introDur := 0.0
		if introPath != "" {
			introDur = fileDuration(introPath)
			if introDur <= 0 {
				introDur = 10.0
			}
		}
		remaining := placeholderDuration - introDur
		if remaining < loopDur {
			remaining = loopDur
		}
		loops := int(remaining / loopDur)
		if remaining-float64(loops)*loopDur > 0.01 {
			loops++
		}
		if loops < 1 {
			loops = 1
		}
		if loops > 30 {
			loops = 30
		}
		for i := 0; i < loops; i++ {
			args = append(args, "-i", loopPath)
			inputs = append(inputs, strconv.Itoa(len(inputs)))
		}
	}
	// Build concat filter: [0:v][0:a][1:v][1:a]...concat=n=N:v=1:a=1[v][a]
	for _, in := range inputs {
		filter += "[" + in + ":v][" + in + ":a]"
	}
	filter += "concat=n=" + strconv.Itoa(len(inputs)) + ":v=1:a=1[v][a]"
	args = append(args, "-filter_complex", filter)

	// Trim the concatenated stream to exactly placeholderDuration (120s)
	// so we don't depend on exact loop multiples.
	args = append(args,
		"-map", "[v]",
		"-t", fmtDuration(placeholderDuration),
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
		"-g", "96", "-keyint_min", "96", "-sc_threshold", "0",
		"-force_key_frames", "expr:gte(t,n_forced*4)",
		"-f", "hls",
		"-hls_time", fmtDuration(segDuration),
		"-hls_playlist_type", "event",
		"-hls_flags", "independent_segments+temp_file",
		"-hls_segment_filename", filepath.Join(outputDir, "video", "seg_%05d.ts"),
		"-start_number", "0",
		filepath.Join(outputDir, "video", "video.m3u8"),
		"-map", "[a]",
		"-t", fmtDuration(placeholderDuration),
		"-c:a", "aac", "-b:a", "128k",
		"-f", "hls",
		"-hls_time", fmtDuration(segDuration),
		"-hls_playlist_type", "event",
		"-hls_flags", "independent_segments+temp_file",
		"-hls_segment_filename", filepath.Join(outputDir, "audio", "seg_%05d.ts"),
		"-start_number", "0",
		filepath.Join(outputDir, "audio", "audio.m3u8"),
	)

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
		cancel:   cancel,
		done:     make(chan struct{}),
		progress: make(chan ProgressSample, 1),
		startN:   0,
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

func fileDuration(path string) float64 {
	cmd := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if v <= 0 || v > 1e6 {
		return 0
	}
	return v
}
