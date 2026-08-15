package ffmpeg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const probeTimeout = 25 * time.Second

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
// audioLang, when non-empty, selects the audio track by language metadata
// (-map a:m:language:<lang>). An empty audioLang selects the first audio track.
func (m *Muxer) GenerateSegment(ctx context.Context, videoURL, audioURL, audioLang string, offset float64, out io.Writer) error {
	langCode := iso6391(audioLang)

	var args []string
	if videoURL == audioURL {
		// Single source: video and audio come from the same file, so only one
		// HTTP connection is opened.
		args = []string{
			"-ss", fmtDuration(offset),
			"-t", fmtDuration(segDuration),
			"-i", videoURL,
			"-map", "0:v:0",
		}
		args = append(args, audioMap(langCode, 0)...)
	} else {
		// Two sources: seek both to the same offset, in parallel.
		args = []string{
			"-ss", fmtDuration(offset),
			"-i", videoURL,
			"-ss", fmtDuration(offset),
			"-i", audioURL,
			"-map", "0:v:0",
		}
		args = append(args, audioMap(langCode, 1)...)
	}
	args = append(args,
		"-c", "copy",
		"-avoid_negative_ts", "make_zero",
		"-f", "mpegts",
		"pipe:1",
	)

	cmd := exec.CommandContext(ctx, m.binaryPath, args...)
	cmd.Stdout = out
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, stderrBuf.String())
	}
	return nil
}

// audioMap returns the -map args for the audio track of input srcIndex, given
// a language code (empty means "first audio track").
func audioMap(langCode string, srcIndex int) []string {
	if langCode == "" {
		return []string{"-map", fmt.Sprintf("%d:a:0", srcIndex)}
	}
	return []string{"-map", fmt.Sprintf("%d:a:m:language:%s", srcIndex, langCode)}
}

// ProbeDuration returns the duration of a stream in seconds.
func (m *Muxer) ProbeDuration(ctx context.Context, url string) (float64, error) {
	args := []string{
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		url,
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, "ffprobe", args...)
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe: %w", err)
	}

	var p struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(output, &p); err != nil {
		return 0, err
	}
	if p.Format.Duration == "" {
		return 0, nil
	}
	return strconv.ParseFloat(p.Format.Duration, 64)
}

// SegDuration returns the length of each HLS segment in seconds.
func SegDuration() float64 { return segDuration }

func fmtDuration(seconds float64) string {
	return strconv.FormatFloat(seconds, 'f', -1, 64)
}

// iso6391 converts a human language name to an ISO 639-2 code for FFmpeg.
func iso6391(lang string) string {
	switch {
	case strings.Contains(lang, "Portuguese"):
		return "por"
	case strings.Contains(lang, "English"):
		return "eng"
	case strings.Contains(lang, "Spanish"):
		return "spa"
	case strings.Contains(lang, "French"):
		return "fra"
	case strings.Contains(lang, "German"):
		return "deu"
	case strings.Contains(lang, "Italian"):
		return "ita"
	case strings.Contains(lang, "Japanese"):
		return "jpn"
	case strings.Contains(lang, "Korean"):
		return "kor"
	case strings.Contains(lang, "Hindi"):
		return "hin"
	default:
		return ""
	}
}