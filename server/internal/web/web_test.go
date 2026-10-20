package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

var built = fstest.MapFS{
	"index.html":         {Data: []byte("<html>app</html>")},
	"placeholder.html":   {Data: []byte("<html>run make dashboard</html>")},
	"favicon.svg":        {Data: []byte("<svg/>")},
	"assets/index-a1.js": {Data: []byte("console.log(1)")},
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestServesTheApp(t *testing.T) {
	h := handler(built)
	tests := []struct {
		name, path string
		code       int
		body       string // substring
		cache      string
	}{
		{"root", "/", 200, "app", "no-cache"},
		// Page URLs aren't files: the app reads the URL and shows the page.
		{"deep link", "/p/3/issues/42", 200, "app", "no-cache"},
		{"settings tab", "/p/3/settings/alerts", 200, "app", "no-cache"},
		{"hashed asset", "/assets/index-a1.js", 200, "console.log", "immutable"},
		{"other file", "/favicon.svg", 200, "<svg/>", ""},
		// A tab still running an old build asks for old asset names.
		{"missing asset", "/assets/index-OLD.js", 404, "not found", ""},
		{"missing file", "/robots.txt", 404, "not found", ""},
		// Go refuses paths with ".." outright, before touching any file.
		{"path tricks stay inside dist", "/../../etc/passwd", 400, "invalid URL path", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := get(t, h, tc.path)
			if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.body) {
				t.Fatalf("%s → %d %q, want %d containing %q", tc.path, w.Code, w.Body.String(), tc.code, tc.body)
			}
			if !strings.Contains(w.Header().Get("Cache-Control"), tc.cache) {
				t.Errorf("Cache-Control = %q, want %q", w.Header().Get("Cache-Control"), tc.cache)
			}
		})
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := handler(built)
	for _, p := range []string{"/", "/p/1/issues", "/assets/index-a1.js", "/assets/nope.js"} {
		w := get(t, h, p)
		csp := w.Header().Get("Content-Security-Policy")
		// No third-party scripts or fonts, and no framing (clickjacking).
		if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: CSP = %q", p, csp)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing nosniff", p)
		}
	}
}

func TestPlaceholderBeforeTheFirstBuild(t *testing.T) {
	h := handler(fstest.MapFS{"placeholder.html": {Data: []byte("run make dashboard")}})
	w := get(t, h, "/p/1/issues")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "make dashboard") {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
}

// The committed placeholder must always be embedded, even in a fresh clone
// where nothing was built.
func TestEmbedsPlaceholder(t *testing.T) {
	if _, err := files.Open("dist/placeholder.html"); err != nil {
		t.Fatal(err)
	}
}
