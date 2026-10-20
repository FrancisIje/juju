package server

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"juju/internal/alert"
	"juju/internal/auth"
	"juju/internal/config"
)

// Go's router only rejects conflicting patterns when they're registered,
// at run time. Building the routes here catches that in CI, and checks the
// dashboard catch-all doesn't swallow API paths.
func TestRoutes(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := Routes(Deps{Auth: &auth.Service{}, Alerts: &alert.Notifier{Log: log}, Config: config.Config{PublicURL: "http://localhost:8080"}, Log: log})

	get := func(path string) (int, string, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Code, rec.Header().Get("Content-Type"), rec.Body.String()
	}
	if code, _, body := get("/p/3/issues/42"); code != 200 || !strings.Contains(body, "<html") {
		t.Errorf("a dashboard URL must serve the app: %d", code)
	}
	if code, ct, _ := get("/api/nope"); code != 404 || !strings.Contains(ct, "json") {
		t.Errorf("an unknown API path must be a JSON 404, not the app: %d %s", code, ct)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", csp)
	}
	if rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("index.html must not be cached: %q", rec.Header().Get("Cache-Control"))
	}
}
