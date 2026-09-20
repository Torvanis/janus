package httpapi

import (
	"bytes"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// serveWebApp serves the built single-page application. Unknown paths fall back
// to index.html so client-side routes deep-link and survive a refresh; unknown
// API paths still return a JSON 404 envelope rather than HTML.
func (s *Server) serveWebApp(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/v1/") {
		WriteError(w, r, ErrNotFoundf("That endpoint"))
		return
	}
	if s.WebAssets == nil {
		WriteJSON(w, http.StatusOK, map[string]any{
			"service": "janus",
			"detail":  "The web interface is not bundled into this binary. Build it with `npm --prefix web ci && npm --prefix web run build`, then rebuild the gateway.",
		})
		return
	}

	clean := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if clean != "" && clean != "index.html" {
		if data, err := fs.ReadFile(s.WebAssets, clean); err == nil {
			// Fingerprinted bundles are immutable; everything else revalidates.
			if strings.HasPrefix(clean, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			if ctype := mime.TypeByExtension(filepath.Ext(clean)); ctype != "" {
				w.Header().Set("Content-Type", ctype)
			}
			http.ServeContent(w, r, clean, time.Time{}, bytes.NewReader(data))
			return
		}
	}

	index, err := fs.ReadFile(s.WebAssets, "index.html")
	if err != nil {
		WriteError(w, r, ErrNotFoundf("That page"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(index); err != nil {
		s.Logger.Warn("write index.html", "error", err.Error())
	}
}
