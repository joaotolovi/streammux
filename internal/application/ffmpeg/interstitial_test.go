package ffmpeg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGenerateInterstitial(t *testing.T) {
	placeholder := "testdata/placeholder.mp4"
	// fallback to actual asset path
	if _, err := os.Stat(placeholder); err != nil {
		placeholder = "../assets/placeholder.mp4"
		if _, err := os.Stat(placeholder); err != nil {
			t.Skip("placeholder.mp4 not found")
		}
	}
	// Also try internal/application/assets/placeholder.mp4 from repo root
	if _, err := os.Stat(placeholder); err != nil {
		placeholder = "../../assets/placeholder.mp4"
	}
	// Direct check for the real file used in production
	if _, err := os.Stat(placeholder); err != nil {
		// Try absolute from project root
		placeholder = "/home/joao/streammux/internal/application/assets/placeholder.mp4"
		if _, err := os.Stat(placeholder); err != nil {
			t.Skip("placeholder.mp4 not found")
		}
	}
	dir := t.TempDir()
	m := New("ffmpeg")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.GenerateInterstitial(ctx, placeholder, dir, 8*time.Second); err != nil {
		t.Fatalf("GenerateInterstitial: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "intro.m3u8")); err != nil {
		t.Fatalf("intro.m3u8 missing: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "intro.m3u8"))
	if len(data) == 0 {
		t.Fatal("intro.m3u8 empty")
	}
	if !strings.Contains(string(data), "#EXT-X-ENDLIST") {
		t.Fatalf("intro.m3u8 should have ENDLIST: %s", string(data))
	}
}
