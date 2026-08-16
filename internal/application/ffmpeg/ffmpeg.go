package ffmpeg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const probeTimeout = 90 * time.Second

// segDuration is the length of each HLS segment in seconds.
const segDuration = 4.0

// Session is a single continuous ffmpeg run that produces HLS segments
// sequentially into an output directory. One -ss seek is performed up front;
// after that ffmpeg reads both sources in lockstep, so video and audio stay
// consistent and each segment's real duration is written by ffmpeg itself
// (no drift, no per-segment reconnect cost).
type Session struct {
	cancel context.CancelFunc
	done   chan struct{}
	startN int // segment number the run starts at
}

// StartSession launches ffmpeg to remux video+audio into HLS segments written
// to outDir as seg_%05d.ts starting at segment number startN. It uses the hls
// muxer with event-type playlist and independent segments so each .ts is
// self-contained and seekable. Returns the session; callers cancel it when
// done (e.g. on seek or job cleanup).
func (m *Muxer) StartSession(ctx context.Context, videoURL, audioURL string, audioTrackIndex, startN int, outDir string) (*Session, error) {
	offset := float64(startN) * segDuration

	args := []string{
		"-ss", fmtDuration(offset),
		"-i", videoURL,
		"-ss", fmtDuration(offset),
		"-i", audioURL,
		"-map", "0:v:0",
		"-map", fmt.Sprintf("1:a:%d", audioTrackIndex),
		"-c", "copy",
		"-avoid_negative_ts", "make_zero",
		"-f", "hls",
		"-hls_time", fmtDuration(segDuration),
		"-hls_playlist_type", "event",
		"-hls_flags", "independent_segments",
		"-hls_segment_filename", filepath.Join(outDir, "seg_%05d.ts"),
		"-start_number", strconv.Itoa(startN),
		filepath.Join(outDir, "live.m3u8"),
	}

	sessCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(sessCtx, m.binaryPath, args...)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	log.Printf("ffmpeg session start: %s", strings.Join(args, " "))

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("ffmpeg start: %w", err)
	}

	s := &Session{cancel: cancel, done: make(chan struct{}), startN: startN}
	go func() {
		err := cmd.Wait()
		if err != nil && sessCtx.Err() == nil {
			log.Printf("ffmpeg session ended: %v: %s", err, tail(stderrBuf.String(), 2000))
		}
		close(s.done)
	}()
	return s, nil
}

// StartN returns the segment number this session starts generating at.
func (s *Session) StartN() int {
	if s == nil {
		return 0
	}
	return s.startN
}

// Done returns a channel closed when the session's ffmpeg process exits.
func (s *Session) Done() <-chan struct{} {
	if s == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return s.done
}

// Cancel terminates the session's ffmpeg process.
func (s *Session) Cancel() {
	if s != nil && s.cancel != nil {
		s.cancel()
	}
}

type Muxer struct {
	binaryPath string
}

func New(binaryPath string) *Muxer {
	if binaryPath == "" {
		binaryPath = "ffmpeg"
	}
	return &Muxer{binaryPath: binaryPath}
}

// tail returns the last n bytes of s.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// AudioTrack is a detected audio stream.
type AudioTrack struct {
	Index    int
	Language string
}

// ProbeResult holds what ffprobe learns about a stream in one call.
type ProbeResult struct {
	Duration    float64
	VideoBitrate float64 // bits/s of the video stream (0 if unknown)
	AudioTracks []AudioTrack
}

// Probe inspects a stream's duration, video bitrate and audio tracks in a single
// ffprobe call. analyzeduration/probesize are kept small so probing a large
// remote file does not read far ahead (defaults read up to 5MB/5s, which at
// high bitrates is tens of megabytes over the network).
func (m *Muxer) Probe(ctx context.Context, url string) (*ProbeResult, error) {
	args := []string{
		"-v", "quiet",
		"-analyzeduration", "1000000",
		"-probesize", "2000000",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		url,
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, "ffprobe", args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}

	var p struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			BitRate   string `json:"bit_rate"`
			Tags      struct {
				Language string `json:"language"`
				BPS      string `json:"BPS"`
			} `json:"tags"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
			BitRate  string `json:"bit_rate"`
		} `json:"format"`
	}
	if err := json.Unmarshal(output, &p); err != nil {
		return nil, err
	}

	res := &ProbeResult{}
	audioRel := 0
	for _, s := range p.Streams {
		if s.CodecType == "video" {
			// MKV often omits stream.bit_rate; fall back to the format-level
			// bitrate or the mkvmerge BPS tag.
			switch {
			case s.BitRate != "":
				res.VideoBitrate = parseFloat(s.BitRate)
			case s.Tags.BPS != "":
				res.VideoBitrate = parseFloat(s.Tags.BPS)
			}
			continue
		}
		if s.CodecType != "audio" {
			continue
		}
		res.AudioTracks = append(res.AudioTracks, AudioTrack{
			Index:    audioRel,
			Language: s.Tags.Language,
		})
		audioRel++
	}
	if p.Format.Duration != "" {
		if d, err := strconv.ParseFloat(p.Format.Duration, 64); err == nil {
			res.Duration = d
		}
	}
	// MKV often omits per-stream bit_rate; the format-level bitrate is the
	// fallback for the video bitrate.
	if res.VideoBitrate <= 0 && p.Format.BitRate != "" {
		res.VideoBitrate = parseFloat(p.Format.BitRate)
	}
	return res, nil
}

func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// AudioTrackIndexByLanguage returns the relative index of the first audio track
// whose language code equals the target (ISO 639-2, e.g. "por"), or -1 when
// none matches. An untagged track is assumed to match when target is "eng".
func AudioTrackIndexByLanguage(tracks []AudioTrack, targetCode string) int {
	for _, t := range tracks {
		if t.Language == targetCode {
			return t.Index
		}
		if t.Language == "" && targetCode == "eng" {
			return t.Index
		}
	}
	return -1
}

// LanguageCode converts a human language name (e.g. "Portuguese (Brazil)") to
// an ISO 639-2 code (e.g. "por") for matching ffprobe language tags.
func LanguageCode(lang string) string {
	lower := strings.ToLower(lang)
	switch {
	case strings.Contains(lower, "portug"):
		return "por"
	case strings.Contains(lower, "english") || lower == "eng":
		return "eng"
	case strings.Contains(lower, "spanish") || strings.Contains(lower, "español"):
		return "spa"
	case strings.Contains(lower, "french") || strings.Contains(lower, "français"):
		return "fra"
	case strings.Contains(lower, "german") || strings.Contains(lower, "deutsch"):
		return "deu"
	case strings.Contains(lower, "italian"):
		return "ita"
	case strings.Contains(lower, "japanese"):
		return "jpn"
	case strings.Contains(lower, "korean"):
		return "kor"
	case strings.Contains(lower, "hindi"):
		return "hin"
	default:
		return ""
	}
}

// SegDuration returns the length of each HLS segment in seconds.
func SegDuration() float64 { return segDuration }

func fmtDuration(seconds float64) string {
	return strconv.FormatFloat(seconds, 'f', -1, 64)
}