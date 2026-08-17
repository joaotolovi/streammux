// Package assets embeds the default placeholder videos (intro + loop) so the
// server can play them immediately while the real film is prepared, without
// requiring external files. The ffmpeg placeholder session needs real file
// paths, so the embedded videos are extracted to a temp dir on demand.
package assets

import (
	"embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

//go:embed placeholder.mp4
var files embed.FS

// PlaceholderPath extracts the unified 120s placeholder (intro crossfaded into
// loop, 120s total — the max wait before we error) to a temp dir.
func PlaceholderPath() (path, dir string, err error) {
	dir, err = os.MkdirTemp("", "streammux-assets-*")
	if err != nil {
		return "", "", fmt.Errorf("assets temp dir: %w", err)
	}
	path, err = extract("placeholder.mp4", dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	return path, dir, nil
}

func extract(name, dir string) (string, error) {
	src, err := files.Open(name)
	if err != nil {
		return "", fmt.Errorf("open embedded %s: %w", name, err)
	}
	defer src.Close()

	dst := filepath.Join(dir, name)
	out, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", dst, err)
	}
	defer out.Close()

	if _, err := io.Copy(out, src); err != nil {
		return "", fmt.Errorf("write %s: %w", dst, err)
	}
	return dst, nil
}
