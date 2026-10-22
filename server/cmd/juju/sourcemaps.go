package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"juju/internal/debugid"
)

// sourcemaps inject stamps a web build's bundles and their source maps with
// debug ids, so juju finds the right map for each bundle whatever the
// release is called (see internal/debugid):
//
//	juju sourcemaps inject dist/
//
// Run it after the build and before deploying and uploading: the bundles
// you deploy must be the stamped ones. Running it again changes nothing.
func sourcemaps(args []string) error {
	if len(args) == 0 || args[0] != "inject" || len(args) < 2 {
		return errors.New("usage: juju sourcemaps inject PATH...  (the build output: directories or .js files)")
	}
	bundles, err := findBundles(args[1:])
	if err != nil {
		return err
	}
	stamped, skipped, failed := 0, 0, 0
	for _, js := range bundles {
		mapPath, err := sourceMapOf(js)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %s: %v\n", js, err)
			failed++
			continue
		}
		if mapPath == "" {
			skipped++ // no source map: nothing to match it with
			continue
		}
		id, err := injectFile(js, mapPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %s: %v\n", js, err)
			failed++
			continue
		}
		fmt.Printf("✓ %s  debug id %s\n", js, id)
		stamped++
	}
	if skipped > 0 {
		fmt.Printf("  %d .js file(s) without a source map left as they were\n", skipped)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d bundles failed", failed, len(bundles))
	}
	if stamped == 0 {
		return fmt.Errorf("no .js file with a source map found in %v: build with source maps on (for example esbuild --sourcemap)", args[1:])
	}
	return nil
}

func findBundles(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			switch filepath.Ext(path) {
			case ".js", ".mjs", ".cjs":
				out = append(out, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

var mappingURL = regexp.MustCompile(`(?m)^//[#@] sourceMappingURL=(\S+)\s*$`)

// sourceMapOf finds a bundle's map: the file its sourceMappingURL comment
// names, else bundle.js.map next to it. "" means it has none.
func sourceMapOf(js string) (string, error) {
	data, err := os.ReadFile(js)
	if err != nil {
		return "", err
	}
	tail := data[max(0, len(data)-4096):] // the comment is at the end
	if m := mappingURL.FindAllSubmatch(tail, -1); len(m) > 0 {
		url := string(m[len(m)-1][1])
		if strings.HasPrefix(url, "data:") {
			return "", errors.New("the source map is inline (a data: URL): build it as a separate .map file to upload it")
		}
		if strings.Contains(url, "://") {
			return "", fmt.Errorf("the source map is at %s, not a local file", url)
		}
		p := filepath.Join(filepath.Dir(js), filepath.FromSlash(url))
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("its source map %s doesn't exist", p)
	}
	if _, err := os.Stat(js + ".map"); err == nil {
		return js + ".map", nil
	}
	return "", nil
}

func injectFile(jsPath, mapPath string) (string, error) {
	js, err := os.ReadFile(jsPath)
	if err != nil {
		return "", err
	}
	sm, err := os.ReadFile(mapPath)
	if err != nil {
		return "", err
	}
	newJS, newMap, id, err := debugid.Inject(js, sm)
	if err != nil {
		return "", err
	}
	// If this stops between the two writes, running it again fixes it:
	// Inject reads what's already done from the files (see its comment).
	if err := writeAtomic(mapPath, newMap); err != nil {
		return "", err
	}
	if !bytes.Equal(js, newJS) {
		if err := writeAtomic(jsPath, newJS); err != nil {
			return "", err
		}
	}
	return id, nil
}

// writeAtomic replaces a file in one step, so an interrupted run never
// leaves half a bundle behind.
func writeAtomic(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".juju-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), info.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
