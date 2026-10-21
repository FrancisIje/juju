// Package ratelimit counts events per key in a sliding window: "at most 10
// failed logins per address in 15 minutes", "5 sign-ups per address an
// hour". It lives in memory, which suits one server: a restart forgets the
// counts, and that only ever helps a legitimate user.
package ratelimit

import (
	"sync"
	"time"
)

// Window allows Max events per key in any span of Per.
type Window struct {
	Max int
	Per time.Duration
	Now func() time.Time // injectable for tests; nil means time.Now

	mu    sync.Mutex
	hits  map[string][]time.Time
	calls int
}

// Allow records an event for key and reports whether it was within the
// limit. Refused events aren't recorded, so a client that keeps trying
// isn't locked out for longer than the window.
func (w *Window) Allow(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	recent := w.recent(key, now)
	if len(recent) >= w.Max {
		return false
	}
	w.hits[key] = append(recent, now)
	return true
}

// Blocked reports whether key is at its limit, without recording anything.
// With Hit, it counts only some events, like failed logins.
func (w *Window) Blocked(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.recent(key, w.now())) >= w.Max
}

// Hit records an event for key, whether or not it is over the limit.
func (w *Window) Hit(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	w.hits[key] = append(w.recent(key, now), now)
}

// Reset forgets key, e.g. after a successful login.
func (w *Window) Reset(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.hits, key)
}

// RetryAfter is how long until key is under its limit again (0 if it is).
func (w *Window) RetryAfter(key string) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	recent := w.recent(key, now)
	if len(recent) < w.Max {
		return 0
	}
	// The oldest hits leave the window first.
	return recent[len(recent)-w.Max].Add(w.Per).Sub(now)
}

// Len is how many keys are being tracked (for tests).
func (w *Window) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.hits)
}

func (w *Window) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// recent returns key's hits still inside the window. Every 1,000 calls it
// also drops keys with nothing left in the window. Without that, an
// attacker sending a new made-up email or address each time would grow
// the map until the server ran out of memory. Call with w.mu held.
func (w *Window) recent(key string, now time.Time) []time.Time {
	if w.hits == nil {
		w.hits = map[string][]time.Time{}
	}
	if w.calls++; w.calls%1000 == 0 {
		for k, ts := range w.hits {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= w.Per {
				delete(w.hits, k)
			}
		}
	}
	ts := w.hits[key]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= w.Per {
		i++
	}
	if i == len(ts) {
		delete(w.hits, key)
		return nil
	}
	return ts[i:]
}
