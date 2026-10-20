package live

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChangedNeverBlocksAndMerges(t *testing.T) {
	h := NewHub()
	ch, stop := h.Subscribe(1)
	defer stop()
	// Nobody is reading: these must not block, and must leave one signal.
	for range 1000 {
		h.Changed(1)
	}
	h.Changed(2) // another project: not ours
	<-ch
	select {
	case <-ch:
		t.Fatal("1000 changes should merge into one pending signal")
	default:
	}
}

func TestStopRemovesSubscriber(t *testing.T) {
	h := NewHub()
	_, stop := h.Subscribe(1)
	_, stop2 := h.Subscribe(1)
	if h.Subscribers(1) != 2 {
		t.Fatal("want 2 subscribers")
	}
	stop()
	stop2()
	if h.Subscribers(1) != 0 || len(h.subs) != 0 {
		t.Fatalf("leaked subscribers: %v", h.subs)
	}
}

// reads SSE lines from a real HTTP connection, the way a browser would.
func stream(t *testing.T, h *Hub) (*bufio.Reader, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.Serve(w, r, 7) }))
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	return bufio.NewReader(res.Body), func() { cancel(); res.Body.Close(); srv.Close() }
}

func next(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	done := make(chan string, 1)
	go func() {
		var msg strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				done <- "EOF"
				return
			}
			if line == "\n" {
				done <- msg.String()
				return
			}
			msg.WriteString(line)
		}
	}()
	select {
	case m := <-done:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("no message within 3s")
		return ""
	}
}

func TestStreamSendsRefreshThrottled(t *testing.T) {
	MinGap = 200 * time.Millisecond
	defer func() { MinGap = time.Second }()
	h := NewHub()
	r, done := stream(t, h)
	defer done()
	if m := next(t, r); m != "retry: 3000\n" {
		t.Fatalf("first message = %q", m)
	}
	waitFor(t, func() bool { return h.Subscribers(7) == 1 })

	h.Changed(7)
	if m := next(t, r); m != "data: refresh\n" {
		t.Fatalf("got %q", m)
	}
	start := time.Now()
	for range 50 {
		h.Changed(7) // a burst
	}
	if m := next(t, r); m != "data: refresh\n" {
		t.Fatalf("got %q", m)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Error("second refresh came before MinGap")
	}
	// The burst merged into that one refresh: nothing else is pending.
	time.Sleep(300 * time.Millisecond)
	if h.Subscribers(7) != 1 {
		t.Fatal("stream ended unexpectedly")
	}
}

func TestPingKeepsConnectionAlive(t *testing.T) {
	Ping = 50 * time.Millisecond
	defer func() { Ping = 25 * time.Second }()
	h := NewHub()
	r, done := stream(t, h)
	defer done()
	next(t, r) // retry
	if m := next(t, r); m != ": ping\n" {
		t.Fatalf("got %q, want a ping comment", m)
	}
}

func TestCloseEndsStreamsAndClientGoneUnsubscribes(t *testing.T) {
	h := NewHub()
	r, done := stream(t, h)
	next(t, r)
	waitFor(t, func() bool { return h.Subscribers(7) == 1 })
	h.Close()
	if m := next(t, r); m != "EOF" {
		t.Fatalf("got %q, want the stream to end", m)
	}
	waitFor(t, func() bool { return h.Subscribers(7) == 0 })
	done()

	// A browser tab closing must free its subscription.
	h2 := NewHub()
	r2, done2 := stream(t, h2)
	next(t, r2)
	waitFor(t, func() bool { return h2.Subscribers(7) == 1 })
	done2()
	waitFor(t, func() bool { return h2.Subscribers(7) == 0 })
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for range 100 {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}
