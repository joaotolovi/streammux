// Package web embeds the built frontend assets.
package web

import (
	"embed"
	"io/fs"
)

// Dist embeds the built Vite frontend.
//
//go:embed all:dist
var Dist embed.FS

// Sub returns the dist subtree as an fs.FS.
func Sub() (fs.FS, error) {
	return fs.Sub(Dist, "dist")
}
