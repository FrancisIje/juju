package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"juju/internal/store"
)

type fakeStore struct {
	gotLimit int
}

func (f *fakeStore) ProjectSecretHash(_ context.Context, id int64) ([]byte, error) {
	if id != 1 {
		return nil, store.ErrNotFound
	}
	return store.HashSecret("sk_good"), nil
}

func (f *fakeStore) ListIssues(_ context.Context, _ int64, limit int) ([]store.Issue, error) {
	f.gotLimit = limit
	return nil, nil
}

func (f *fakeStore) ListIssueEvents(_ context.Context, _, _ int64, _ int) ([]json.RawMessage, error) {
	return []json.RawMessage{json.RawMessage(`{"event_id":"a"}`)}, nil
}

func TestAPI(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		auth       string
		wantStatus int
		wantBody   string
		wantLimit  int
	}{
		{"list issues", "GET", "/api/v1/projects/1/issues", "Bearer sk_good", 200, `{"issues":[]}`, 50},
		{"limit is clamped", "GET", "/api/v1/projects/1/issues?limit=5000", "Bearer sk_good", 200, "", 200},
		{"list events", "GET", "/api/v1/projects/1/issues/7/events", "Bearer sk_good", 200, `"event_id":"a"`, 0},
		{"no auth", "GET", "/api/v1/projects/1/issues", "", 401, "missing Authorization", 0},
		{"wrong secret", "GET", "/api/v1/projects/1/issues", "Bearer sk_bad", 401, "invalid secret key", 0},
		{"public key is not a secret key", "GET", "/api/v1/projects/1/issues", "Bearer pk_whatever", 401, "", 0},
		{"unknown project", "GET", "/api/v1/projects/2/issues", "Bearer sk_good", 401, "", 0},
		{"POST not allowed", "POST", "/api/v1/projects/1/issues", "Bearer sk_good", 405, "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{}
			mux := http.NewServeMux()
			(&API{Store: fs, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Register(mux)

			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body %s", rec.Code, tc.wantStatus, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body %s does not contain %s", rec.Body, tc.wantBody)
			}
			if tc.wantLimit != 0 && fs.gotLimit != tc.wantLimit {
				t.Errorf("limit = %d, want %d", fs.gotLimit, tc.wantLimit)
			}
		})
	}
}
