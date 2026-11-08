// Package web embeds the client built from web/ by Vite.
package web

import (
	"embed"
	"io/fs"
)

// assets/dist is produced by `make web`. assets/fallback is checked in, so the
// pattern always matches and `go build` works without a Node toolchain.
//
//go:embed all:assets
var assets embed.FS

// FS returns the built client, or a placeholder page explaining how to build
// it when the binary was compiled without running the Vite build.
func FS() fs.FS {
	if dist, err := fs.Sub(assets, "assets/dist"); err == nil {
		if _, err := fs.Stat(dist, "index.html"); err == nil {
			return dist
		}
	}
	fallback, err := fs.Sub(assets, "assets/fallback")
	if err != nil {
		panic(err)
	}
	return fallback
}
