package event

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var now = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

const validID = "9b1f3c2a-5d4e-4f6a-8b7c-1d2e3f4a5b6c"

// validEvent returns a minimal event that passes, as a map so each test case
// can change one field.
func validEvent() map[string]any {
	return map[string]any{
		"event_id":  validID,
		"type":      "error",
		"timestamp": "2026-10-19T11:59:00Z",
		"platform":  "dart",
		"exception": map[string]any{
			"type":    "StateError",
			"message": "Bad state: no element",
			"stacktrace": map[string]any{"frames": []any{
				map[string]any{"function": "CartPage.build", "filename": "package:shop/cart.dart", "lineno": 88, "in_app": true},
			}},
		},
	}
}

func encode(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func with(change func(m map[string]any)) map[string]any {
	m := validEvent()
	change(m)
	return m
}

func frames(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = map[string]any{"function": fmt.Sprintf("f%d", i), "in_app": true}
	}
	return out
}

func TestNormalizeRejects(t *testing.T) {
	tests := []struct {
		name      string
		in        any
		wantField string
	}{
		{"not an object", []any{1, 2}, "event"},
		{"lineno is a string", with(func(m map[string]any) {
			m["exception"].(map[string]any)["stacktrace"] = map[string]any{"frames": []any{map[string]any{"lineno": "88"}}}
		}), "event"},
		{"missing event_id", with(func(m map[string]any) { delete(m, "event_id") }), "event_id"},
		{"event_id not a uuid", with(func(m map[string]any) { m["event_id"] = "abc" }), "event_id"},
		{"nil uuid", with(func(m map[string]any) { m["event_id"] = "00000000-0000-0000-0000-000000000000" }), "event_id"},
		{"unknown type", with(func(m map[string]any) { m["type"] = "profile" }), "type"},
		{"missing exception", with(func(m map[string]any) { delete(m, "exception") }), "exception"},
		{"exception without type or message", with(func(m map[string]any) {
			m["exception"] = map[string]any{"type": " ", "message": ""}
		}), "exception"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, err := Normalize(encode(t, tc.in), now)
			if err == nil {
				t.Fatalf("want error, got event %+v", e)
			}
			verr, ok := err.(ValidationError)
			if !ok {
				t.Fatalf("want ValidationError, got %T: %v", err, err)
			}
			if verr[0].Field != tc.wantField {
				t.Errorf("field = %q, want %q (error: %v)", verr[0].Field, tc.wantField, err)
			}
		})
	}
}

func TestNormalizeReportsAllProblems(t *testing.T) {
	_, err := Normalize(json.RawMessage(`{"event_id":"nope","type":"error"}`), now)
	verr, _ := err.(ValidationError)
	if len(verr) != 2 {
		t.Fatalf("want 2 problems (event_id and exception), got %v", err)
	}
}

func TestNormalizeRepairs(t *testing.T) {
	tests := []struct {
		name       string
		in         map[string]any
		check      func(t *testing.T, e *Event)
		wantRepair string // substring of a repair note; "" means none expected
	}{
		{
			name: "valid event is untouched",
			in:   validEvent(),
			check: func(t *testing.T, e *Event) {
				if e.Timestamp != "2026-10-19T11:59:00Z" || e.Environment != "production" {
					t.Errorf("got timestamp %q env %q", e.Timestamp, e.Environment)
				}
			},
		},
		{
			name: "uppercase uuid is canonicalised so dedupe matches",
			in:   with(func(m map[string]any) { m["event_id"] = strings.ToUpper(validID) }),
			check: func(t *testing.T, e *Event) {
				if e.EventID != validID {
					t.Errorf("event_id = %q", e.EventID)
				}
			},
		},
		{
			name:       "missing timestamp uses receive time",
			in:         with(func(m map[string]any) { delete(m, "timestamp") }),
			check:      wantTime(now),
			wantRepair: "timestamp: missing",
		},
		{
			name:       "garbage timestamp uses receive time",
			in:         with(func(m map[string]any) { m["timestamp"] = "yesterday" }),
			check:      wantTime(now),
			wantRepair: "timestamp: not RFC 3339",
		},
		{
			name:       "timestamp 2h in the future is clamped",
			in:         with(func(m map[string]any) { m["timestamp"] = now.Add(2 * time.Hour).Format(time.RFC3339) }),
			check:      wantTime(now),
			wantRepair: "in the future",
		},
		{
			name:  "timestamp 30m in the future is allowed (clock skew)",
			in:    with(func(m map[string]any) { m["timestamp"] = now.Add(30 * time.Minute).Format(time.RFC3339) }),
			check: wantTime(now.Add(30 * time.Minute)),
		},
		{
			name:       "timestamp older than 30 days is clamped",
			in:         with(func(m map[string]any) { m["timestamp"] = now.AddDate(0, 0, -31).Format(time.RFC3339) }),
			check:      wantTime(now),
			wantRepair: "older than 30 days",
		},
		{
			name: "timezone offset is converted to UTC",
			in:   with(func(m map[string]any) { m["timestamp"] = "2026-10-19T12:59:00+01:00" }),
			check: func(t *testing.T, e *Event) {
				if e.Timestamp != "2026-10-19T11:59:00Z" {
					t.Errorf("timestamp = %q", e.Timestamp)
				}
			},
		},
		{
			name: "unknown platform becomes other",
			in:   with(func(m map[string]any) { m["platform"] = "cobol" }),
			check: func(t *testing.T, e *Event) {
				if e.Platform != "other" {
					t.Errorf("platform = %q", e.Platform)
				}
			},
			wantRepair: "platform",
		},
		{
			name: "long message is truncated",
			in:   with(func(m map[string]any) { m["exception"].(map[string]any)["message"] = strings.Repeat("x", 10_000) }),
			check: func(t *testing.T, e *Event) {
				if len(e.Exception.Message) != maxMessageBytes {
					t.Errorf("message length = %d", len(e.Exception.Message))
				}
			},
			wantRepair: "exception.message: truncated",
		},
		{
			name: "300 frames keeps first 50 and last 200",
			in: with(func(m map[string]any) {
				m["exception"].(map[string]any)["stacktrace"] = map[string]any{"frames": frames(300)}
			}),
			check: func(t *testing.T, e *Event) {
				fs := e.Exception.Stacktrace.Frames
				if len(fs) != 250 || fs[49].Function != "f49" || fs[50].Function != "f100" || fs[249].Function != "f299" {
					t.Errorf("got %d frames: [49]=%s [50]=%s [249]=%s", len(fs), fs[49].Function, fs[50].Function, fs[len(fs)-1].Function)
				}
			},
			wantRepair: "300 frames",
		},
		{
			name: "150 breadcrumbs keeps the last 100",
			in: with(func(m map[string]any) {
				crumbs := make([]any, 150)
				for i := range crumbs {
					crumbs[i] = map[string]any{"message": fmt.Sprint(i)}
				}
				m["breadcrumbs"] = crumbs
			}),
			check: func(t *testing.T, e *Event) {
				if len(e.Breadcrumbs) != 100 || e.Breadcrumbs[0].Message != "50" {
					t.Errorf("got %d crumbs, first %q", len(e.Breadcrumbs), e.Breadcrumbs[0].Message)
				}
			},
			wantRepair: "breadcrumbs",
		},
		{
			name: "bad tags are dropped, good ones kept",
			in: with(func(m map[string]any) {
				m["tags"] = map[string]any{"flavor": "prod", strings.Repeat("k", 40): "v", "big": strings.Repeat("v", 300)}
			}),
			check: func(t *testing.T, e *Event) {
				if len(e.Tags) != 1 || e.Tags["flavor"] != "prod" {
					t.Errorf("tags = %v", e.Tags)
				}
			},
			wantRepair: "tags: dropped 2",
		},
		{
			name: "client cannot set server-owned fields",
			in: with(func(m map[string]any) {
				m["repairs"] = []string{"fake"}
				m["received_at"] = "2000-01-01T00:00:00Z"
			}),
			check: func(t *testing.T, e *Event) {
				if len(e.Repairs) != 0 || !e.ReceivedAt.Equal(now) {
					t.Errorf("repairs %v received_at %v", e.Repairs, e.ReceivedAt)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, err := Normalize(encode(t, tc.in), now)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, e)
			}
			got := strings.Join(e.Repairs, " | ")
			if tc.wantRepair == "" && got != "" {
				t.Errorf("unexpected repairs: %s", got)
			}
			if tc.wantRepair != "" && !strings.Contains(got, tc.wantRepair) {
				t.Errorf("repairs %q do not mention %q", got, tc.wantRepair)
			}
		})
	}
}

func wantTime(want time.Time) func(*testing.T, *Event) {
	return func(t *testing.T, e *Event) {
		t.Helper()
		if !e.Time.Equal(want) {
			t.Errorf("time = %v, want %v", e.Time, want)
		}
	}
}

func TestTruncateKeepsUTF8Valid(t *testing.T) {
	// "é" is 2 bytes; cutting at 3 would split the second one.
	got, cut := truncate("éé", 3)
	if !cut || got != "é" || !utf8.ValidString(got) {
		t.Errorf("truncate = %q, %v", got, cut)
	}
}

// FuzzNormalize feeds random bytes in. Ingest is the part of juju open to the
// internet, so whatever arrives, Normalize must not panic, and anything it
// accepts must satisfy the limits. Run longer with:
//
//	go test ./internal/event -fuzz=FuzzNormalize -fuzztime=30s
func FuzzNormalize(f *testing.F) {
	valid, _ := json.Marshal(validEvent())
	f.Add(valid)
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"event_id":"` + validID + `","type":"error","exception":{"message":"x"},"tags":{"":""}}`))
	session, _ := json.Marshal(validSession())
	f.Add(session)
	transaction, _ := json.Marshal(validTransaction())
	f.Add(transaction)
	f.Fuzz(func(t *testing.T, raw []byte) {
		e, err := Normalize(raw, now)
		if err != nil {
			return
		}
		if len(e.Tags) > maxTags || len(e.Breadcrumbs) > maxBreadcrumbs {
			t.Fatalf("limits not enforced: %+v", e)
		}
		// Exactly one payload, the one matching the type.
		switch {
		case e.Type == TypeError && (e.Exception == nil || e.Session != nil || e.Transaction != nil),
			e.Type == TypeSession && (e.Session == nil || e.Exception != nil || e.Transaction != nil),
			e.Type == TypeTransaction && (e.Transaction == nil || e.Exception != nil || e.Session != nil):
			t.Fatalf("type %q with the wrong payload: %+v", e.Type, e)
		}
		if ex := e.Exception; ex != nil {
			if len(ex.Message) > maxMessageBytes {
				t.Fatalf("message of %d bytes", len(ex.Message))
			}
			if st := ex.Stacktrace; st != nil && len(st.Frames) > maxFrames {
				t.Fatalf("%d frames", len(st.Frames))
			}
		}
		if tx := e.Transaction; tx != nil {
			if tx.DurationMs < 0 || len(tx.Spans) > maxSpans {
				t.Fatalf("duration %v, %d spans", tx.DurationMs, len(tx.Spans))
			}
			for _, sp := range tx.Spans {
				if sp.DurationMs < 0 {
					t.Fatalf("span with negative duration: %+v", sp)
				}
			}
		}
		if _, err := time.Parse(time.RFC3339Nano, e.Timestamp); err != nil {
			t.Fatalf("stored timestamp %q does not parse", e.Timestamp)
		}
	})
}
