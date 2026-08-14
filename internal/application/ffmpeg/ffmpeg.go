package ffmpeg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// probeTimeout bounds each ffprobe call. Probing a remote URL (especially a
// debrid link) can take several seconds to connect and read metadata, so this
// is generous — the probe now runs at playback time, not during stream listing.
const probeTimeout = 25 * time.Second

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

// RemuxToHLS runs FFmpeg to produce HLS segments (.ts) and a playlist (.m3u8)
// in the given output directory. It uses stream copy (no re-encoding) and
// writes segments as they are processed, so the player can start playback
// before FFmpeg finishes. The returned channel closes when FFmpeg exits.
//
// Instead of probing the audio source separately (which causes a second HTTP
// request that can hit rate limits), FFmpeg itself selects the audio track
// by language metadata: -map 1:a:m:language:<lang>. If no track matches, the
// first audio track is used as fallback.
func (m *Muxer) RemuxToHLS(ctx context.Context, videoURL, audioURL, audioLang string, outDir string) (<-chan error, error) {
	playlistPath := filepath.Join(outDir, "playlist.m3u8")
	segPattern := filepath.Join(outDir, "seg_%05d.ts")

	langCode := iso6391(audioLang)

	var args []string
	if videoURL == audioURL {
		// Single source: video and audio come from the same file. This avoids
		// a second HTTP request that would hit debrid proxy rate limits.
		args = []string{
			"-i", videoURL,
			"-map", "0:v:0",
			"-map", fmt.Sprintf("0:a:m:language:%s", langCode),
			"-c", "copy",
			"-f", "hls",
			"-hls_time", "4",
			"-hls_list_size", "0",
			"-hls_segment_filename", segPattern,
			playlistPath,
		}
	} else {
		// Two sources: video from input 0, audio from input 1.
		args = []string{
			"-i", videoURL,
			"-i", audioURL,
			"-map", "0:v:0",
			"-map", fmt.Sprintf("1:a:m:language:%s", langCode),
			"-c", "copy",
			"-f", "hls",
			"-hls_time", "4",
			"-hls_list_size", "0",
			"-hls_segment_filename", segPattern,
			playlistPath,
		}
	}

	cmd := exec.CommandContext(ctx, m.binaryPath, args...)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg start: %w", err)
	}

	errCh := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if err != nil {
			err = fmt.Errorf("ffmpeg: %w: %s", err, stderrBuf.String())
		}
		errCh <- err
		close(errCh)
	}()
	return errCh, nil
}

// iso6391 converts a human language name (e.g. "Portuguese (Brazil)") to an
// ISO 639-1 code (e.g. "por") suitable for FFmpeg's -map language filter.
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
		return "eng"
	}
}

// RemuxToHLSFirstAudio is a fallback that maps the first audio track (no
// language selection). Used when the language-specific map fails.
func (m *Muxer) RemuxToHLSFirstAudio(ctx context.Context, videoURL, audioURL, outDir string) (<-chan error, error) {
	playlistPath := filepath.Join(outDir, "playlist.m3u8")
	segPattern := filepath.Join(outDir, "seg_%05d.ts")

	var args []string
	if videoURL == audioURL {
		args = []string{
			"-i", videoURL,
			"-map", "0:v:0",
			"-map", "0:a:0",
			"-c", "copy",
			"-f", "hls",
			"-hls_time", "4",
			"-hls_list_size", "0",
			"-hls_segment_filename", segPattern,
			playlistPath,
		}
	} else {
		args = []string{
			"-i", videoURL,
			"-i", audioURL,
			"-map", "0:v:0",
			"-map", "1:a:0",
			"-c", "copy",
			"-f", "hls",
			"-hls_time", "4",
			"-hls_list_size", "0",
			"-hls_segment_filename", segPattern,
			playlistPath,
		}
	}

	cmd := exec.CommandContext(ctx, m.binaryPath, args...)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg start: %w", err)
	}

	errCh := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if err != nil {
			err = fmt.Errorf("ffmpeg: %w: %s", err, stderrBuf.String())
		}
		errCh <- err
		close(errCh)
	}()
	return errCh, nil
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
