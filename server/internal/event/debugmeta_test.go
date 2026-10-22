package event

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDebugMetaRepair(t *testing.T) {
	const id = "4F1A2B3C-0000-4D5E-8F90-ABCDEF012345" // uppercase on purpose
	for _, tc := range []struct {
		name   string
		images string
		want   []DebugImage // nil: debug_meta removed
		repair bool
	}{
		{"valid, id lowercased", `[{"type":"sourcemap","code_file":"https://shop.com/app.min.js","debug_id":"` + id + `"}]`,
			[]DebugImage{{"sourcemap", "https://shop.com/app.min.js", strings.ToLower(id)}}, false},
		{"not a UUID", `[{"type":"sourcemap","code_file":"a.js","debug_id":"nope"}]`, nil, true},
		{"nil UUID", `[{"type":"sourcemap","code_file":"a.js","debug_id":"00000000-0000-0000-0000-000000000000"}]`, nil, true},
		{"unknown type", `[{"type":"elf","code_file":"a.js","debug_id":"` + id + `"}]`, nil, true},
		{"no code_file", `[{"type":"sourcemap","code_file":"","debug_id":"` + id + `"}]`, nil, true},
		{"one good, one bad", `[{"type":"sourcemap","code_file":"a.js","debug_id":"x"},{"type":"sourcemap","code_file":"b.js","debug_id":"` + id + `"}]`,
			[]DebugImage{{"sourcemap", "b.js", strings.ToLower(id)}}, true},
		{"empty list", `[]`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"event_id":"8f14e45f-ceea-4f6a-9b1e-1f0b2c3d4e5f","type":"error","platform":"javascript","exception":{"type":"E","message":"x"},"debug_meta":{"images":` + tc.images + `}}`
			e, err := Normalize(json.RawMessage(raw), time.Now())
			if err != nil {
				t.Fatal(err) // a bad image never rejects the event
			}
			var got []DebugImage
			if e.DebugMeta != nil {
				got = e.DebugMeta.Images
			}
			if len(got) != len(tc.want) || (len(got) > 0 && got[0] != tc.want[0]) {
				t.Errorf("images = %+v, want %+v", got, tc.want)
			}
			repaired := strings.Contains(strings.Join(e.Repairs, ";"), "debug_meta")
			if repaired != tc.repair {
				t.Errorf("repair noted = %v, want %v (%v)", repaired, tc.repair, e.Repairs)
			}
		})
	}
}

func TestDebugMetaCapped(t *testing.T) {
	var imgs []string
	for i := 0; i < maxDebugImages+5; i++ {
		imgs = append(imgs, `{"type":"sourcemap","code_file":"f.js","debug_id":"4f1a2b3c-0000-4d5e-8f90-abcdef012345"}`)
	}
	raw := `{"event_id":"8f14e45f-ceea-4f6a-9b1e-1f0b2c3d4e5f","type":"error","platform":"javascript","exception":{"type":"E","message":"x"},"debug_meta":{"images":[` + strings.Join(imgs, ",") + `]}}`
	e, err := Normalize(json.RawMessage(raw), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(e.DebugMeta.Images); n != maxDebugImages {
		t.Errorf("kept %d images, want %d", n, maxDebugImages)
	}
}
