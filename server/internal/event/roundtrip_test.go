package event

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// Events reach workers through the queue as JSON. Fields JSON skips
// (json:"-", set by the server) must come back exactly as Normalize left
// them, or the worker stores something different from what ingest checked:
// that's how sessions once arrived with no start time. This compares every
// field, so a new json:"-" field fails here until RestoreParsed handles it.
func TestQueueRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for name, raw := range map[string]string{
		"error":       `{"event_id":"8f14e45f-ceea-4f6a-9b1e-1f0b2c3d4e5f","type":"error","timestamp":"2026-10-21T11:59:00Z","platform":"dart","exception":{"type":"E","message":"x","stacktrace":{"frames":[{"function":"f","in_app":true}]}},"tags":{"a":"b"}}`,
		"session":     `{"event_id":"8f14e45f-ceea-4f6a-9b1e-1f0b2c3d4e60","type":"session","timestamp":"2026-10-21T11:59:00Z","platform":"dart","session":{"sid":"6f1c2b3a-4d5e-4f60-8a7b-9c0d1e2f3a4b","seq":2,"status":"exited","started":"2026-10-21T11:50:00.5Z","errors":1,"duration_ms":540000}}`,
		"transaction": `{"event_id":"8f14e45f-ceea-4f6a-9b1e-1f0b2c3d4e61","type":"transaction","timestamp":"2026-10-21T11:59:01.25Z","platform":"javascript","transaction":{"name":"/cart","op":"pageload","start_timestamp":"2026-10-21T11:59:00Z","spans":[{"span_id":"a","op":"http","start_timestamp":"2026-10-21T11:59:00.1Z","timestamp":"2026-10-21T11:59:00.9Z"}]}}`,
		"analytics":   `{"event_id":"8f14e45f-ceea-4f6a-9b1e-1f0b2c3d4e62","type":"event","timestamp":"2026-10-21T11:59:00Z","platform":"javascript","name":"signed_up","distinct_id":"u1","properties":{"plan":"pro","n":2}}`,
	} {
		t.Run(name, func(t *testing.T) {
			e, err := Normalize(json.RawMessage(raw), now)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			var back Event
			if err := json.Unmarshal(data, &back); err != nil {
				t.Fatal(err)
			}
			back.RestoreParsed()
			back.ProjectID = e.ProjectID // travels beside the event on the queue
			if !reflect.DeepEqual(&back, e) {
				t.Errorf("after the queue:\n got %+v\nwant %+v", &back, e)
				if e.Session != nil {
					t.Errorf("session: got %+v want %+v", back.Session, e.Session)
				}
				if e.Transaction != nil {
					t.Errorf("transaction: got %+v want %+v", back.Transaction, e.Transaction)
				}
			}
		})
	}
}
