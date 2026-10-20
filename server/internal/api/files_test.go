package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"juju/internal/blob"
	"juju/internal/store"
)

func filesAPI(t *testing.T, maxUpload int64) (*http.ServeMux, *fakeStore) {
	t.Helper()
	blobs, err := blob.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeStore{}
	mux := http.NewServeMux()
	(&API{Store: fs, Blobs: blobs, MaxUpload: maxUpload, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Register(mux)
	return mux, fs
}

func upload(mux *http.ServeMux, query string, body []byte, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/v1/projects/1/files?"+query, bytes.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func testdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../symbolicate/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUploadSourceMap(t *testing.T) {
	mux, fs := filesAPI(t, 10<<20)
	rec := upload(mux, "kind=sourcemap&release=web@1.0.0&name=app.min.js", testdata(t, "app.min.js.map"), "Bearer sk_good")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var f store.DebugFile
	json.Unmarshal(rec.Body.Bytes(), &f)
	if f.Kind != "sourcemap" || f.Release != "web@1.0.0" || f.Name != "app.min.js" || len(f.SHA256) != 64 || f.Size != 1067 {
		t.Fatalf("saved %+v", f)
	}
	if len(fs.files) != 1 {
		t.Fatal("not recorded")
	}
}

func TestUploadDartSymbolsIsStoredUnderItsBuildID(t *testing.T) {
	mux, fs := filesAPI(t, 10<<20)
	rec := upload(mux, "kind=dart_symbols&name=ignored", testdata(t, "dart_macos_arm64.debug"), "Bearer sk_good")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := fs.files[0]; got.Name != "b4485c2e9c56418697e466d0c2ca3195" || got.Release != "" {
		t.Fatalf("saved %+v: the build id must come from the file, not the request", got)
	}
}

func TestUploadRejects(t *testing.T) {
	mux, _ := filesAPI(t, 100)
	cases := []struct {
		name, query string
		body        []byte
		auth        string
		status      int
		msg         string
	}{
		{"no secret key", "kind=sourcemap&release=r&name=a.js", []byte("{}"), "", 401, "Authorization"},
		{"unknown kind", "kind=zip", []byte("x"), "Bearer sk_good", 400, "kind must be"},
		{"source map without release", "kind=sourcemap&name=a.js", []byte("{}"), "Bearer sk_good", 400, "needs release"},
		{"not a source map", "kind=sourcemap&release=r&name=a.js", []byte("hello"), "Bearer sk_good", 400, "not a valid source map"},
		{"not a debug file", "kind=dart_symbols", []byte("hello"), "Bearer sk_good", 400, "ELF"},
		{"over the limit", "kind=sourcemap&release=r&name=a.js", bytes.Repeat([]byte("x"), 101), "Bearer sk_good", 413, "upload limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := upload(mux, tc.query, tc.body, tc.auth)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.msg) {
				t.Errorf("got %d %s, want %d containing %q", rec.Code, rec.Body, tc.status, tc.msg)
			}
		})
	}
}

func TestListFiles(t *testing.T) {
	mux, _ := filesAPI(t, 10<<20)
	upload(mux, "kind=sourcemap&release=web@1&name=a.js", testdata(t, "app.min.js.map"), "Bearer sk_good")
	upload(mux, "kind=sourcemap&release=web@2&name=a.js", testdata(t, "app.min.js.map"), "Bearer sk_good")
	req := httptest.NewRequest("GET", "/api/v1/projects/1/files?release=web@2", nil)
	req.Header.Set("Authorization", "Bearer sk_good")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var body struct{ Files []store.DebugFile }
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || len(body.Files) != 1 || body.Files[0].Release != "web@2" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}
