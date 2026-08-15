package ffmpeg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const probeTimeout = 40 * time.Second

// segmentTimeout bounds each ffmpeg segment generation. A 4s segment from a
// fast CDN should finish in a few seconds; this cap releases the per-segment
// singleflight lock even if a source stalls, so concurrent requests never
// block forever waiting on a stuck generator.
const segmentTimeout = 30 * time.Second

// segDuration is the length of each HLS segment in seconds.
const segDuration = 4.0

type Muxer struct {
	binaryPath string
}

func New(binaryPath string) *Muxer {
	if binaryPath == "" {
		binaryPath = "ffmpeg"
	}
	return &Muxer{binaryPath: binaryPath}
}

// GenerateSegment produces a single HLS .ts segment covering [offset,
// offset+segDuration). It uses -ss before each -i (input seek), which makes
// FFmpeg issue an HTTP Range request to the source instead of downloading from
// the start — so seeking to minute 5 or minute 100 costs the same.
//
// audioTrackIndex selects the audio track by numeric index (e.g. 0 = first
// GenerateVideoSegment produces a single HLS .ts segment containing only the
// video stream of the source at [offset, offset+segDuration). It uses -ss
// before -i (input seek via HTTP Range), so seeking costs the same anywhere.
//
// analyzeduration/probesize are kept small so FFmpeg starts emitting output
// almost immediately instead of reading far ahead into the remote file.
func (m *Muxer) GenerateVideoSegment(ctx context.Context, videoURL string, offset float64, out io.Writer) error {
	args := []string{
		"-ss", fmtDuration(offset),
		"-i", videoURL,
		"-map", "0:v:0",
		"-t", fmtDuration(segDuration),
		"-analyzeduration", "1000000",
		"-probesize", "2000000",
		"-c", "copy",
		"-avoid_negative_ts", "make_zero",
		"-f", "mpegts",
		"pipe:1",
	}
	return m.runFFmpeg(ctx, args, out)
}

// GenerateAudioSegment produces a single HLS .ts segment containing only the
// audio track at audioTrackIndex of the source at [offset, offset+segDuration).
// The track index is resolved once per job by probing, so each segment avoids
// scanning language metadata. A negative index falls back to the first track.
func (m *Muxer) GenerateAudioSegment(ctx context.Context, audioURL string, audioTrackIndex int, offset float64, out io.Writer) error {
	idx := audioTrackIndex
	if idx < 0 {
		idx = 0
	}
	args := []string{
		"-ss", fmtDuration(offset),
		"-i", audioURL,
		"-map", fmt.Sprintf("0:a:%d", idx),
		"-t", fmtDuration(segDuration),
		"-analyzeduration", "1000000",
		"-probesize", "2000000",
		"-c", "copy",
		"-avoid_negative_ts", "make_zero",
		"-f", "mpegts",
		"pipe:1",
	}
	return m.runFFmpeg(ctx, args, out)
}

// runFFmpeg executes an ffmpeg command with a dedicated timeout, streaming its
// stdout to out and capturing a bounded tail of stderr for errors.
func (m *Muxer) runFFmpeg(ctx context.Context, args []string, out io.Writer) error {
	// Cap the whole ffmpeg run: a stalled source must not hold the segment
	// singleflight lock indefinitely (that would block every retry of the same
	// segment). On timeout the caller fails fast and the next attempt retries.
	segCtx, cancel := context.WithTimeout(ctx, segmentTimeout)
	defer cancel()

	cmd := exec.CommandContext(segCtx, m.binaryPath, args...)
	cmd.Stdout = out
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	log.Printf("ffmpeg segment: %s", strings.Join(args, " "))

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, tail(stderrBuf.String(), 800))
	}
	return nil
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
	AudioTracks []AudioTrack
}

// Probe inspects a stream's duration and audio tracks in a single ffprobe call.
// analyzeduration/probesize are kept small so probing a large remote file does
// not read far ahead (defaults read up to 5MB/5s, which at high bitrates is
// tens of megabytes over the network).
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
			Tags      struct {
				Language string `json:"language"`
			} `json:"tags"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(output, &p); err != nil {
		return nil, err
	}

	res := &ProbeResult{}
	audioRel := 0
	for _, s := range p.Streams {
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
	return res, nil
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