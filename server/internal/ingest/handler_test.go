package ingest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"juju/internal/event"
	"juju/internal/store"
)

const key = "pk_test"

// fakeKeys and fakeQueue stand in for Postgres and the channel, so these
// tests check only the handler's own logic, in milliseconds.
type fakeKeys map[int64]string

func (f fakeKeys) ProjectPublicKey(_ context.Context, id int64) (string, error) {
	k, ok := f[id]
	if !ok {
		return "", store.ErrNotFound
	}
	return k, nil
}

type fakeQueue struct {
	events []*event.Event
	room   int
}

func (q *fakeQueue) TryEnqueue(e *event.Event) bool {
	if len(q.events) >= q.room {
		return false
	}
	q.events = append(q.events, e)
	return true
}

func newMux(q *fakeQueue) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/projects/{id}/events", &Handler{
		Keys:  fakeKeys{1: key},
		Queue: q,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:   func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) },
	})
	return mux
}

func goodEvent(id string) string {
	return fmt.Sprintf(`{"event_id":%q,"type":"error","platform":"javascript","exception":{"type":"TypeError","message":"x is undefined"}}`, id)
}

func envelope(events ...string) string {
	return `{"sdk":{"name":"juju.test","version":"0"},"events":[` + strings.Join(events, ",") + `]}`
}

const (
	id1 = "11111111-1111-4111-8111-111111111111"
	id2 = "22222222-2222-4222-8222-222222222222"
)

func gzipped(t *testing.T, s []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(s)
	gz.Close()
	return buf.Bytes()
}

func TestIngest(t *testing.T) {
	big := envelope(goodEvent(id1)) + strings.Repeat(" ", event.MaxBodyBytes)
	// 2 MB of spaces compresses to a few KB: a small gzip bomb.
	bomb := gzipped(t, []byte(big+strings.Repeat(" ", event.MaxBodyBytes)))

	tests := []struct {
		name       string
		method     string
		path       string
		headers    map[string]string
		body       []byte
		queueRoom  int
		wantStatus int
		wantBody   string // substring
		wantQueued int
	}{
		{name: "valid event is queued", body: []byte(envelope(goodEvent(id1))), wantStatus: 202, wantBody: `"accepted":1`, wantQueued: 1},
		{name: "gzip body", headers: map[string]string{"Content-Encoding": "gzip"}, body: gzipped(t, []byte(envelope(goodEvent(id1)))), wantStatus: 202, wantQueued: 1},
		{name: "charset in content type is fine", headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}, body: []byte(envelope(goodEvent(id1))), wantStatus: 202, wantQueued: 1},
		{name: "partial batch: good kept, bad dropped", body: []byte(envelope(goodEvent(id1), `{"event_id":"nope","type":"error"}`, goodEvent(id2))),
			wantStatus: 202, wantBody: `"accepted":2,"dropped":[{"index":1,"reason":"event_id: must be a UUID`, wantQueued: 2},
		{name: "every event bad", body: []byte(envelope(`{"type":"error"}`)), wantStatus: 400, wantBody: `"accepted":0`},

		{name: "GET is not allowed", method: "GET", wantStatus: 405},
		{name: "non-numeric project", path: "/api/v1/projects/abc/events", body: []byte(envelope(goodEvent(id1))), wantStatus: 404},
		{name: "missing key", headers: map[string]string{"X-Juju-Key": ""}, body: []byte(envelope(goodEvent(id1))), wantStatus: 401},
		{name: "wrong key", headers: map[string]string{"X-Juju-Key": "pk_wrong"}, body: []byte(envelope(goodEvent(id1))), wantStatus: 401},
		{name: "unknown project looks like a wrong key", path: "/api/v1/projects/999/events", body: []byte(envelope(goodEvent(id1))), wantStatus: 401, wantBody: "invalid project key"},
		{name: "wrong content type", headers: map[string]string{"Content-Type": "application/xml"}, body: []byte(envelope(goodEvent(id1))), wantStatus: 415},
		{name: "text/plain is accepted (sendBeacon)", headers: map[string]string{"Content-Type": "text/plain;charset=UTF-8"}, body: []byte(envelope(goodEvent(id1))), wantStatus: 202, wantQueued: 1},
		{name: "key in query string (sendBeacon)", path: "/api/v1/projects/1/events?juju_key=" + key, headers: map[string]string{"X-Juju-Key": ""}, body: []byte(envelope(goodEvent(id1))), wantStatus: 202, wantQueued: 1},
		{name: "wrong key in query string", path: "/api/v1/projects/1/events?juju_key=pk_wrong", headers: map[string]string{"X-Juju-Key": ""}, body: []byte(envelope(goodEvent(id1))), wantStatus: 401},
		{name: "unsupported encoding", headers: map[string]string{"Content-Encoding": "br"}, body: []byte(envelope(goodEvent(id1))), wantStatus: 415},
		{name: "body over 1 MB", body: []byte(big), wantStatus: 413},
		{name: "gzip bomb over 1 MB decompressed", headers: map[string]string{"Content-Encoding": "gzip"}, body: bomb, wantStatus: 413},
		{name: "corrupt gzip", headers: map[string]string{"Content-Encoding": "gzip"}, body: []byte("not gzip"), wantStatus: 400},
		{name: "malformed json", body: []byte(`{"sdk":`), wantStatus: 400, wantBody: "invalid JSON"},
		{name: "empty batch", body: []byte(`{"sdk":{"name":"x"},"events":[]}`), wantStatus: 400},

		{name: "queue full asks SDK to retry", body: []byte(envelope(goodEvent(id1))), queueRoom: -1, wantStatus: 429},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			method, path := "POST", "/api/v1/projects/1/events"
			if tc.method != "" {
				method = tc.method
			}
			if tc.path != "" {
				path = tc.path
			}
			req := httptest.NewRequest(method, path, bytes.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Juju-Key", key)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			q := &fakeQueue{room: 10}
			if tc.queueRoom != 0 {
				q.room = max(tc.queueRoom, 0)
			}
			rec := httptest.NewRecorder()
			newMux(q).ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body: %s", rec.Code, tc.wantStatus, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body %s does not contain %s", rec.Body, tc.wantBody)
			}
			if len(q.events) != tc.wantQueued {
				t.Errorf("queued %d events, want %d", len(q.events), tc.wantQueued)
			}
		})
	}
}

func TestIngestSetsServerFields(t *testing.T) {
	q := &fakeQueue{room: 10}
	req := httptest.NewRequest("POST", "/api/v1/projects/1/events", strings.NewReader(envelope(goodEvent(id1))))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Juju-Key", key)
	rec := httptest.NewRecorder()
	newMux(q).ServeHTTP(rec, req)

	if rec.Code != 202 || len(q.events) != 1 {
		t.Fatalf("status %d, queued %d", rec.Code, len(q.events))
	}
	e := q.events[0]
	if e.ProjectID != 1 || e.SDK.Name != "juju.test" {
		t.Errorf("project %d sdk %q: the handler must stamp these, not the client", e.ProjectID, e.SDK.Name)
	}
	var resp Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Dropped == nil {
		t.Errorf("dropped should encode as [] not null: %s", rec.Body)
	}
}

// The status-code test above can't tell whether we stopped reading at 1 MB or
// inflated everything and *then* noticed it was too big. Both return 413, but
// only the first survives a real bomb. So measure memory directly: a 100 MB
// bomb must not cost anywhere near 100 MB.
func TestGzipBombIsNotInflated(t *testing.T) {
	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	zeros := make([]byte, 1<<20)
	for range 100 {
		gz.Write(zeros)
	}
	gz.Close()
	bomb := buf.Bytes() // about 100 KB on the wire

	req := httptest.NewRequest("POST", "/api/v1/projects/1/events", bytes.NewReader(bomb))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("X-Juju-Key", key)
	rec := httptest.NewRecorder()
	mux := newMux(&fakeQueue{room: 10})

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	mux.ServeHTTP(rec, req)
	runtime.ReadMemStats(&after)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 16<<20 {
		t.Fatalf("allocated %d MB for a %d KB request: the body is being inflated past the limit", allocated>>20, len(bomb)>>10)
	}
}

// Without these headers the browser hides the response from the SDK, even
// though the server processed the request. That includes error responses:
// the SDK must be able to see a 401 or 429 to react to it.
func TestCORSHeadersOnEveryResponse(t *testing.T) {
	for _, k := range []string{key, "pk_wrong"} {
		req := httptest.NewRequest("POST", "/api/v1/projects/1/events", strings.NewReader(envelope(goodEvent(id1))))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Juju-Key", k)
		rec := httptest.NewRecorder()
		newMux(&fakeQueue{room: 10}).ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("status %d: Access-Control-Allow-Origin = %q, want *", rec.Code, got)
		}
		if got := rec.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, "Retry-After") {
			t.Errorf("status %d: Retry-After not exposed to scripts", rec.Code)
		}
	}
}

func TestPreflight(t *testing.T) {
	rec := httptest.NewRecorder()
	Preflight(rec, httptest.NewRequest("OPTIONS", "/api/v1/projects/1/events", nil))
	h := rec.Header()
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if !strings.Contains(h.Get("Access-Control-Allow-Headers"), "X-Juju-Key") {
		t.Errorf("Allow-Headers = %q: the browser would refuse to send X-Juju-Key", h.Get("Access-Control-Allow-Headers"))
	}
	if h.Get("Access-Control-Allow-Methods") != "POST" || h.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("headers = %v", h)
	}
}
