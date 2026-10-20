package httpx

import (
	"net/http/httptest"
	"testing"
)

// Every JSON answer, data or error, must tell browsers not to keep it.
func TestJSONIsNeverStored(t *testing.T) {
	for _, write := range []func(*httptest.ResponseRecorder){
		func(w *httptest.ResponseRecorder) { WriteJSON(w, 200, map[string]string{"secret_key": "sk_x"}) },
		func(w *httptest.ResponseRecorder) { WriteError(w, 401, "log in to continue") },
	} {
		w := httptest.NewRecorder()
		write(w)
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
		if got := w.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
	}
}
