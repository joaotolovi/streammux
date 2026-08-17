package ffmpeg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const probeTimeout = 90 * time.Second

// VideoStream is a detected video stream. Index is relative to video streams
// and can be used directly in an FFmpeg map such as 0:v:Index.
type VideoStream struct {
	Index     int
	Codec     string
	Language  string
	Title     string
	BitRate   float64
	FrameRate float64
	Width     int
	Height    int
}

// AudioTrack is a detected audio stream. Index is relative to audio streams
// and can be used directly in an FFmpeg map such as 0:a:Index.
type AudioTrack struct {
	Index    int
	Language string
	Title    string
	Codec    string
	Channels int
	BitRate  float64
	Default  bool
	Forced   bool
}

// ProbeResult holds what ffprobe learns about a source. Duration,
// VideoBitrate, and AudioTracks retain their previous meanings for callers.
type ProbeResult struct {
	FormatName     string
	FormatLongName string
	Duration       float64
	StartTime      float64
	VideoBitrate   float64 // bits/s of the first video stream (0 if unknown)
	VideoStreams   []VideoStream
	AudioTracks    []AudioTrack
}

type probeJSON struct {
	Streams []probeStream `json:"streams"`
	Format  probeFormat   `json:"format"`
}

type probeStream struct {
	CodecType    string `json:"codec_type"`
	CodecName    string `json:"codec_name"`
	BitRate      string `json:"bit_rate"`
	Duration     string `json:"duration"`
	StartTime    string `json:"start_time"`
	Channels     int    `json:"channels"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	AvgFrameRate string `json:"avg_frame_rate"`
	RFrameRate   string `json:"r_frame_rate"`
	Tags         struct {
		Language string `json:"language"`
		Title    string `json:"title"`
		BPS      string `json:"BPS"`
	} `json:"tags"`
	Disposition struct {
		Default int `json:"default"`
		Forced  int `json:"forced"`
	} `json:"disposition"`
}

type probeFormat struct {
	FormatName     string `json:"format_name"`
	FormatLongName string `json:"format_long_name"`
	Duration       string `json:"duration"`
	StartTime      string `json:"start_time"`
	BitRate        string `json:"bit_rate"`
}

type probeLimits struct {
	analyzeDuration string
	probeSize       string
}

// Probe inspects a source with ffprobe. The first pass is small for speed; if
// it does not return a complete picture (no streams, no duration, or no audio
// track at all), a second pass reads more of the container header. The MKV
// header (which lists every track) lives at the start of the file, so a
// moderately larger probesize is enough — it is not a full-file download.
func (m *Muxer) Probe(ctx context.Context, url string) (*ProbeResult, error) {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	limits := []probeLimits{
		{analyzeDuration: "1000000", probeSize: "2000000"},
		{analyzeDuration: "10000000", probeSize: "20000000"},
	}

	var first *ProbeResult
	for attempt, limit := range limits {
		result, err := m.probeOnce(probeCtx, url, limit)
		if err != nil {
			if attempt == 1 {
				return nil, fmt.Errorf("ffprobe larger retry after incomplete result: %w", err)
			}
			return nil, err
		}
		if attempt == 0 && probeNeedsRetry(result) {
			first = result
			continue
		}
		return result, nil
	}
	return first, nil
}

func (m *Muxer) probeOnce(ctx context.Context, url string, limit probeLimits) (*ProbeResult, error) {
	args := []string{
		"-v", "error",
		"-analyzeduration", limit.analyzeDuration,
		"-probesize", limit.probeSize,
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		url,
	}

	binary := m.probeBinaryPath()
	cmd := exec.CommandContext(ctx, binary, args...)
	var stdout bytes.Buffer
	stderr := newTailBuffer(stderrTailSize)
	cmd.Stdout = &stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
		return nil, probeRunError(binary, err, stderr.String())
	}

	var decoded probeJSON
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(tail(stdout.String(), 2000))
		}
		if detail == "" {
			return nil, fmt.Errorf("decode %s output: %w", binary, err)
		}
		return nil, fmt.Errorf("decode %s output: %w; output tail: %s", binary, err, detail)
	}
	return decoded.result(), nil
}

func (m *Muxer) probeBinaryPath() string {
	path := m.binaryPath
	if path == "" {
		return "ffprobe"
	}

	dir, base := filepath.Split(path)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	lower := strings.ToLower(stem)
	if index := strings.Index(lower, "ffmpeg"); index >= 0 {
		stem = stem[:index] + "ffprobe" + stem[index+len("ffmpeg"):]
		return filepath.Join(dir, stem+ext)
	}
	return "ffprobe"
}

func probeRunError(binary string, cause error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return fmt.Errorf("%s: %w", binary, cause)
	}
	return fmt.Errorf("%s: %w; stderr tail: %s", binary, cause, stderr)
}

func probeNeedsRetry(result *ProbeResult) bool {
	if result == nil {
		return true
	}
	return len(result.VideoStreams)+len(result.AudioTracks) == 0 || result.Duration <= 0
}

func (p probeJSON) result() *ProbeResult {
	formatStart, hasFormatStart := parseFiniteFloat(p.Format.StartTime)
	result := &ProbeResult{
		FormatName:     p.Format.FormatName,
		FormatLongName: p.Format.FormatLongName,
		Duration:       parseFloat(p.Format.Duration),
		StartTime:      formatStart,
	}

	var maxStreamDuration float64
	var firstStreamStart float64
	haveStreamStart := false
	videoRelative := 0
	audioRelative := 0

	for _, stream := range p.Streams {
		if duration := parseFloat(stream.Duration); duration > maxStreamDuration {
			maxStreamDuration = duration
		}
		if start, ok := parseFiniteFloat(stream.StartTime); ok && (!haveStreamStart || start < firstStreamStart) {
			firstStreamStart = start
			haveStreamStart = true
		}

		switch stream.CodecType {
		case "video":
			bitRate := parseFloat(stream.BitRate)
			if bitRate <= 0 {
				bitRate = parseFloat(stream.Tags.BPS)
			}
			frameRate := parseFrameRate(stream.AvgFrameRate)
			if frameRate <= 0 {
				frameRate = parseFrameRate(stream.RFrameRate)
			}
			result.VideoStreams = append(result.VideoStreams, VideoStream{
				Index:     videoRelative,
				Codec:     stream.CodecName,
				Language:  strings.TrimSpace(stream.Tags.Language),
				Title:     strings.TrimSpace(stream.Tags.Title),
				BitRate:   bitRate,
				FrameRate: frameRate,
				Width:     stream.Width,
				Height:    stream.Height,
			})
			if result.VideoBitrate <= 0 && bitRate > 0 {
				result.VideoBitrate = bitRate
			}
			videoRelative++

		case "audio":
			bitRate := parseFloat(stream.BitRate)
			if bitRate <= 0 {
				bitRate = parseFloat(stream.Tags.BPS)
			}
			result.AudioTracks = append(result.AudioTracks, AudioTrack{
				Index:    audioRelative,
				Language: strings.TrimSpace(stream.Tags.Language),
				Title:    strings.TrimSpace(stream.Tags.Title),
				Codec:    stream.CodecName,
				Channels: stream.Channels,
				BitRate:  bitRate,
				Default:  stream.Disposition.Default != 0,
				Forced:   stream.Disposition.Forced != 0,
			})
			audioRelative++
		}
	}

	if result.Duration <= 0 {
		result.Duration = maxStreamDuration
	}
	if !hasFormatStart && haveStreamStart {
		result.StartTime = firstStreamStart
	}
	if result.VideoBitrate <= 0 {
		result.VideoBitrate = parseFloat(p.Format.BitRate)
	}
	return result
}

func parseFrameRate(value string) float64 {
	value = strings.TrimSpace(value)
	if value == "" || value == "0/0" {
		return 0
	}
	if numerator, denominator, ok := strings.Cut(value, "/"); ok {
		n := parseFloat(numerator)
		d := parseFloat(denominator)
		if d == 0 {
			return 0
		}
		return n / d
	}
	return parseFloat(value)
}

func parseFiniteFloat(value string) (float64, bool) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	return parsed, true
}

func parseFloat(value string) float64 {
	parsed, ok := parseFiniteFloat(value)
	if !ok {
		return 0
	}
	return parsed
}

// AudioTrackIndexByLanguage returns the relative index of the first audio track
// matching targetCode. An untagged track is still treated as English.
func AudioTrackIndexByLanguage(tracks []AudioTrack, targetCode string) int {
	target := normalizeLanguage(targetCode)
	for _, track := range tracks {
		language := normalizeLanguage(track.Language)
		if language == target {
			return track.Index
		}
		if language == "" && target == "eng" {
			return track.Index
		}
	}
	return -1
}

// LanguageCode converts language names and common ISO variants to a normalized
// ISO 639-2 code suitable for matching ffprobe tags.
func LanguageCode(language string) string {
	return normalizeLanguage(language)
}

func normalizeLanguage(language string) string {
	value := strings.ToLower(strings.TrimSpace(language))
	value = strings.ReplaceAll(value, "_", "-")
	if value == "" {
		return ""
	}

	base := value
	if index := strings.IndexByte(base, '-'); index >= 0 {
		base = base[:index]
	}

	switch base {
	case "pt", "por", "pob":
		return "por"
	case "en", "eng":
		return "eng"
	case "es", "spa":
		return "spa"
	case "fr", "fra", "fre":
		return "fra"
	case "de", "deu", "ger":
		return "deu"
	case "it", "ita":
		return "ita"
	case "ja", "jpn":
		return "jpn"
	case "ko", "kor":
		return "kor"
	case "hi", "hin":
		return "hin"
	case "ru", "rus":
		return "rus"
	case "ar", "ara":
		return "ara"
	case "zh", "zho", "chi":
		return "zho"
	}

	switch {
	case strings.Contains(value, "portug"):
		return "por"
	case strings.Contains(value, "english"), strings.Contains(value, "inglês"), strings.Contains(value, "ingles"):
		return "eng"
	case strings.Contains(value, "spanish"), strings.Contains(value, "español"), strings.Contains(value, "espanol"):
		return "spa"
	case strings.Contains(value, "french"), strings.Contains(value, "français"), strings.Contains(value, "francais"):
		return "fra"
	case strings.Contains(value, "german"), strings.Contains(value, "deutsch"), strings.Contains(value, "alemão"), strings.Contains(value, "alemao"):
		return "deu"
	case strings.Contains(value, "italian"), strings.Contains(value, "italiano"):
		return "ita"
	case strings.Contains(value, "japanese"), strings.Contains(value, "japonês"), strings.Contains(value, "japones"):
		return "jpn"
	case strings.Contains(value, "korean"), strings.Contains(value, "coreano"):
		return "kor"
	case strings.Contains(value, "hindi"):
		return "hin"
	case strings.Contains(value, "russian"), strings.Contains(value, "russo"):
		return "rus"
	case strings.Contains(value, "arabic"), strings.Contains(value, "árabe"), strings.Contains(value, "arabe"):
		return "ara"
	case strings.Contains(value, "chinese"), strings.Contains(value, "mandarin"), strings.Contains(value, "chinês"), strings.Contains(value, "chines"):
		return "zho"
	}

	if len(base) == 3 {
		return base
	}
	return ""
}
