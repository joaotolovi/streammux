package ffmpeg

import (
	"math"
	"testing"
)

func TestProbeResultUsesRelativeStreamIndexes(t *testing.T) {
	var payload probeJSON
	payload.Format.FormatName = "matroska,webm"
	payload.Format.Duration = "42.5"
	payload.Format.StartTime = "0.125"
	payload.Streams = append(payload.Streams,
		probeStream{CodecType: "video", CodecName: "h264", BitRate: "4000000", AvgFrameRate: "30000/1001"},
		probeStream{CodecType: "audio", CodecName: "aac", Channels: 2},
		probeStream{CodecType: "audio", CodecName: "ac3", Channels: 6},
	)

	result := payload.result()
	if result.FormatName != "matroska,webm" || result.Duration != 42.5 || result.StartTime != 0.125 {
		t.Fatalf("format fields = %#v", result)
	}
	if len(result.VideoStreams) != 1 || result.VideoStreams[0].Index != 0 {
		t.Fatalf("video streams = %#v", result.VideoStreams)
	}
	if math.Abs(result.VideoStreams[0].FrameRate-29.97002997) > 0.000001 {
		t.Fatalf("frame rate = %f", result.VideoStreams[0].FrameRate)
	}
	if len(result.AudioTracks) != 2 || result.AudioTracks[0].Index != 0 || result.AudioTracks[1].Index != 1 {
		t.Fatalf("audio tracks = %#v", result.AudioTracks)
	}
}

func TestProbeBinaryDerivedFromFFmpegPath(t *testing.T) {
	tests := map[string]string{
		"ffmpeg":                   "ffprobe",
		"/usr/local/bin/ffmpeg":    "/usr/local/bin/ffprobe",
		"/opt/media/ffmpeg-static": "/opt/media/ffprobe-static",
		"custom-binary":            "ffprobe",
	}
	for configured, want := range tests {
		if got := New(configured).probeBinaryPath(); got != want {
			t.Errorf("probe path for %q = %q, want %q", configured, got, want)
		}
	}
}
