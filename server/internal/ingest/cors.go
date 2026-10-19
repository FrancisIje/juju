package ingest

import "net/http"

// Browsers block a page on shop.example.com from reading responses from
// another origin (juju's server) unless the server opts in. That rule is
// CORS. Because the SDK sends a custom header (X-Juju-Key), the browser first
// sends an OPTIONS "preflight" request asking permission, and only sends the
// real POST if the answer allows it.
//
// "*" (any origin) is right for ingest: every customer's site must be able to
// send errors, and the public key, not the origin, decides which project
// they land in. The read API deliberately has no CORS headers, so a web page
// can't use a secret key from someone's browser.

func allowCORS(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	// Browsers hide most response headers from scripts unless listed here,
	// and the SDK needs Retry-After to back off correctly on 429.
	h.Set("Access-Control-Expose-Headers", "Retry-After")
}

// Preflight answers the browser's OPTIONS request.
func Preflight(w http.ResponseWriter, r *http.Request) {
	allowCORS(w)
	h := w.Header()
	h.Set("Access-Control-Allow-Methods", "POST")
	h.Set("Access-Control-Allow-Headers", "Content-Type, Content-Encoding, X-Juju-Key")
	// Let the browser cache this answer for a day instead of asking again
	// before every send.
	h.Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
}
