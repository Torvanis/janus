// Package web embeds the built single-page application into the gateway binary
// so a deployment is one artefact with no static-file sidecar.
package web

import (
	"embed"
	"fmt"
	"io/fs"
)

// dist holds the Vite build output. The `all:` prefix keeps files whose names
// begin with an underscore or dot, which asset hashing can produce.
//
//go:embed all:dist
var dist embed.FS

// Assets returns the built web interface rooted at dist/, or an error when the
// bundle has not been built yet.
func Assets() (fs.FS, error) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, fmt.Errorf("open embedded web assets: %w", err)
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, fmt.Errorf("web bundle is missing index.html; run `npm --prefix web ci && npm --prefix web run build`")
	}
	return sub, nil
}
