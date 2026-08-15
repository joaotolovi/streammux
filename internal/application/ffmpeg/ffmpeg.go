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

const probeTimeout = 40 * time.Second

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
// audio track). It is resolved once per job by probing the source, so each
// segment avoids scanning language metadata — this keeps the time to the first
// byte low (the MKV header is read once, not re-scanned for languages).
//
// analyzeduration/probesize are kept small so FFmpeg starts emitting output
// almost immediately instead of reading far ahead into the remote file.
func (m *Muxer) GenerateSegment(ctx context.Context, videoURL, audioURL string, audioTrackIndex int, offset float64, out io.Writer) error {
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
		args = append(args, audioMapByIndex(audioTrackIndex, 0)...)
	} else {
		// Two sources: seek both to the same offset, in parallel.
		args = []string{
			"-ss", fmtDuration(offset),
			"-i", videoURL,
			"-ss", fmtDuration(offset),
			"-i", audioURL,
			"-map", "0:v:0",
		}
		args = append(args, audioMapByIndex(audioTrackIndex, 1)...)
	}
	args = append(args,
		"-analyzeduration", "1000000",
		"-probesize", "2000000",
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

// audioMapByIndex returns the -map args for the audio track of input srcIndex.
// A negative index falls back to the first audio track.
func audioMapByIndex(audioTrackIndex, srcIndex int) []string {
	idx := audioTrackIndex
	if idx < 0 {
		idx = 0
	}
	return []string{"-map", fmt.Sprintf("%d:a:%d", srcIndex, idx)}
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
func (m *Muxer) Probe(ctx context.Context, url string) (*ProbeResult, error) {
	args := []string{
		"-v", "quiet",
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