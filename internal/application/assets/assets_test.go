package assets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPlaceholderPathExtractsMaskAssets(t *testing.T) {
	placeholderPath, dir, err := PlaceholderPath()
	if err != nil {
		t.Fatalf("PlaceholderPath() error = %v", err)
	}
	defer os.RemoveAll(dir)

	if placeholderPath == "" {
		t.Fatal("placeholder path is empty")
	}

	for _, name := range []string{"placeholder.mp4", "poster_round_mask.png", "poster_round_border.png"} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
}
