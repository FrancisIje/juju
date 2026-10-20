// Package httpx holds small HTTP helpers shared by the ingest and API handlers.
package httpx

import (
	"encoding/json"
	"net/http"
)

// WriteJSON sends v as a JSON response with the given status code.
//
// no-store: API answers hold private data (issues, keys, teammates), and are
// always fetched fresh. Without it a browser may keep them on disk, where the
// next person on a shared computer could find them after logout.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // an error means the client hung up; nothing useful to do
}

// WriteError sends {"error": msg}. Every error response has this shape, so
// SDKs and the dashboard can show the reason without special cases.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}
