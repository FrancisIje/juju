package event

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseEnvelope(t *testing.T) {
	ev := `{"event_id":"x"}`
	tests := []struct {
		name    string
		body    string
		wantErr string // "" means it should parse
	}{
		{"valid", `{"sdk":{"name":"juju.web"},"events":[` + ev + `]}`, ""},
		{"unknown fields are ignored", `{"sdk":{"name":"juju.web","future":1},"events":[` + ev + `],"v2_field":true}`, ""},
		{"not json", `hello`, "invalid JSON"},
		{"top level array", `[` + ev + `]`, "invalid JSON"},
		{"trailing garbage", `{"sdk":{"name":"a"},"events":[` + ev + `]}garbage`, "unexpected data"},
		{"trailing brace", `{"sdk":{"name":"a"},"events":[` + ev + `]}}`, "unexpected data"},
		{"null", `null`, "events"},
		{"no events", `{"sdk":{"name":"a"},"events":[]}`, "at least one event"},
		{"missing sdk name", `{"events":[` + ev + `]}`, "sdk.name"},
		{"too many events", `{"sdk":{"name":"a"},"events":[` + strings.TrimSuffix(strings.Repeat(ev+",", 101), ",") + `]}`, "has 101 events"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseEnvelope([]byte(tc.body))
			got := fmt.Sprint(err)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && !strings.Contains(got, tc.wantErr) {
				t.Fatalf("error %q does not contain %q", got, tc.wantErr)
			}
		})
	}
}
