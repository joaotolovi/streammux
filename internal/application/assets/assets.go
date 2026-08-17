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

//go:embed intro.mp4 loop.mp4 placeholder.mp4
var files embed.FS

// PlaceholderPaths extracts the embedded intro and loop videos to a temporary
// directory and returns their paths. The caller is responsible for removing
// the returned directory when done. placeholder.mp4 is the unified 120s
// intro+loop video (max wait); intro/loop are kept for backwards compat.
func PlaceholderPaths() (introPath, loopPath, dir string, err error) {
	dir, err = os.MkdirTemp("", "streammux-assets-*")
	if err != nil {
		return "", "", "", fmt.Errorf("assets temp dir: %w", err)
	}
	introPath, err = extract("intro.mp4", dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", "", err
	}
	loopPath, err = extract("loop.mp4", dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", "", err
	}
	return introPath, loopPath, dir, nil
}

// Placeholder120Path extracts the unified 120s placeholder (intro + loop,
// trimmed to exactly 120s — the max wait before we error) to a temp dir.
func Placeholder120Path() (path, dir string, err error) {
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
