package symbolicate

import (
	"context"
	"os"
	"strings"
	"testing"

	"juju/internal/debugid"
	"juju/internal/event"
)

// Injecting a debug id adds a line at the top of the bundle. Every
// position must still map to exactly the same original location, one line
// down, or symbolicated traces would point at the wrong code. Checked for
// every column of every line, on a minified and a multi-line bundle.
func TestInjectedMapMapsSamePositions(t *testing.T) {
	for _, name := range []string{"app.min.js", "multiline.js"} {
		t.Run(name, func(t *testing.T) {
			js, _ := os.ReadFile("testdata/" + name)
			sm, _ := os.ReadFile("testdata/" + name + ".map")
			newJS, newMap, _, err := debugid.Inject(js, sm)
			if err != nil {
				t.Fatal(err)
			}
			before, err := parseSourceMap(name, sm)
			if err != nil {
				t.Fatal(err)
			}
			after, err := parseSourceMap(name, newMap)
			if err != nil {
				t.Fatal(err)
			}
			mapped := 0
			for i, line := range strings.Split(string(js), "\n") {
				for col := 1; col <= len(line)+1; col++ {
					l1, n1, ok1 := before.lookup(i+1, col)
					l2, n2, ok2 := after.lookup(i+2, col)
					if l1 != l2 || n1 != n2 || ok1 != ok2 {
						t.Fatalf("%d:%d was %v %q %v, after injecting %d:%d is %v %q %v", i+1, col, l1, n1, ok1, i+2, col, l2, n2, ok2)
					}
					if ok1 {
						mapped++
					}
				}
			}
			if mapped == 0 {
				t.Fatal("no position mapped at all: the test checked nothing")
			}
			// The snippet itself is juju's code, not the app's: it maps to nothing.
			first := strings.SplitN(string(newJS), "\n", 2)[0]
			for col := 1; col <= len(first); col++ {
				if _, _, ok := after.lookup(1, col); ok {
					t.Fatalf("snippet column %d maps to app source", col)
				}
			}
			t.Logf("%d positions checked", mapped)
		})
	}
}

// injectedEvent is the real V8 stack from testdata, as it looks once the
// bundle has a debug id: app.min.js frames are one line further down.
func injectedEvent(t *testing.T, release, id string) (*event.Event, *fakeFiles) {
	t.Helper()
	js, _ := os.ReadFile("testdata/app.min.js")
	sm, _ := os.ReadFile("testdata/app.min.js.map")
	_, newMap, gotID, err := debugid.Inject(js, sm)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		id = gotID
	}
	paths := writeFiles(t, map[string][]byte{"app.min.js.map": newMap})
	e := jsEvent(t, release)
	for i := range e.Exception.Stacktrace.Frames {
		if f := &e.Exception.Stacktrace.Frames[i]; strings.HasSuffix(f.Filename, "/app.min.js") {
			f.Lineno++
		}
	}
	e.DebugMeta = &event.DebugMeta{Images: []event.DebugImage{{Type: "sourcemap", CodeFile: "https://shop.example.com/assets/app.min.js", DebugID: id}}}
	return e, &fakeFiles{byID: map[string]string{gotID: paths["app.min.js.map"]}}
}

// The point of debug ids: the release name the SDK sends doesn't matter.
// Here it matches no upload at all, and the map is still found.
func TestSymbolicatesByDebugIDWhateverTheRelease(t *testing.T) {
	e, files := injectedEvent(t, "typo-in-ci@0.0.0", "")
	newService(files).Symbolicate(context.Background(), e)
	got := e.Exception.Stacktrace.Frames
	for i, w := range []struct {
		fn   string
		line int
	}{{"lineTotal", 3}, {"Array.map", 0}, {"cartTotal", 7}, {"checkout", 11}, {"run", 17}} {
		if got[i].Function != w.fn || got[i].Lineno != w.line {
			t.Errorf("frame %d = %s:%d, want %s:%d", i, got[i].Function, got[i].Lineno, w.fn, w.line)
		}
	}
	if r := got[0].Raw; r == nil || r.Lineno != 2 {
		t.Errorf("raw frame keeps what the app reported (line 2 after injection): %+v", r)
	}
}

// An SDK that sends a debug id the server has no map for (say, uploaded the
// old way, by release) still gets symbolicated.
func TestUnknownDebugIDFallsBackToRelease(t *testing.T) {
	files := &fakeFiles{maps: map[string]string{"web@1.0.0|app.min.js": "testdata/app.min.js.map"}}
	e := jsEvent(t, "web@1.0.0")
	e.DebugMeta = &event.DebugMeta{Images: []event.DebugImage{{Type: "sourcemap", CodeFile: "https://shop.example.com/assets/app.min.js", DebugID: "00000000-0000-4000-8000-000000000001"}}}
	newService(files).Symbolicate(context.Background(), e)
	if f := e.Exception.Stacktrace.Frames[0]; f.Function != "lineTotal" || f.Lineno != 3 {
		t.Errorf("frame 0 = %s:%d, want lineTotal:3 from the release upload", f.Function, f.Lineno)
	}
}

func TestMissingDebugIDNoteNamesIt(t *testing.T) {
	const id = "00000000-0000-4000-8000-000000000002"
	e, _ := injectedEvent(t, "web@9", id)
	newService(&fakeFiles{}).Symbolicate(context.Background(), e)
	want := `no source map uploaded with debug id ` + id + ` (for "app.min.js"), nor for "app.min.js" in release "web@9"`
	if notes := strings.Join(e.Symbolication.Notes, "|"); !strings.Contains(notes, want) {
		t.Errorf("notes = %s\nwant to contain %s", notes, want)
	}
}

func TestDebugIDTable(t *testing.T) {
	e := &event.Event{DebugMeta: &event.DebugMeta{Images: []event.DebugImage{
		{CodeFile: "https://shop.com/assets/app.js", DebugID: "a"},
		{CodeFile: "https://cdn.shop.com/vendor.js", DebugID: "b"},
		{CodeFile: "https://other.com/app.js", DebugID: "c"},
	}}}
	tab := debugIDs(e)
	for file, want := range map[string]string{
		"https://shop.com/assets/app.js":        "a", // exact
		"https://cdn.shop.com/vendor.js?v=3":    "b", // same file, query added by the browser
		"https://elsewhere.example.com/app.js":  "",  // two app.js images: ambiguous, so not guessed
		"https://elsewhere.example.com/main.js": "",
	} {
		if got := tab.of(file); got != want {
			t.Errorf("%s: got %q, want %q", file, got, want)
		}
	}
}
