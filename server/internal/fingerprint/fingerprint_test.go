package fingerprint

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"juju/internal/event"
)

// Run `go test ./internal/fingerprint -update` to rewrite the golden files
// after an intentional algorithm change, then review the diff.
var update = flag.Bool("update", false, "rewrite testdata/*.golden")

func ev(typ, msg string, frames ...event.Frame) *event.Event {
	return &event.Event{Exception: &event.Exception{Type: typ, Message: msg, Stacktrace: &event.Stacktrace{Frames: frames}}}
}

func frame(fn, file string, line int, inApp bool) event.Frame {
	return event.Frame{Function: fn, Filename: file, Lineno: line, InApp: inApp}
}

// The core promises of grouping, each as a pair of events that must (or must
// not) land in the same issue.
func TestGrouping(t *testing.T) {
	cart := func(line int) *event.Event {
		return ev("StateError", "no element for id 812",
			frame("CartPage.build", "package:shop/cart.dart", line, true),
			frame("_buildItems", "package:shop/cart.dart", line+32, true),
			frame("StatelessElement.build", "package:flutter/src/widgets/framework.dart", 5, false),
		)
	}
	tests := []struct {
		name string
		a, b *event.Event
		same bool
	}{
		{"line numbers moved in a new release", cart(88), cart(91), true},
		{"different message data", cart(88), func() *event.Event { e := cart(88); e.Exception.Message = "no element for id 4471"; return e }(), true},
		{"different exception type", cart(88), func() *event.Event { e := cart(88); e.Exception.Type = "RangeError"; return e }(), false},
		{"framework frames differ, app frames same", cart(88), func() *event.Event {
			e := cart(88)
			e.Exception.Stacktrace.Frames[2] = frame("Element.rebuild", "package:flutter/src/widgets/framework.dart", 9, false)
			return e
		}(), true},
		{"different app function", cart(88), func() *event.Event {
			e := cart(88)
			e.Exception.Stacktrace.Frames[0].Function = "CheckoutPage.build"
			return e
		}(), false},
		{"recursion depth differs",
			ev("StackOverflowError", "", frame("walk", "tree.dart", 1, true), frame("walk", "tree.dart", 1, true), frame("main", "main.dart", 1, true)),
			ev("StackOverflowError", "", frame("walk", "tree.dart", 1, true), frame("walk", "tree.dart", 1, true), frame("walk", "tree.dart", 1, true), frame("main", "main.dart", 1, true)),
			true},
		{"same file built on two machines",
			ev("TypeError", "", frame("render", "/Users/mac/build/abc/app.js", 1, true)),
			ev("TypeError", "", frame("render", "/home/ci/work/app.js", 1, true)),
			true},
		{"bundle content hash changed between deploys",
			ev("TypeError", "", frame("render", "https://cdn.example.com/static/js/main.3f2a9c1b.js?v=2", 1, true)),
			ev("TypeError", "", frame("render", "https://staging.example.com/static/js/main.77d0e4aa.js", 1, true)),
			true},
		{"no stack: messages differ only by ids",
			ev("NetworkError", "request 8812 failed for user 3f2a9c1b-0000-4000-8000-000000000001"),
			ev("NetworkError", "request 17 failed for user 0b8e1d2c-1111-4111-8111-111111111112"),
			true},
		{"no stack: different messages",
			ev("NetworkError", "timeout"),
			ev("NetworkError", "connection refused"),
			false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, b := Compute(tc.a).Hash, Compute(tc.b).Hash
			if (a == b) != tc.same {
				t.Errorf("same issue = %v, want %v\n a: %s\n b: %s", a == b, tc.same, a, b)
			}
		})
	}
}

func TestTitleAndCulprit(t *testing.T) {
	r := Compute(ev("StateError", "Bad state: no element\nmore details",
		frame("dispatch", "package:flutter/src/x.dart", 1, false),
		frame("CartPage.build", "package:shop/cart.dart", 88, true),
	))
	if r.Title != "StateError: Bad state: no element" {
		t.Errorf("title = %q", r.Title)
	}
	if r.Culprit != "CartPage.build (package:shop/cart.dart)" {
		t.Errorf("culprit = %q", r.Culprit)
	}
	if long := Compute(ev("E", strings.Repeat("é", 300))).Title; len(long) > maxTitle+len("…") {
		t.Errorf("title not truncated: %d bytes", len(long))
	}
}

// TestGolden pins the exact hashes for realistic events from each platform.
// If this fails and you didn't mean to change grouping, you just would have
// regrouped every customer's issues.
func TestGolden(t *testing.T) {
	inputs, _ := filepath.Glob("testdata/*.json")
	if len(inputs) == 0 {
		t.Fatal("no testdata/*.json inputs")
	}
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, in := range inputs {
		name := strings.TrimSuffix(filepath.Base(in), ".json")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			e, err := event.Normalize(raw, now)
			if err != nil {
				t.Fatalf("testdata event is invalid: %v", err)
			}
			got, _ := json.MarshalIndent(Compute(e), "", "  ")
			got = append(got, '\n')

			golden := strings.TrimSuffix(in, ".json") + ".golden"
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if string(got) != string(want) {
				t.Errorf("fingerprint changed for %s\n got: %s\nwant: %s", name, got, want)
			}
		})
	}
}
