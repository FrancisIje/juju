package ratelimit

import (
	"strconv"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func TestWindowSlides(t *testing.T) {
	c := &clock{time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	w := &Window{Max: 3, Per: time.Minute, Now: c.now}
	for i := 0; i < 3; i++ {
		if !w.Allow("a") {
			t.Fatalf("event %d refused", i+1)
		}
		c.add(10 * time.Second)
	}
	if w.Allow("a") {
		t.Fatal("4th event within a minute allowed")
	}
	if !w.Allow("b") {
		t.Fatal("another key must have its own count")
	}
	// The first event was at 0 s; at 60 s it leaves the window.
	if got := w.RetryAfter("a"); got != 30*time.Second {
		t.Errorf("RetryAfter = %v, want 30s", got)
	}
	c.add(30 * time.Second)
	if !w.Allow("a") {
		t.Error("allowed again once the oldest event left the window")
	}
}

// Refused attempts aren't counted: someone who keeps trying is blocked for
// one window, not forever.
func TestRefusalsDontExtendTheBlock(t *testing.T) {
	c := &clock{time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	w := &Window{Max: 2, Per: time.Minute, Now: c.now}
	w.Allow("a")
	w.Allow("a")
	for i := 0; i < 50; i++ {
		c.add(time.Second)
		w.Allow("a")
	}
	c.add(10 * time.Second) // 60 s after the two real events
	if !w.Allow("a") {
		t.Error("still blocked: refused attempts were counted")
	}
}

func TestBlockedHitReset(t *testing.T) {
	w := &Window{Max: 2, Per: time.Minute}
	if w.Blocked("a") {
		t.Fatal("blocked with no hits")
	}
	w.Hit("a")
	w.Hit("a")
	if !w.Blocked("a") {
		t.Fatal("not blocked after 2 hits")
	}
	w.Reset("a")
	if w.Blocked("a") {
		t.Error("still blocked after Reset")
	}
}

// One made-up key per request (a new email each login attempt) must not
// grow memory forever: expired keys are dropped.
func TestPrunesExpiredKeys(t *testing.T) {
	c := &clock{time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	w := &Window{Max: 5, Per: time.Minute, Now: c.now}
	for i := 0; i < 5000; i++ {
		w.Allow("k" + strconv.Itoa(i))
	}
	c.add(2 * time.Minute)
	for i := 0; i < 1000; i++ {
		w.Allow("fresh")
	}
	if n := w.Len(); n > 1 {
		t.Errorf("%d keys tracked, want only the 1 still in its window", n)
	}
}
