package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// upload sends debug files to a juju server after a release build:
//
//	juju upload dist/                               (after juju sourcemaps inject)
//	juju upload -kind sourcemap -release web@2.3.1 dist/
//	juju upload -kind dart_symbols build/symbols/
//
// Directories are searched for *.map (source maps) or *.symbols (Flutter's
// --split-debug-info output). Server, project and secret key come from
// flags or JUJU_URL, JUJU_PROJECT and JUJU_SECRET_KEY, which suits CI,
// where secrets are passed as environment variables.
func upload(args []string) error {
	fs := flag.NewFlagSet("upload", flag.ContinueOnError)
	server := fs.String("url", envOr("JUJU_URL", "http://localhost:8080"), "juju server URL (JUJU_URL)")
	project := fs.String("project", os.Getenv("JUJU_PROJECT"), "project id (JUJU_PROJECT)")
	secret := fs.String("secret", os.Getenv("JUJU_SECRET_KEY"), "project secret key (JUJU_SECRET_KEY)")
	kind := fs.String("kind", "sourcemap", `"sourcemap" or "dart_symbols"`)
	release := fs.String("release", "", "release the source maps belong to, e.g. web@2.3.1 (must match the SDK's release). Not needed for maps with a debug id")
	name := fs.String("name", "", "bundle name for a single source map (default: the file name without .map)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" || *secret == "" {
		return errors.New("set -project and -secret (or JUJU_PROJECT and JUJU_SECRET_KEY)")
	}
	ext := ".map"
	switch *kind {
	case "sourcemap":
		// Without -release, each map must have a debug id; the server says
		// so for any that doesn't.
	case "dart_symbols":
		ext = ".symbols"
	default:
		return fmt.Errorf("unknown -kind %q", *kind)
	}

	files, err := findFiles(fs.Args(), ext)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no %s files found in %v", ext, fs.Args())
	}
	if *name != "" && len(files) > 1 {
		return errors.New("-name only works with a single file")
	}

	client := &http.Client{Timeout: 15 * time.Minute}
	failed := 0
	for _, path := range files {
		q := url.Values{"kind": {*kind}}
		if *kind == "sourcemap" {
			bundle := *name
			if bundle == "" {
				// app.min.js.map maps app.min.js: that's the name frames will
				// be matched against.
				bundle = strings.TrimSuffix(filepath.Base(path), ".map")
			}
			if *release != "" {
				q.Set("release", *release)
				q.Set("name", bundle)
			}
		}
		if err := uploadOne(client, *server, *project, *secret, path, q); err != nil {
			fmt.Fprintf(os.Stderr, "✗ %s: %v\n", path, err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d uploads failed", failed, len(files))
	}
	return nil
}

func uploadOne(client *http.Client, server, project, secret, path string, q url.Values) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	endpoint := strings.TrimRight(server, "/") + "/api/v1/projects/" + url.PathEscape(project) + "/files?" + q.Encode()
	req, err := http.NewRequest("POST", endpoint, f) // streamed from disk, never fully in memory
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusCreated {
		var e struct{ Error string }
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s: %s", res.Status, e.Error)
		}
		return fmt.Errorf("%s", res.Status)
	}
	var saved struct {
		Kind, Release, Name, SHA256 string
		Size                        int64
		DebugID                     string `json:"debug_id"`
	}
	_ = json.Unmarshal(body, &saved)
	what := fmt.Sprintf("%s %q", saved.Kind, saved.Name)
	if saved.DebugID != "" {
		what = "source map, debug id " + saved.DebugID
	}
	fmt.Printf("✓ %s → %s (%s, sha256 %s…)\n", path, what, humanSize(saved.Size), saved.SHA256[:min(12, len(saved.SHA256))])
	return nil
}

// findFiles expands directories into the files with ext inside them.
func findFiles(paths []string, ext string) ([]string, error) {
	var out []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			out = append(out, p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(d.Name(), ext) {
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

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
