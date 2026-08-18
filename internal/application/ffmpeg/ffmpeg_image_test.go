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

func TestShiftExprPosterStopsAtEndX(t *testing.T) {
	// Poster slides from 1280 to 947 between t=2.5 and t=3.3.
	expr := shiftExpr(2.5, 0.8, 1280-947, (1280-947)/0.8, true, 947)
	// The condition must be gt(t,2.50) — the animation START time — so the
	// poster begins moving at t=2.5 and clamps at 947 after t=3.3.
	if !strings.Contains(expr, "gt(t,2.50)") {
		t.Fatalf("expected gt(t,2.50) in expression, got %s", expr)
	}
	if !strings.Contains(expr, "min(947") {
		t.Fatalf("expected min(947,...) to clamp at endX, got %s", expr)
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

func TestBuildImagePlaceholderArgsScalesMaskAndBorder(t *testing.T) {
	args := buildImagePlaceholderArgs("placeholder.mp4", "poster.jpg", "/tmp/out", true)
	joined := strings.Join(args, " ")
	// The mask and border PNGs are 320x480 but the poster is 256x384 (20%
	// smaller), so they must be scaled in the filter chain.
	if !strings.Contains(joined, "[2:v]scale=256:384") {
		t.Error("missing mask scale to 256x384")
	}
	if !strings.Contains(joined, "[3:v]scale=256:384") {
		t.Error("missing border scale to 256x384")
	}
}
