package ffmpeg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildImagePlaceholderArgsContainsExpectedInputsAndFilter(t *testing.T) {
	args := buildImagePlaceholderArgs("placeholder.mp4", "poster.jpg", "/tmp/out", true)

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "placeholder.mp4") {
		t.Error("missing placeholder video input")
	}
	if !strings.Contains(joined, "poster.jpg") {
		t.Error("missing poster image input")
	}
	if !strings.Contains(joined, "poster_round_mask.png") {
		t.Error("missing mask asset input")
	}
	if !strings.Contains(joined, "poster_round_border.png") {
		t.Error("missing border asset input")
	}
	if !strings.Contains(joined, "filter_complex") {
		t.Error("missing filter_complex flag")
	}
	if !strings.Contains(joined, "alphamerge") {
		t.Error("missing alphamerge filter for rounded corners")
	}
	if !strings.Contains(joined, "overlay") {
		t.Error("missing overlay filters")
	}
}

func TestFindAssetFindsExistingFile(t *testing.T) {
	dir := t.TempDir()
	placeholderPath := filepath.Join(dir, "placeholder.mp4")
	_ = os.WriteFile(placeholderPath, []byte("video"), 0644)

	maskPath := filepath.Join(dir, "poster_round_mask.png")
	_ = os.WriteFile(maskPath, []byte("mask"), 0644)

	got := findAsset(placeholderPath, "poster_round_mask.png")
	if got != maskPath {
		t.Fatalf("findAsset() = %q, want %q", got, maskPath)
	}
}

func TestFindAssetFallsBackToName(t *testing.T) {
	dir := t.TempDir()
	placeholderPath := filepath.Join(dir, "placeholder.mp4")
	got := findAsset(placeholderPath, "missing.png")
	if got != "missing.png" {
		t.Fatalf("findAsset() = %q, want 'missing.png'", got)
	}
}
