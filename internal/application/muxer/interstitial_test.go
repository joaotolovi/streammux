package muxer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/streammux/streammux/internal/domain/model"
)

func TestInterstitialInjection(t *testing.T) {
	dir := t.TempDir()
	interDir := filepath.Join(dir, "interstitial")
	if err := os.MkdirAll(interDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(interDir, "intro.m3u8"), []byte("#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nseg_00000.ts\n#EXT-X-ENDLIST\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(interDir, "seg_00000.ts"), []byte("dummy"), 0644); err != nil {
		t.Fatal(err)
	}
	state := &playbackState{
		cacheDir:          dir,
		duration:          100,
		filmBase:          0,
		interstitialReady: true,
		active:            &generation{dir: dir},
	}
	mux := &Muxer{
		placeholderPath: "placeholder.mp4",
		policy:          defaultPolicy(),
		states:          map[string]*playbackState{"job": state},
	}
	job := &model.MuxJob{ID: "job", CacheDir: dir}
	data, ok := mux.VideoPlaylist(job, 0)
	if !ok {
		t.Fatal("VideoPlaylist returned false")
	}
	playlist := string(data)
	if !strings.Contains(playlist, "com.apple.hls.interstitial") {
		t.Fatalf("playlist missing interstitial DATERANGE: %s", playlist)
	}
	if !strings.Contains(playlist, "https://samplelib.com/mp4/sample-5s.mp4") {
		t.Fatalf("playlist missing asset URI: %s", playlist)
	}
	if !strings.Contains(playlist, "#EXT-X-VERSION:9") {
		t.Fatalf("playlist should be version 9 when interstitial present: %s", playlist)
	}
	if !strings.Contains(playlist, "DURATION=5.000") {
		t.Fatalf("playlist missing duration: %s", playlist)
	}
}

func TestInterstitialNotInjectedWhenNotReady(t *testing.T) {
	dir := t.TempDir()
	state := &playbackState{
		cacheDir: dir,
		duration: 100,
		filmBase: 0,
		active:   &generation{dir: dir},
	}
	mux := &Muxer{
		placeholderPath: "",
		policy:          defaultPolicy(),
		states:          map[string]*playbackState{"job": state},
	}
	job := &model.MuxJob{ID: "job"}
	data, ok := mux.VideoPlaylist(job, 0)
	if !ok {
		t.Fatal("VideoPlaylist returned false")
	}
	playlist := string(data)
	if strings.Contains(playlist, "com.apple.hls.interstitial") {
		t.Fatalf("playlist should not contain interstitial when placeholder disabled: %s", playlist)
	}
	if strings.Contains(playlist, "#EXT-X-VERSION:9") {
		t.Fatalf("playlist version should be 6 when no interstitial: %s", playlist)
	}
}

func TestInterstitialFilePath(t *testing.T) {
	dir := t.TempDir()
	interDir := filepath.Join(dir, "interstitial")
	if err := os.MkdirAll(interDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(interDir, "intro.m3u8"), []byte("playlist"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(interDir, "seg_00000.ts"), []byte("segment"), 0644); err != nil {
		t.Fatal(err)
	}
	state := &playbackState{cacheDir: dir}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	if got := mux.InterstitialFilePath("job", "intro.m3u8"); got != filepath.Join(interDir, "intro.m3u8") {
		t.Fatalf("intro path = %q", got)
	}
	if got := mux.InterstitialFilePath("job", "seg_00000.ts"); got != filepath.Join(interDir, "seg_00000.ts") {
		t.Fatalf("seg path = %q", got)
	}
	if got := mux.InterstitialFilePath("job", "../etc/passwd"); got != "" {
		t.Fatalf("path traversal should be blocked, got %q", got)
	}
	if got := mux.InterstitialFilePath("job", "seg_00001.ts"); got != "" {
		t.Fatalf("missing segment should return empty, got %q", got)
	}
}
