package analytics

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"juju/internal/event"
)

func TestOSAndBrowserFromContext(t *testing.T) {
	tests := []struct {
		name    string
		ctx     map[string]any
		os, brw string
	}{
		{"android app", map[string]any{"os": map[string]any{"name": "android", "version": "15"}}, "Android", ""},
		{"ios app", map[string]any{"os": map[string]any{"name": "iOS"}}, "iOS", ""},
		{"chrome on mac", ua("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"), "macOS", "Chrome"},
		{"safari on iphone", ua("Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1"), "iOS", "Safari"},
		{"chrome on iphone says Safari and Mac OS X too", ua("Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/141.0 Mobile/15E148 Safari/604.1"), "iOS", "Chrome"},
		{"edge on windows says Chrome too", ua("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.0.0"), "Windows", "Edge"},
		{"firefox on android", ua("Mozilla/5.0 (Android 15; Mobile; rv:141.0) Gecko/141.0 Firefox/141.0"), "Android", "Firefox"},
		{"nothing", nil, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			os, brw := clientOf(tc.ctx)
			if os != tc.os || brw != tc.brw {
				t.Errorf("got %q %q, want %q %q", os, brw, tc.os, tc.brw)
			}
		})
	}
}

func ua(s string) map[string]any { return map[string]any{"browser": map[string]any{"user_agent": s}} }

// With ClickHouse down, the writer holds at most MaxPending events and
// drops the oldest, so memory stays bounded however long the outage lasts.
func TestWriterBoundsWhatItHolds(t *testing.T) {
	w := &Writer{Client: &Client{URL: "http://127.0.0.1:1"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), FlushEvery: time.Hour, BatchSize: 1 << 20, MaxPending: 5}
	w.Start()
	for range 12 {
		w.Add(&event.Event{Type: event.TypeEvent, Name: "x", DistinctID: "a", Time: time.Now(), ReceivedAt: time.Now()})
	}
	deadline := time.Now().Add(5 * time.Second)
	for w.Dropped() < 7 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if w.Dropped() != 7 {
		t.Fatalf("dropped %d, want 7 (12 added, 5 kept)", w.Dropped())
	}
	done := make(chan struct{})
	go func() { w.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Stop hung with ClickHouse unreachable")
	}
}
