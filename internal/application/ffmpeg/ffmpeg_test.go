package ffmpeg

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBuildSessionArgsSingleSource(t *testing.T) {
	spec := SessionSpec{
		VideoURL:        "https://example.test/media.mkv",
		AudioURL:        "https://example.test/media.mkv",
		VideoTrackIndex: 1,
		AudioTrackIndex: 2,
		StartSegment:    3,
		StartTime:       12,
		OutputDir:       "/tmp/session",
		AudioMode:       AudioModeCopy,
		AudioLanguage:   "pt-BR",
		AudioTitle:      "Português",
	}

	got, err := buildSessionArgs(spec)
	if err != nil {
		t.Fatalf("buildSessionArgs() error = %v", err)
	}

	// Single input, two HLS outputs (video-only + audio-only).
	if countArgument(got, "-i") != 1 {
		t.Fatalf("input count = %d, want 1; args: %#v", countArgument(got, "-i"), got)
	}
	for _, want := range [][]string{
		{"-readrate", "1", "-readrate_initial_burst", "120", "-ss", "12", "-icy", "0", "-i", "https://example.test/media.mkv"},
		{"-map", "0:v:1"},
		{"-map", "0:a:2"},
		{"-c:v", "copy"},
		{"-c:a", "copy"},
		{"-metadata:s:a:0", "language=por"},
		{"-disposition:a:0", "default"},
		{"-metadata:s:a:0", "title=Português"},
		{"-hls_flags", "temp_file+split_by_time+discont_start"},
		{"-hls_segment_filename", "/tmp/session/video/seg_%05d.ts"},
		{"-hls_list_size", "0"},
		{"-hls_segment_filename", "/tmp/session/audio/seg_%05d.ts"},
		{"/tmp/session/video/video.m3u8"},
		{"/tmp/session/audio/audio.m3u8"},
	} {
		if !containsArguments(got, want) {
			t.Errorf("args do not contain %q: %#v", want, got)
		}
	}
}

func TestBuildAudioSessionArgs(t *testing.T) {
	spec := AudioSessionSpec{
		AudioURL:        "https://audio.test/audio.mka",
		AudioTrackIndex: 2,
		StartSegment:    7,
		StartTime:       28,
		OutputDir:       "/tmp/audio-alt",
		AudioMode:       AudioModeAAC,
		AudioLanguage:   "English",
		AudioTitle:      "English",
	}
	got, err := buildAudioSessionArgs(spec)
	if err != nil {
		t.Fatalf("buildAudioSessionArgs() error = %v", err)
	}
	for _, want := range [][]string{
		{"-ss", "28", "-icy", "0", "-i", spec.AudioURL},
		{"-map", "0:a:2"},
		{"-c:a", "aac"},
		{"-metadata:s:a:0", "language=eng"},
		{"-hls_segment_filename", "/tmp/audio-alt/audio/seg_%05d.ts"},
		{"-hls_list_size", "0"},
		{"-start_number", "7"},
		{"/tmp/audio-alt/audio/audio.m3u8"},
	} {
		if !containsArguments(got, want) {
			t.Errorf("audio args do not contain %q: %#v", want, got)
		}
	}
}

func TestBuildSessionArgsDualSource(t *testing.T) {
	spec := SessionSpec{
		VideoURL:        "https://video.test/video.mp4",
		AudioURL:        "https://audio.test/audio.mka",
		VideoTrackIndex: 0,
		AudioTrackIndex: 3,
		StartSegment:    5,
		StartTime:       20,
		OutputDir:       "/tmp/dual",
		AudioMode:       AudioMode("AAC"),
	}

	got, err := buildSessionArgs(spec)
	if err != nil {
		t.Fatalf("buildSessionArgs() error = %v", err)
	}

	if countArgument(got, "-i") != 2 {
		t.Fatalf("input count = %d, want 2; args: %#v", countArgument(got, "-i"), got)
	}
	for _, want := range [][]string{
		{"-readrate", "1", "-readrate_initial_burst", "120", "-ss", "20", "-icy", "0", "-i", spec.VideoURL},
		{"-readrate", "1", "-readrate_initial_burst", "120", "-ss", "20", "-icy", "0", "-i", spec.AudioURL},
		{"-map", "0:v:0"},
		{"-map", "1:a:3"},
		{"-c:v", "copy"},
		{"-c:a", "aac"},
		{"-hls_flags", "temp_file+split_by_time+discont_start"},
		{"/tmp/dual/video/video.m3u8"},
		{"/tmp/dual/audio/audio.m3u8"},
	} {
		if !containsArguments(got, want) {
			t.Errorf("args do not contain %q: %#v", want, got)
		}
	}
}

func TestBuildSessionArgsRejectsInvalidAudioMode(t *testing.T) {
	_, err := buildSessionArgs(SessionSpec{
		VideoURL:        "video",
		VideoTrackIndex: 0,
		AudioTrackIndex: 0,
		OutputDir:       "/tmp/out",
		AudioMode:       "mp3",
	})
	if err == nil {
		t.Fatal("buildSessionArgs() error = nil, want invalid audio mode error")
	}
}

func TestBuildSessionArgsOpeningOverlay(t *testing.T) {
	got, err := buildSessionArgs(SessionSpec{
		VideoURL:             "https://video.test/movie.mkv",
		VideoTrackIndex:      0,
		AudioTrackIndex:      0,
		OutputDir:            "/tmp/opening",
		OpeningOverlayPath:   "/assets/intro.mp4",
		OpeningOverlayHeight: 1080,
		Duration:             4 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildSessionArgs() error = %v", err)
	}
	if countArgument(got, "-i") != 2 {
		t.Fatalf("input count = %d, want film plus intro; args: %#v", countArgument(got, "-i"), got)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{"colorkey=0x050505:0.08:0.02", "amix=inputs=2", "-map [v]", "-map [a]", "-t 4", "-c:v libx264", "-c:a aac"} {
		if !strings.Contains(joined, want) {
			t.Errorf("overlay args missing %q: %s", want, joined)
		}
	}
}

func TestBuildPlaceholderArgsSupportsCardAndNonZeroStart(t *testing.T) {
	args := buildPlaceholderArgsWithOptions("placeholder.mp4", "/tmp/out", true, 7, "/tmp/card:with-colon.txt")
	if !containsArguments(args, []string{"-start_number", "7"}) {
		t.Fatalf("placeholder start number missing: %#v", args)
	}
	joined := strings.Join(args, " ")
	if (!strings.Contains(joined, "drawtext=textfile='/tmp/card\\:with-colon.txt'") && !strings.Contains(joined, "drawtext@quality=textfile='/tmp/card\\:with-colon.txt'")) || !strings.Contains(joined, "reload=1") {
		t.Fatalf("placeholder card filter missing or not escaped: %s", joined)
	}
}

func TestSessionCancelIsIdempotent(t *testing.T) {
	calls := 0
	session := &Session{cancel: func() { calls++ }}

	session.Cancel()
	session.Cancel()

	if calls != 1 {
		t.Fatalf("cancel calls = %d, want 1", calls)
	}
}

func TestTailBufferRetainsOnlyConfiguredTail(t *testing.T) {
	buffer := newTailBuffer(4)
	_, _ = buffer.Write([]byte("123"))
	_, _ = buffer.Write([]byte("456"))

	if got := buffer.String(); got != "3456" {
		t.Fatalf("tail buffer = %q, want %q", got, "3456")
	}
}

func countArgument(args []string, value string) int {
	count := 0
	for _, arg := range args {
		if arg == value {
			count++
		}
	}
	return count
}

func containsArguments(args, sequence []string) bool {
	if len(sequence) > len(args) {
		return false
	}
	for start := 0; start <= len(args)-len(sequence); start++ {
		if reflect.DeepEqual(args[start:start+len(sequence)], sequence) {
			return true
		}
	}
	return false
}

func TestBuildSessionArgsKeepsVideoAndAudioOnSameGrid(t *testing.T) {
	// The public timeline uses one segment number for video and audio, so both
	// outputs must split on the same fixed grid from the initial segment.
	spec := SessionSpec{
		VideoURL:        "https://example.test/media.mkv",
		VideoTrackIndex: 0,
		AudioTrackIndex: 0,
		OutputDir:       "/tmp/fresh",
	}
	got, err := buildSessionArgs(spec)
	if err != nil {
		t.Fatalf("buildSessionArgs() error = %v", err)
	}
	videoFlags := flagsFor(got, "/tmp/fresh/video/video.m3u8")
	audioFlags := flagsFor(got, "/tmp/fresh/audio/audio.m3u8")
	if videoFlags != "temp_file+split_by_time" {
		t.Fatalf("video flags = %q, want fixed-grid split", videoFlags)
	}
	if audioFlags != "independent_segments+temp_file+split_by_time" {
		t.Fatalf("audio flags = %q, want muxer-managed split-by-time retention", audioFlags)
	}
}

func flagsFor(args []string, playlist string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-hls_flags" {
			// associate flags with the following playlist path
			for j := i + 2; j < len(args); j++ {
				if args[j] == playlist {
					return args[i+1]
				}
				if args[j] == "-hls_flags" {
					break
				}
			}
		}
	}
	return ""
}
