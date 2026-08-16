package ffmpeg

import (
	"reflect"
	"testing"
)

func TestBuildSessionArgsSingleSource(t *testing.T) {
	spec := SessionSpec{
		VideoURL:        "https://example.test/media.mkv",
		AudioURL:        "https://example.test/media.mkv",
		VideoTrackIndex: 1,
		AudioTrackIndex: 2,
		StartSegment:    3,
		OutputDir:       "/tmp/session",
		AudioMode:       AudioModeCopy,
		AudioLanguage:   "pt-BR",
		AudioTitle:      "Português",
	}

	got, err := buildSessionArgs(spec)
	if err != nil {
		t.Fatalf("buildSessionArgs() error = %v", err)
	}

	want := []string{
		"-nostdin",
		"-hide_banner",
		"-nostats",
		"-stats_period", "1",
		"-progress", "pipe:1",
		"-y",
		"-ss", "12",
		"-i", "https://example.test/media.mkv",
		"-map", "0:v:1",
		"-map", "0:a:2",
		"-c:v", "copy",
		"-c:a", "copy",
		"-metadata:s:a:0", "language=por",
		"-disposition:a:0", "default",
		"-metadata:s:a:0", "title=Português",
		"-shortest",
		"-avoid_negative_ts", "make_zero",
		"-f", "hls",
		"-hls_time", "4",
		"-hls_playlist_type", "event",
		"-hls_flags", "independent_segments+temp_file+discont_start",
		"-hls_segment_filename", "/tmp/session/seg_%05d.ts",
		"-start_number", "3",
		"/tmp/session/live.m3u8",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildSessionArgs() mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestBuildSessionArgsDualSource(t *testing.T) {
	spec := SessionSpec{
		VideoURL:        "https://video.test/video.mp4",
		AudioURL:        "https://audio.test/audio.mka",
		VideoTrackIndex: 0,
		AudioTrackIndex: 3,
		StartSegment:    5,
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
		{"-ss", "20", "-i", spec.VideoURL},
		{"-ss", "20", "-i", spec.AudioURL},
		{"-map", "0:v:0"},
		{"-map", "1:a:3"},
		{"-c:v", "copy"},
		{"-c:a", "aac"},
		{"-shortest"},
		{"-hls_flags", "independent_segments+temp_file+discont_start"},
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
