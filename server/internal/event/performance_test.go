package event

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const sessionID = "2c7e4f1a-8b3d-4e5f-9a6b-7c8d9e0f1a2b"

func validSession() map[string]any {
	return map[string]any{
		"event_id":  validID,
		"type":      "session",
		"timestamp": "2026-10-19T11:59:00Z",
		"platform":  "javascript",
		"release":   "shop@1.4.0",
		"session":   map[string]any{"sid": sessionID, "seq": 2, "status": "exited", "started": "2026-10-19T11:50:00Z", "errors": 1, "duration_ms": 540000},
	}
}

// validTransaction: a 1.2 s page load with one HTTP request inside it.
func validTransaction() map[string]any {
	return map[string]any{
		"event_id":  validID,
		"type":      "transaction",
		"timestamp": "2026-10-19T11:59:01.2Z",
		"platform":  "javascript",
		"transaction": map[string]any{
			"name":            "/checkout",
			"op":              "pageload",
			"start_timestamp": "2026-10-19T11:59:00Z",
			"spans": []any{
				map[string]any{"span_id": "a1", "op": "http.client", "description": "GET /api/cart", "start_timestamp": "2026-10-19T11:59:00.25Z", "timestamp": "2026-10-19T11:59:00.7505Z"},
			},
		},
	}
}

func change(base func() map[string]any, key string, f func(m map[string]any)) map[string]any {
	m := base()
	f(m[key].(map[string]any))
	return m
}

func TestNormalizeRejectsBadPerformanceEvents(t *testing.T) {
	tests := []struct {
		name      string
		in        any
		wantField string
	}{
		{"session without a session", with(func(m map[string]any) { m["type"] = "session" }), "session"},
		{"session id not a uuid", change(validSession, "session", func(s map[string]any) { s["sid"] = "abc" }), "session.sid"},
		// "errored" is a state other tools use; here errors are a count,
		// so the status set stays small and every SDK agrees on it.
		{"unknown session status", change(validSession, "session", func(s map[string]any) { s["status"] = "errored" }), "session.status"},
		{"negative seq", change(validSession, "session", func(s map[string]any) { s["seq"] = -1 }), "session.seq"},
		{"transaction without a transaction", with(func(m map[string]any) { m["type"] = "transaction" }), "transaction"},
		{"transaction without a name", change(validTransaction, "transaction", func(tx map[string]any) { tx["name"] = "  " }), "transaction.name"},
		{"transaction without a start", change(validTransaction, "transaction", func(tx map[string]any) { delete(tx, "start_timestamp") }), "transaction.start_timestamp"},
		{"transaction ending before it starts", change(validTransaction, "transaction", func(tx map[string]any) {
			tx["start_timestamp"] = "2026-10-19T11:59:05Z"
		}), "timestamp"},
		// Unlike an error, a transaction's end can't fall back to the
		// receive time: that would invent a duration.
		{"transaction without an end", func() map[string]any { m := validTransaction(); delete(m, "timestamp"); return m }(), "timestamp"},
		{"transaction over an hour", change(validTransaction, "transaction", func(tx map[string]any) {
			tx["start_timestamp"] = "2026-10-19T10:00:00Z"
		}), "transaction"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, err := Normalize(encode(t, tc.in), now)
			if err == nil {
				t.Fatalf("want error, got event %+v", e)
			}
			verr, ok := err.(ValidationError)
			if !ok || verr[0].Field != tc.wantField {
				t.Fatalf("got %v, want a problem with %q", err, tc.wantField)
			}
		})
	}
}

func TestNormalizeSession(t *testing.T) {
	t.Run("valid update", func(t *testing.T) {
		in := validSession()
		in["breadcrumbs"] = []any{map[string]any{"message": "not needed"}}
		in["exception"] = map[string]any{"type": "E"} // a payload for another type
		e, err := Normalize(encode(t, in), now)
		if err != nil {
			t.Fatal(err)
		}
		s := e.Session
		if s.ID != sessionID || s.Seq != 2 || s.Status != "exited" || s.Errors != 1 || *s.DurationMs != 540000 {
			t.Errorf("session = %+v", s)
		}
		if !s.StartedAt.Equal(time.Date(2026, 10, 19, 11, 50, 0, 0, time.UTC)) {
			t.Errorf("started = %v", s.StartedAt)
		}
		if e.Exception != nil || e.Breadcrumbs != nil {
			t.Errorf("a session update must not keep an exception (%v) or breadcrumbs (%v)", e.Exception, e.Breadcrumbs)
		}
		if len(e.Repairs) != 0 {
			t.Errorf("unexpected repairs %v", e.Repairs)
		}
	})
	t.Run("start after the update is clamped", func(t *testing.T) {
		e, err := Normalize(encode(t, change(validSession, "session", func(s map[string]any) { s["started"] = "2026-10-19T11:59:30Z" })), now)
		if err != nil {
			t.Fatal(err)
		}
		if !e.Session.StartedAt.Equal(e.Time) || !strings.Contains(strings.Join(e.Repairs, " "), "session.started") {
			t.Errorf("started %v, time %v, repairs %v", e.Session.StartedAt, e.Time, e.Repairs)
		}
	})
	t.Run("uppercase sid is canonicalised so updates merge", func(t *testing.T) {
		e, err := Normalize(encode(t, change(validSession, "session", func(s map[string]any) { s["sid"] = strings.ToUpper(sessionID) })), now)
		if err != nil || e.Session.ID != sessionID {
			t.Fatalf("sid %q, err %v", e.Session.ID, err)
		}
	})
}

func TestNormalizeTransaction(t *testing.T) {
	t.Run("durations and span offsets", func(t *testing.T) {
		e, err := Normalize(encode(t, validTransaction()), now)
		if err != nil {
			t.Fatal(err)
		}
		tx := e.Transaction
		if tx.DurationMs != 1200 || tx.Status != "ok" || tx.Op != "pageload" {
			t.Errorf("transaction = %+v", tx)
		}
		sp := tx.Spans[0]
		// Sub-millisecond precision survives: 500.5 ms, not 500 or 501.
		if sp.StartMs != 250 || sp.DurationMs != 500.5 {
			t.Errorf("span start %v ms, duration %v ms; want 250 and 500.5", sp.StartMs, sp.DurationMs)
		}
	})
	t.Run("wrong device clock shifts times but keeps durations", func(t *testing.T) {
		// The phone thinks it's 2 days in the future. The end becomes the
		// receive time, and start and spans move with it.
		in := validTransaction()
		in["timestamp"] = "2026-10-21T11:59:01.2Z"
		tx := in["transaction"].(map[string]any)
		tx["start_timestamp"] = "2026-10-21T11:59:00Z"
		tx["spans"] = []any{map[string]any{"span_id": "a1", "op": "http.client", "start_timestamp": "2026-10-21T11:59:00.25Z", "timestamp": "2026-10-21T11:59:00.75Z"}}
		e, err := Normalize(encode(t, in), now)
		if err != nil {
			t.Fatal(err)
		}
		got := e.Transaction
		if !e.Time.Equal(now) || got.DurationMs != 1200 || !got.Start.Equal(now.Add(-1200*time.Millisecond)) {
			t.Errorf("end %v start %v duration %v", e.Time, got.Start, got.DurationMs)
		}
		if got.Spans[0].StartMs != 250 || got.Spans[0].StartTimestamp != now.Add(-950*time.Millisecond).Format(time.RFC3339Nano) {
			t.Errorf("span = %+v", got.Spans[0])
		}
	})
	t.Run("bad spans are dropped, good ones kept", func(t *testing.T) {
		in := change(validTransaction, "transaction", func(tx map[string]any) {
			tx["spans"] = append(tx["spans"].([]any),
				map[string]any{"op": "", "start_timestamp": "2026-10-19T11:59:00Z", "timestamp": "2026-10-19T11:59:01Z"},
				map[string]any{"op": "db", "start_timestamp": "2026-10-19T11:59:01Z", "timestamp": "2026-10-19T11:59:00Z"},
				map[string]any{"op": "db", "start_timestamp": "soon", "timestamp": "2026-10-19T11:59:00Z"},
			)
		})
		e, err := Normalize(encode(t, in), now)
		if err != nil {
			t.Fatal(err)
		}
		if len(e.Transaction.Spans) != 1 || !strings.Contains(strings.Join(e.Repairs, " "), "dropped 3") {
			t.Errorf("%d spans, repairs %v", len(e.Transaction.Spans), e.Repairs)
		}
	})
	t.Run("too many spans keeps the first 500", func(t *testing.T) {
		in := change(validTransaction, "transaction", func(tx map[string]any) {
			spans := make([]any, 600)
			for i := range spans {
				spans[i] = map[string]any{"span_id": fmt.Sprint(i), "op": "x", "start_timestamp": "2026-10-19T11:59:00Z", "timestamp": "2026-10-19T11:59:00Z"}
			}
			tx["spans"] = spans
		})
		e, err := Normalize(encode(t, in), now)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(e.Transaction.Spans); n != maxSpans || e.Transaction.Spans[0].SpanID != "0" {
			t.Errorf("%d spans", n)
		}
	})
	t.Run("missing op becomes custom", func(t *testing.T) {
		e, err := Normalize(encode(t, change(validTransaction, "transaction", func(tx map[string]any) { delete(tx, "op") })), now)
		if err != nil || e.Transaction.Op != "custom" {
			t.Fatalf("op %q err %v", e.Transaction.Op, err)
		}
	})
}

// Sessions and transactions have no exception, like analytics events, but
// they're not analytics: their tags must still be cleaned. (Repairs are
// chosen by type, not by "has no exception".)
func TestPerformanceEventsGetTheCommonRepairs(t *testing.T) {
	for name, base := range map[string]func() map[string]any{"session": validSession, "transaction": validTransaction} {
		t.Run(name, func(t *testing.T) {
			in := base()
			in["tags"] = map[string]any{"ok": "v", strings.Repeat("k", 40): "too long a key"}
			e, err := Normalize(encode(t, in), now)
			if err != nil {
				t.Fatal(err)
			}
			if len(e.Tags) != 1 || !strings.Contains(strings.Join(e.Repairs, " "), "tags: dropped 1") {
				t.Errorf("tags %v, repairs %v", e.Tags, e.Repairs)
			}
		})
	}
}
