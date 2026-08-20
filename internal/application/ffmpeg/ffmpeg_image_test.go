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
	if !strings.Contains(joined, "scale=360:180") {
		t.Error("missing logo scale")
	}
	if !strings.Contains(joined, "filter_complex") {
		t.Error("missing filter_complex flag")
	}
	if !strings.Contains(joined, "overlay") {
		t.Error("missing overlay filters")
	}
}

func TestBuildImagePlaceholderArgsBlendsCinemetaBackground(t *testing.T) {
	args := buildImagePlaceholderArgsWithBackground("placeholder.mp4", "logo.png", "background.jpg", "/tmp/out", true, 0, "", "", "")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "background.jpg") {
		t.Error("missing Cinemeta background input")
	}
	if !strings.Contains(joined, "colorchannelmixer=aa=0.35") || !strings.Contains(joined, "fade=t=in:st=5:d=0.8:alpha=1") {
		t.Error("missing timed background blend")
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

func TestShiftExprPosterStopsAtEndX(t *testing.T) {
	// Poster slides from 1280 to 947 between t=2.5 and t=3.3.
	expr := shiftExpr(2.5, 0.8, 1280-947, (1280-947)/0.8, true, 947)
	// The condition must be gt(t,2.50) — the animation START time — so the
	// poster begins moving at t=2.5 and clamps at 947 after t=3.3.
	if !strings.Contains(expr, "gt(t,2.50)") {
		t.Fatalf("expected gt(t,2.50) in expression, got %s", expr)
	}
	// Must use max() not min() so the poster can start at 1280 (above endX)
	// and slide down to 947, then clamp there.
	if !strings.Contains(expr, "max(947") {
		t.Fatalf("expected max(947,...) to clamp at endX, got %s", expr)
	}
}

func TestShiftExprVideoStopsAtEndX(t *testing.T) {
	// Video shifts from 0 to -140 between t=2.5 and t=3.3.
	expr := shiftExpr(2.5, 0.8, -140, 140/0.8, false, -140)
	if !strings.Contains(expr, "gt(t,2.50)") {
		t.Fatalf("expected gt(t,2.50) in expression, got %s", expr)
	}
	if !strings.Contains(expr, "max(-140") {
		t.Fatalf("expected max(-140,...) to clamp at endX, got %s", expr)
	}
}

func TestBuildImagePlaceholderArgsUsesLogoFade(t *testing.T) {
	args := buildImagePlaceholderArgs("placeholder.mp4", "poster.jpg", "/tmp/out", true)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "fade=t=in:st=5.00:d=0.80") {
		t.Error("missing logo fade-in")
	}
	if strings.Contains(joined, "poster_round_mask.png") || strings.Contains(joined, "poster_round_border.png") {
		t.Error("poster mask or border should not be used for logo")
	}
}
