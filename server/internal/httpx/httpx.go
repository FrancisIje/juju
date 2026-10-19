// Package httpx holds small HTTP helpers shared by the ingest and API handlers.
package httpx

import (
	"encoding/json"
	"net/http"
)

// WriteJSON sends v as a JSON response with the given status code.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // an error means the client hung up; nothing useful to do
}

// WriteError sends {"error": msg}. Every error response has this shape, so
// SDKs and the dashboard can show the reason without special cases.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}
