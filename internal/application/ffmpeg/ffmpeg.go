package ffmpeg

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"time"
)

// probeTimeout bounds each ffprobe call. Probing a remote URL can be slow
// (HTTP headers for a multi-GB file), so we cap it to avoid stalling the
// stream response.
const probeTimeout = 8 * time.Second

type Muxer struct {
	binaryPath string
}

func New(binaryPath string) *Muxer {
	if binaryPath == "" {
		binaryPath = "ffmpeg"
	}
	return &Muxer{binaryPath: binaryPath}
}

func (m *Muxer) Remux(ctx context.Context, videoURL, audioURL string, audioTrackIndex int, out io.Writer) error {
	// Select the video track from input 0 and the requested audio track from
	// input 1. `-c copy` streams both without re-encoding.
	args := []string{
		"-i", videoURL,
		"-i", audioURL,
		"-map", "0:v:0",
		"-map", fmt.Sprintf("1:a:%d", audioTrackIndex),
		"-c", "copy",
		"-f", "matroska",
		"pipe:1",
	}

	cmd := exec.CommandContext(ctx, m.binaryPath, args...)
	cmd.Stdout = out
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg wait: %w", err)
	}
	return nil
}

// ProbeResult holds everything ffprobe learns about a stream in one call.
type ProbeResult struct {
	Duration    float64
	AudioTracks []AudioTrack
}

// Probe inspects a stream: its duration and its audio tracks. A single ffprobe
// call covers both, so remote URLs (which can be slow) are only fetched once.
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
			Codec     string `json:"codec_name"`
			Channels  int    `json:"channels"`
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
		// Only count audio streams, remapping to the relative audio-track
		// index expected by `-map 1:a:N`.
		if s.CodecType != "audio" {
			continue
		}
		res.AudioTracks = append(res.AudioTracks, AudioTrack{
			Index:    audioRel,
			Codec:    s.Codec,
			Channels: s.Channels,
			Language: s.Tags.Language,
		})
		audioRel++
	}
	if p.Format.Duration != "" {
		dur, err := strconv.ParseFloat(p.Format.Duration, 64)
		if err == nil {
			res.Duration = dur
		}
	}
	return res, nil
}

// ProbeAudioTracks returns the audio tracks of a stream.
func (m *Muxer) ProbeAudioTracks(ctx context.Context, url string) ([]AudioTrack, error) {
	res, err := m.Probe(ctx, url)
	if err != nil {
		return nil, err
	}
	return res.AudioTracks, nil
}

// ProbeDuration returns the duration of a stream in seconds.
func (m *Muxer) ProbeDuration(ctx context.Context, url string) (float64, error) {
	res, err := m.Probe(ctx, url)
	if err != nil {
		return 0, err
	}
	return res.Duration, nil
}

type AudioTrack struct {
	Index    int    `json:"index"`
	Codec    string `json:"codec_name"`
	Channels int    `json:"channels"`
	Language string `json:"language"`
}
