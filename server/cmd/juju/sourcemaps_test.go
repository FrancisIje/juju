package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"juju/internal/debugid"
)

func TestSourcemapsInject(t *testing.T) {
	dir := t.TempDir()
	js, _ := os.ReadFile("../../internal/symbolicate/testdata/multiline.js")
	sm, _ := os.ReadFile("../../internal/symbolicate/testdata/multiline.js.map")
	write := func(name string, data []byte) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The bundle names its map in a sourceMappingURL comment.
	write("assets/multiline.js", js)
	write("assets/multiline.js.map", sm)
	write("assets/no-map.js", []byte("console.log(1)\n"))

	if err := sourcemaps([]string{"inject", dir}); err != nil {
		t.Fatal(err)
	}
	gotJS, _ := os.ReadFile(filepath.Join(dir, "assets/multiline.js"))
	gotMap, _ := os.ReadFile(filepath.Join(dir, "assets/multiline.js.map"))
	id := debugid.FromBundle(gotJS)
	if id == "" || debugid.FromSourceMap(gotMap) != id {
		t.Fatalf("bundle id %q, map id %q", id, debugid.FromSourceMap(gotMap))
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "assets/no-map.js")); string(b) != "console.log(1)\n" {
		t.Error("a file without a source map was changed")
	}

	// Again: identical files.
	if err := sourcemaps([]string{"inject", dir}); err != nil {
		t.Fatal(err)
	}
	againJS, _ := os.ReadFile(filepath.Join(dir, "assets/multiline.js"))
	againMap, _ := os.ReadFile(filepath.Join(dir, "assets/multiline.js.map"))
	if string(againJS) != string(gotJS) || string(againMap) != string(gotMap) {
		t.Error("running inject twice changed the files")
	}
}

func TestSourcemapsInjectRefusesInlineMaps(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.js"), []byte("x()\n//# sourceMappingURL=data:application/json;base64,e30=\n"), 0o644)
	err := sourcemaps([]string{"inject", dir})
	if err == nil {
		t.Fatal("an inline map was accepted")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "app.js")); strings.Contains(string(b), "__jujuDebugIds") {
		t.Error("the bundle was changed anyway")
	}
}
