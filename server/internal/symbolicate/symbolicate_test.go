package symbolicate

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"juju/internal/event"
)

// fakeFiles serves debug files from testdata, as if they'd been uploaded.
type fakeFiles struct {
	maps    map[string]string // "release|name" -> testdata file
	dart    map[string]string // build id -> testdata file
	lookups atomic.Int32
}

func (f *fakeFiles) SourceMap(_ context.Context, _ int64, release, name string) ([]byte, error) {
	f.lookups.Add(1)
	p, ok := f.maps[release+"|"+name]
	if !ok {
		return nil, ErrNotFound
	}
	return os.ReadFile(p)
}

func (f *fakeFiles) DartSymbols(_ context.Context, _ int64, buildID string) ([]byte, error) {
	f.lookups.Add(1)
	p, ok := f.dart[buildID]
	if !ok {
		return nil, ErrNotFound
	}
	return os.ReadFile(p)
}

func newService(f *fakeFiles) *Service { return &Service{Files: f} }

// framesFromV8 parses the real minified stack in testdata the way the web
// SDK does (frames with no position, like "Array.map (<anonymous>)", keep
// only their name).
func framesFromV8(t *testing.T, path string) []event.Frame {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`^\s+at (?:(\S+) \()?([^()\s]+?)(?::(\d+):(\d+))?\)?$`)
	var frames []event.Frame
	for _, line := range strings.Split(string(data), "\n") {
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		l, _ := strconv.Atoi(m[3])
		c, _ := strconv.Atoi(m[4])
		frames = append(frames, event.Frame{Function: m[1], Filename: m[2], Lineno: l, Colno: c, InApp: true})
	}
	return frames
}

func jsEvent(t *testing.T, release string) *event.Event {
	return &event.Event{
		Platform:  "javascript",
		Release:   release,
		Exception: &event.Exception{Type: "TypeError", Stacktrace: &event.Stacktrace{Frames: framesFromV8(t, "testdata/app.min.js.stack.txt")}},
	}
}

func TestJavaScriptFramesBecomeOriginalSource(t *testing.T) {
	files := &fakeFiles{maps: map[string]string{"web@1.0.0|app.min.js": "testdata/app.min.js.map"}}
	e := jsEvent(t, "web@1.0.0")
	newService(files).Symbolicate(context.Background(), e)

	got := e.Exception.Stacktrace.Frames
	want := []struct {
		fn, file string
		line     int
	}{
		{"lineTotal", "../app.js", 3}, // top frame: caller is native Array.map, so the name comes from the source
		{"Array.map", "<anonymous>", 0},
		{"cartTotal", "../app.js", 7}, // name from the caller's call site
		{"checkout", "../app.js", 11}, // paused on its call to cartTotal(cart)
		{"run", "../app.js", 17},      // paused on its call to checkout(...)
	}
	for i, w := range want {
		f := got[i]
		if f.Function != w.fn || !strings.HasSuffix(f.Filename, strings.TrimPrefix(w.file, "..")) || f.Lineno != w.line {
			t.Errorf("frame %d = %s %s:%d, want %s %s:%d", i, f.Function, f.Filename, f.Lineno, w.fn, w.file, w.line)
		}
	}
	// The original minified frame is kept for debugging.
	if r := got[0].Raw; r == nil || r.Function != "o" || r.Colno != 24 {
		t.Errorf("raw frame = %+v", got[0].Raw)
	}
	// The run.mjs frame (no source map) is untouched, so this is partial.
	if e.Symbolication == nil || e.Symbolication.Status != "partial" {
		t.Fatalf("symbolication = %+v", e.Symbolication)
	}
	if !strings.Contains(strings.Join(e.Symbolication.Notes, "|"), `no source map uploaded for "run.mjs"`) {
		t.Errorf("notes = %v", e.Symbolication.Notes)
	}
}

func TestJavaScriptWithoutUploadKeepsFramesAndSaysWhy(t *testing.T) {
	e := jsEvent(t, "web@2.0.0") // nothing uploaded for this release
	before := append([]event.Frame(nil), e.Exception.Stacktrace.Frames...)
	newService(&fakeFiles{}).Symbolicate(context.Background(), e)

	if e.Symbolication.Status != "missing" {
		t.Fatalf("status = %s", e.Symbolication.Status)
	}
	if e.Symbolication.Notes[0] != `no source map uploaded for "app.min.js" in release "web@2.0.0"` {
		t.Errorf("note = %q", e.Symbolication.Notes[0])
	}
	for i := range before {
		if e.Exception.Stacktrace.Frames[i].Function != before[i].Function {
			t.Fatal("frames must be left as sent when nothing could be looked up")
		}
	}
}

func TestDependenciesAreNotAppCodeOnceSymbolicated(t *testing.T) {
	// Every React Native frame is in one bundle file, so before
	// symbolication they all look like app code. After, node_modules shows.
	sm := []byte(`{"version":3,"sources":["App.tsx","node_modules/react-native/Libraries/Core/ExceptionsManager.js"],"names":[],"mappings":"AAAA,CCAA"}`)
	files := writeFiles(t, map[string][]byte{"index.android.bundle.map": sm})
	e := &event.Event{Platform: "javascript", Release: "shop@1", Exception: &event.Exception{Stacktrace: &event.Stacktrace{Frames: []event.Frame{
		{Function: "a", Filename: "index.android.bundle", Lineno: 1, Colno: 1, InApp: true},
		{Function: "b", Filename: "index.android.bundle", Lineno: 1, Colno: 2, InApp: true},
	}}}}
	newService(&fakeFiles{maps: map[string]string{"shop@1|index.android.bundle": files["index.android.bundle.map"]}}).Symbolicate(context.Background(), e)
	fr := e.Exception.Stacktrace.Frames
	if fr[0].Filename != "App.tsx" || !fr[0].InApp || fr[1].InApp {
		t.Fatalf("frames = %+v", fr)
	}
}

func TestDartEventIsExpandedWithInlinedFrames(t *testing.T) {
	trace, _ := os.ReadFile("testdata/dart_macos_arm64.trace.txt")
	re := regexp.MustCompile(`#\d+ abs \S+ virt \S+ (\S+)`)
	var frames []event.Frame
	for _, m := range re.FindAllStringSubmatch(string(trace), -1) {
		frames = append(frames, event.Frame{Function: m[1], Filename: "<non-symbolic>", InApp: true}) // as the Flutter SDK sends them
	}
	e := &event.Event{
		Platform:  "dart",
		Context:   map[string]any{"dart_stack": map[string]any{"build_id": "b4485c2e9c56418697e466d0c2ca3195"}},
		Exception: &event.Exception{Stacktrace: &event.Stacktrace{Frames: frames}},
	}
	newService(&fakeFiles{dart: map[string]string{"b4485c2e9c56418697e466d0c2ca3195": "testdata/dart_macos_arm64.debug"}}).Symbolicate(context.Background(), e)

	var got []string
	for _, f := range e.Exception.Stacktrace.Frames {
		got = append(got, f.Function)
	}
	if strings.Join(got, ",") != "CartPage.total,CartPage.build,checkout,main" {
		t.Fatalf("functions = %v", got)
	}
	if e.Symbolication.Status != "symbolicated" {
		t.Fatalf("status = %+v", e.Symbolication)
	}
	if r := e.Exception.Stacktrace.Frames[0].Raw; r == nil || r.Function != "_kDartIsolateSnapshotInstructions+0x34c67" {
		t.Errorf("raw = %+v", r)
	}
}

func TestDartWithoutSymbolsSaysWhichBuild(t *testing.T) {
	e := &event.Event{
		Platform:  "dart",
		Context:   map[string]any{"dart_stack": map[string]any{"build_id": "abc123"}},
		Exception: &event.Exception{Stacktrace: &event.Stacktrace{Frames: []event.Frame{{Function: "_kDartIsolateSnapshotInstructions+0x10", Filename: "<non-symbolic>"}}}},
	}
	newService(&fakeFiles{}).Symbolicate(context.Background(), e)
	if e.Symbolication.Status != "missing" || !strings.Contains(e.Symbolication.Notes[0], "abc123") {
		t.Fatalf("symbolication = %+v", e.Symbolication)
	}
}

func TestParsedFilesAndMissesAreCached(t *testing.T) {
	files := &fakeFiles{maps: map[string]string{"web@1.0.0|app.min.js": "testdata/app.min.js.map"}}
	s := newService(files)
	s.Symbolicate(context.Background(), jsEvent(t, "web@1.0.0"))
	first := files.lookups.Load() // one per distinct file, found or not
	for range 4 {
		s.Symbolicate(context.Background(), jsEvent(t, "web@1.0.0"))
	}
	if n := files.lookups.Load(); n != first {
		t.Fatalf("lookups grew from %d to %d over 4 more events: parsed maps and misses must be cached", first, n)
	}
}

func TestOtherPlatformsAreLeftAlone(t *testing.T) {
	e := &event.Event{Platform: "dart", Exception: &event.Exception{Stacktrace: &event.Stacktrace{Frames: []event.Frame{{Function: "main", Filename: "package:shop/main.dart", Lineno: 3}}}}}
	newService(&fakeFiles{}).Symbolicate(context.Background(), e)
	if e.Symbolication != nil {
		t.Fatal("a symbolic Dart trace needs no symbolication")
	}
}

func TestEnclosingFunctionHeuristic(t *testing.T) {
	src := `function outer() {
  const helper = (x) => {
    return x.y;
  };
  if (ready) {
    doThing();
  }
  return helper(1);
}
class Cart {
  total(items) {
    return items.length;
  }
  onClick = (event) => {
    event.preventDefault();
  };
}
const short = (a) => a.b;`
	m := &sourceMap{lines: map[string][]string{"a.js": strings.Split(src, "\n")}}
	for _, tc := range []struct {
		line int
		want string
	}{
		{3, "helper"}, // inside the arrow function
		{6, "outer"},  // inside an if block, inside outer
		{8, "outer"},  // after helper's block closed
		{12, "total"}, // a class method
		{15, "onClick"},
		{18, "short"}, // a one-line arrow function
	} {
		if got := m.enclosingFunction("a.js", tc.line, 5); got != tc.want {
			t.Errorf("line %d: got %q, want %q", tc.line, got, tc.want)
		}
	}
}

func writeFiles(t *testing.T, files map[string][]byte) map[string]string {
	t.Helper()
	dir := t.TempDir()
	out := map[string]string{}
	for name, data := range files {
		p := dir + "/" + name
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		out[name] = p
	}
	return out
}

// Engines report 1-based columns; source maps count from 0. Here generated
// columns 0 and 1 come from different original lines, so an off-by-one
// picks the wrong one. (In real maps a segment usually spans a whole
// token, which hides the mistake; this map doesn't.)
func TestColumnsAreConvertedFromOneBased(t *testing.T) {
	sm, err := parseSourceMap("a.js", []byte(`{"version":3,"sources":["a.ts"],"names":[],"mappings":"AAAA,CACA"}`))
	if err != nil {
		t.Fatal(err)
	}
	loc, _, ok := sm.lookup(1, 1) // what an engine reports for the very first character
	if !ok || loc.Line != 1 {
		t.Fatalf("1:1 mapped to line %d (ok=%v), want line 1", loc.Line, ok)
	}
	loc, _, _ = sm.lookup(1, 2)
	if loc.Line != 2 {
		t.Fatalf("1:2 mapped to line %d, want line 2", loc.Line)
	}
}

// Workers share parsed files. Run with -race: concurrent lookups on one
// parsed source map and one Dart file must not race (a concurrent map
// write is a fatal error that recover() can't catch).
func TestConcurrentSymbolication(t *testing.T) {
	files := &fakeFiles{
		maps: map[string]string{"web@1.0.0|app.min.js": "testdata/app.min.js.map"},
		dart: map[string]string{"b4485c2e9c56418697e466d0c2ca3195": "testdata/dart_macos_arm64.debug"},
	}
	s := newService(files)
	trace, _ := os.ReadFile("testdata/dart_macos_arm64.trace.txt")
	re := regexp.MustCompile(`#\d+ abs \S+ virt \S+ (\S+)`)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			if i%2 == 0 {
				e := jsEvent(t, "web@1.0.0")
				s.Symbolicate(context.Background(), e)
				if e.Exception.Stacktrace.Frames[0].Function != "lineTotal" {
					t.Errorf("js: %s", e.Exception.Stacktrace.Frames[0].Function)
				}
				return
			}
			var frames []event.Frame
			for _, m := range re.FindAllStringSubmatch(string(trace), -1) {
				frames = append(frames, event.Frame{Function: m[1], Filename: "<non-symbolic>"})
			}
			e := &event.Event{Platform: "dart", Context: map[string]any{"dart_stack": map[string]any{"build_id": "b4485c2e9c56418697e466d0c2ca3195"}}, Exception: &event.Exception{Stacktrace: &event.Stacktrace{Frames: frames}}}
			s.Symbolicate(context.Background(), e)
			if f := e.Exception.Stacktrace.Frames[0]; f.Function != "CartPage.total" || f.Lineno != 11 {
				t.Errorf("dart: %+v", f)
			}
		})
	}
	wg.Wait()
}

// The whole path on the real obfuscated Flutter trace: frames as the SDK
// sends them in, frames matching Dart's decoder out, and none left raw.
// Some addresses are only compiler helpers: they must vanish, not stay as
// raw addresses with a "no debug info" note.
func TestFlutterAndroidTraceEndToEnd(t *testing.T) {
	trace, _ := os.ReadFile("testdata/flutter_android_arm64.trace.txt")
	// The file holds two traces, each starting with a "*** *** ***" line.
	// Keep just the first.
	var lines []string
	for i, l := range strings.Split(string(trace), "\n") {
		if i > 0 && strings.HasPrefix(l, "*** ***") {
			break
		}
		lines = append(lines, l)
	}
	first := strings.Join(lines, "\n")
	re := regexp.MustCompile(`#\d+ abs \S+ virt \S+ (\S+)`)
	var frames []event.Frame
	for _, m := range re.FindAllStringSubmatch(first, -1) {
		frames = append(frames, event.Frame{Function: m[1], Filename: "<non-symbolic>", InApp: true})
	}
	e := &event.Event{
		Platform:  "dart",
		Context:   map[string]any{"dart_stack": map[string]any{"build_id": "b718850940a6ab861ab806427ac43cab"}},
		Exception: &event.Exception{Stacktrace: &event.Stacktrace{Frames: frames}},
	}
	newService(&fakeFiles{dart: map[string]string{"b718850940a6ab861ab806427ac43cab": "testdata/flutter_android_arm64.symbols"}}).Symbolicate(context.Background(), e)
	if e.Symbolication.Status != "symbolicated" {
		t.Fatalf("symbolication = %+v", e.Symbolication)
	}
	var got []string
	for _, f := range e.Exception.Stacktrace.Frames {
		if f.Filename == "<non-symbolic>" {
			t.Fatalf("raw frame left behind: %+v", f)
		}
		got = append(got, f.Function)
	}
	want := "scenarios.<anonymous closure>.<anonymous closure>.<anonymous closure>,new Future.delayed.<anonymous closure>,Timer._createTimer.<anonymous closure>,_Timer._runTimers,_Timer._handleMessage,_RawReceivePort._handleMessage"
	if strings.Join(got, ",") != want {
		t.Fatalf("functions:\n got %s\nwant %s", strings.Join(got, ","), want)
	}
	if f := e.Exception.Stacktrace.Frames[0]; !strings.HasSuffix(f.Filename, "main.dart") || f.Lineno != 64 || !f.InApp {
		t.Errorf("top frame = %+v", f)
	}
	if f := e.Exception.Stacktrace.Frames[1]; f.InApp {
		t.Errorf("dart:async code marked as app code: %+v", f)
	}
}

func TestDartInApp(t *testing.T) {
	for file, want := range map[string]bool{
		"/Users/mac/juju/examples/flutter/lib/main.dart":                      true,
		"third_party/dart/sdk/lib/async/future.dart":                          false,
		"/Users/mac/flutter/packages/flutter/lib/src/widgets/framework.dart":  false,
		"../../../flutter/lib/ui/hooks.dart":                                  false,
		"/Users/mac/.pub-cache/hosted/pub.dev/http-1.6.0/lib/src/client.dart": false,
		"dart:core/errors.dart":                                               false,
		"":                                                                    false,
	} {
		if got := dartInApp(file); got != want {
			t.Errorf("dartInApp(%q) = %v, want %v", file, got, want)
		}
	}
}

func TestEngineFramesAreNotSymbolicated(t *testing.T) {
	for _, f := range []string{"native", "[native code]", "<anonymous>", "InternalBytecode.js", "/a/b/InternalBytecode.js"} {
		if got := bundleName(f); got != "" {
			t.Errorf("bundleName(%q) = %q: engine code never has a source map", f, got)
		}
	}
	if got := bundleName("https://shop.example.com/assets/app.min.js?v=2#x"); got != "app.min.js" {
		t.Errorf("bundleName = %q", got)
	}
}
