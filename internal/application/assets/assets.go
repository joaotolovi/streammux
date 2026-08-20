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

//go:embed placeholder.mp4 error.mp4 loading_spinner.gif poster_round_mask.png poster_round_border.png
var files embed.FS

// PlaceholderPath extracts the default placeholder and its overlay mask assets
// to a temporary directory. The caller should clean up the directory when done.
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
	if err := extractAll(dir, []string{"loading_spinner.gif", "poster_round_mask.png", "poster_round_border.png"}); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	return path, dir, nil
}

func ErrorPath() (path, dir string, err error) {
	dir, err = os.MkdirTemp("", "streammux-assets-*")
	if err != nil {
		return "", "", fmt.Errorf("assets temp dir: %w", err)
	}
	path, err = extract("error.mp4", dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	return path, dir, nil
}

func extractAll(dir string, names []string) error {
	for _, name := range names {
		if _, err := extract(name, dir); err != nil {
			return err
		}
	}
	return nil
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
