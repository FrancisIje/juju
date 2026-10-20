// Package live tells open dashboards that something changed, over
// server-sent events (SSE). It sends no data, just "refresh": the page
// then reloads through the normal API, which already checks permissions
// and shapes the data, so there's nothing new to keep in sync.
//
// The hub lives in this process. With more than one server instance, a
// change on one wouldn't reach dashboards connected to another; that needs
// a shared bus (Phase 9).
package live

import (
	"net/http"
	"sync"
	"time"
)

// Hub fans "project N changed" out to the dashboards watching project N.
type Hub struct {
	mu     sync.Mutex
	subs   map[int64]map[chan struct{}]struct{}
	closed chan struct{}
	once   sync.Once
}

func NewHub() *Hub {
	return &Hub{subs: map[int64]map[chan struct{}]struct{}{}, closed: make(chan struct{})}
}

// Changed never blocks, so the worker storing events is never slowed by a
// slow browser. Each subscriber has room for one pending signal; if one is
// already waiting, this one merges into it, because "refresh" twice means
// the same as once.
func (h *Hub) Changed(projectID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[projectID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Subscribe returns a channel that receives a signal after each change,
// and a function to stop.
func (h *Hub) Subscribe(projectID int64) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs[projectID] == nil {
		h.subs[projectID] = map[chan struct{}]struct{}{}
	}
	h.subs[projectID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[projectID], ch)
		if len(h.subs[projectID]) == 0 {
			delete(h.subs, projectID)
		}
		h.mu.Unlock()
	}
}

// Close ends every stream. Called when the server shuts down: open streams
// never finish by themselves, so otherwise shutdown would wait for its
// timeout.
func (h *Hub) Close() { h.once.Do(func() { close(h.closed) }) }

// Subscribers counts open streams for a project (for tests and debugging).
func (h *Hub) Subscribers(projectID int64) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[projectID])
}

var (
	// MinGap is the shortest time between two refreshes on one stream, so a
	// crash loop sending 1000 events a second causes one reload a second.
	MinGap = time.Second
	// Ping keeps the connection alive through proxies that close quiet ones.
	Ping = 25 * time.Second
)

// Serve streams refresh signals for projectID until the client goes away
// or the hub closes. The caller has already checked the user may see the
// project.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, projectID int64) {
	rc := http.NewResponseController(w)
	// The server's WriteTimeout would cut the stream after 30 s. Lift it
	// for this one response.
	_ = rc.SetWriteDeadline(time.Time{})

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Accel-Buffering", "no") // nginx: send each message now, don't buffer
	w.WriteHeader(http.StatusOK)

	signals, stop := h.Subscribe(projectID)
	defer stop()

	// retry: how long the browser waits before reconnecting after a drop.
	if !send(w, rc, "retry: 3000\n\n") {
		return
	}
	ping := time.NewTicker(Ping)
	defer ping.Stop()
	var last time.Time
	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.closed:
			return
		case <-ping.C:
			// A line starting with ":" is a comment: browsers ignore it.
			if !send(w, rc, ": ping\n\n") {
				return
			}
		case <-signals:
			if wait := MinGap - time.Since(last); wait > 0 {
				// More changes during the wait merge into the pending signal.
				select {
				case <-time.After(wait):
				case <-r.Context().Done():
					return
				case <-h.closed:
					return
				}
			}
			if !send(w, rc, "data: refresh\n\n") {
				return
			}
			last = time.Now()
		}
	}
}

func send(w http.ResponseWriter, rc *http.ResponseController, msg string) bool {
	if _, err := w.Write([]byte(msg)); err != nil {
		return false
	}
	return rc.Flush() == nil
}
