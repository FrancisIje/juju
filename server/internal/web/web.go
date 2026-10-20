// Package web serves the dashboard: the built single-page app in dist/,
// compiled into the binary so a deploy is still one file.
//
// `make dashboard` builds dashboard/ and copies the result here. Until then
// the committed dist/placeholder.html is served, saying how to build it.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var files embed.FS

// Handler serves the dashboard. Any path that isn't a real file gets
// index.html: the app itself reads the URL and shows the right page
// (/p/1/issues/42 is a page, not a file).
func Handler() http.Handler {
	dist, _ := fs.Sub(files, "dist")
	return handler(dist)
}

// handler takes the files as a parameter so tests can pass their own.
func handler(dist fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w.Header())
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "index.html" {
			if f, err := dist.Open(name); err == nil {
				f.Close()
				if strings.HasPrefix(name, "assets/") {
					// Vite puts a content hash in these names, so a new build
					// never reuses one: browsers can keep them forever.
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
			// A missing file (it has an extension) is a 404, not the app.
			// After a deploy, a tab still running the old build asks for
			// old asset names; answering with HTML would fail as a confusing
			// "wrong MIME type" error instead of a plain not found.
			if path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
		}
		// index.html must never be cached, or a deploy wouldn't reach
		// people until their cache expired.
		w.Header().Set("Cache-Control", "no-cache")
		page := "index.html"
		if _, err := fs.Stat(dist, page); err != nil {
			page = "placeholder.html" // the dashboard hasn't been built
		}
		http.ServeFileFS(w, r, dist, page)
	})
}

// securityHeaders lock the dashboard down: only our own scripts, styles and
// fonts may load (that's why fonts are bundled, not fetched from Google),
// no other site can frame it (clickjacking), and browsers mustn't guess
// file types.
func securityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
}
