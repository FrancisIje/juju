package event

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func analyticsEvent(t *testing.T, mutate func(m map[string]any)) (*Event, error) {
	t.Helper()
	m := map[string]any{
		"event_id":    "7b1e9a62-3a3b-4f5e-9c2d-1f0a8b7c6d5e",
		"type":        "event",
		"timestamp":   "2026-10-20T10:00:00Z",
		"platform":    "javascript",
		"name":        "checkout_started",
		"distinct_id": "anon-1",
		"properties":  map[string]any{"plan": "pro", "items": 3, "coupon": true, "cart": map[string]any{"total": 9.5}, "gone": nil},
	}
	if mutate != nil {
		mutate(m)
	}
	raw, _ := json.Marshal(m)
	return Normalize(raw, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
}

func TestAnalyticsEventIsKeptAndPropertiesBecomeText(t *testing.T) {
	e, err := analyticsEvent(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"plan": "pro", "items": "3", "coupon": "true", "cart": `{"total":9.5}`}
	got := e.StringProperties()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("properties[%q] = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["gone"]; ok {
		t.Error("a null property should be dropped")
	}
	if !e.IsAnalytics() || e.Exception != nil {
		t.Errorf("type %q, exception %v", e.Type, e.Exception)
	}
}

func TestAnalyticsValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(m map[string]any)
		field  string // "" means valid
	}{
		{"no name", func(m map[string]any) { delete(m, "name") }, "name"},
		{"blank name", func(m map[string]any) { m["name"] = "  " }, "name"},
		{"no distinct_id", func(m map[string]any) { delete(m, "distinct_id") }, "distinct_id"},
		{"long distinct_id", func(m map[string]any) { m["distinct_id"] = strings.Repeat("x", 201) }, "distinct_id"},
		{"identify", func(m map[string]any) {
			m["type"] = "identify"
			m["distinct_id"] = "user_42"
			m["anon_id"] = "anon-1"
			delete(m, "name")
		}, ""},
		// An analytics event can't sneak into issues by carrying an exception.
		{"event with an exception", func(m map[string]any) { m["exception"] = map[string]any{"type": "E", "message": "x"} }, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, err := analyticsEvent(t, tc.mutate)
			if tc.field == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if e.Exception != nil {
					t.Error("exception kept on an analytics event")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("err = %v, want one about %s", err, tc.field)
			}
		})
	}
	e, _ := analyticsEvent(t, func(m map[string]any) { m["type"] = "identify"; m["anon_id"] = "anon-1"; m["distinct_id"] = "user_42" })
	if e.Name != "$identify" {
		t.Errorf("identify name = %q", e.Name)
	}
}

func TestTooManyPropertiesAreCut(t *testing.T) {
	e, err := analyticsEvent(t, func(m map[string]any) {
		p := map[string]any{}
		for i := range 150 {
			p[strings.Repeat("k", 1+i%5)+string(rune('a'+i%26))+strings.Repeat("z", i/26)] = i
		}
		p[strings.Repeat("x", 65)] = "key too long"
		m["properties"] = p
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(e.Properties); n != maxProperties {
		t.Errorf("kept %d properties, want %d", n, maxProperties)
	}
	if len(e.Repairs) == 0 {
		t.Error("cutting properties should be recorded in repairs")
	}
}
