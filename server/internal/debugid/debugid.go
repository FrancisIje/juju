// Package debugid ties a JavaScript bundle to its source map with a debug
// id: a UUID written into both files by the build that made them.
//
// Without it, juju matches a source map by the release name and file name
// the upload was given, and those must equal what the SDK sends. A typo, an
// auto-detected release that CI spells differently, or a rebuild that
// keeps the version number, and the trace stays minified (or worse, gets a
// different build's map). With it, each bundle names its own map.
//
// The id gets from the bundle to the SDK through a one-line snippet that
// runs first in the bundle and records the id under its own stack trace.
// The SDK reads the file name from that stack, so it knows which file the
// id belongs to, even with several bundles on one page:
//
//	globalThis.__jujuDebugIds["Error\n at https://shop.com/app.min.js:1:40"] = "<id>"
//
// Two ways to add it:
//   - web: `juju sourcemaps inject dist/` after the build (Inject), which
//     also writes the id into the .map as "debugId";
//   - React Native: the Metro plugin in @juju/react-native adds the same
//     snippet as a file of the bundle. Hermes compiles the bundle to
//     bytecode, so nothing can be added afterwards, but the snippet's
//     source, id included, ends up in the final map's sourcesContent.
//
// FromSourceMap reads the id back from either kind of map.
package debugid

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Snippet is the code that records id at run time. It must never throw:
// it runs before the app's own code. The SDK side is in
// sdks/js-core/src/debugids.ts, and the React Native copy in
// sdks/react-native/metro.js must stay identical (a test checks).
func Snippet(id string) string {
	return `;!function(){try{var g="undefined"!=typeof globalThis?globalThis:"undefined"!=typeof window?window:"undefined"!=typeof self?self:{},s=(new Error).stack;s&&((g.__jujuDebugIds=g.__jujuDebugIds||{})[s]="` + id + `")}catch(e){}}();`
}

var (
	snippetID = regexp.MustCompile(`__jujuDebugIds=g\.__jujuDebugIds\|\|\{\}\)\[s\]="([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})"`)
	validID   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// FromSourceMap returns the debug id of a source map, or "" if it has none:
// its "debugId" field (or "debug_id"), or else the id in the snippet's
// source when the bundler embedded it (React Native).
func FromSourceMap(data []byte) string {
	var m struct {
		DebugID        string            `json:"debugId"`
		DebugIDSnake   string            `json:"debug_id"`
		SourcesContent []*string         `json:"sourcesContent"`
		Sections       []json.RawMessage `json:"sections"`
	}
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	for _, id := range []string{m.DebugID, m.DebugIDSnake} {
		if validID.MatchString(id) {
			return strings.ToLower(id)
		}
	}
	for _, c := range m.SourcesContent {
		if c != nil {
			if sm := snippetID.FindStringSubmatch(*c); sm != nil {
				return sm[1]
			}
		}
	}
	// An index map ("sections") is several maps in one: look in each.
	for _, raw := range m.Sections {
		var s struct {
			Map json.RawMessage `json:"map"`
		}
		if json.Unmarshal(raw, &s) == nil && len(s.Map) > 0 {
			if id := FromSourceMap(s.Map); id != "" {
				return id
			}
		}
	}
	return ""
}

// FromBundle returns the id of a bundle Inject already stamped, or "".
func FromBundle(js []byte) string {
	if sm := snippetID.FindSubmatch(js); sm != nil {
		return string(sm[1])
	}
	return ""
}

// ForContent makes a debug id from a bundle's bytes. The same bundle
// always gets the same id, so running inject twice, or on two CI machines
// that built identical files, changes nothing.
func ForContent(js []byte) string {
	h := sha256.Sum256(js)
	h[6] = h[6]&0x0f | 0x40 // shaped like a version 4 UUID
	h[8] = h[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// Inject stamps a bundle and its source map with a debug id and returns the
// new contents.
//
// The snippet goes on a new first line, so it runs before anything else and
// even a crash while the bundle loads has an id. Every position in the old
// file is now one line further down, and the map is shifted to match.
//
// Each file's state is read from the files themselves, so running it again,
// even after a run that stopped between writing the two files, never
// shifts a map twice or leaves it unshifted: a map carrying the bundle's id
// was shifted when it got it.
func Inject(js, sourceMap []byte) (newJS, newMap []byte, id string, err error) {
	var m map[string]any
	if err := json.Unmarshal(sourceMap, &m); err != nil {
		return nil, nil, "", fmt.Errorf("source map isn't valid JSON: %w", err)
	}
	stamped := FromBundle(js)
	if stamped == "" && bytes.HasPrefix(js, []byte("#!")) {
		return nil, nil, "", errors.New("bundle starts with #!: not a browser bundle, not supported")
	}
	id = stamped
	if id == "" {
		id = ForContent(js)
	}
	if mapID, _ := m["debugId"].(string); mapID != id {
		if err := shift(m); err != nil {
			return nil, nil, "", err
		}
		m["debugId"] = id
	}
	if newMap, err = json.Marshal(m); err != nil {
		return nil, nil, "", err
	}
	if stamped != "" {
		return js, newMap, id, nil
	}
	return stamp(js, id), newMap, id, nil
}

// shift moves every mapping in m one line down, for the line stamp adds.
func shift(m map[string]any) error {
	if sections, ok := m["sections"].([]any); ok {
		for _, s := range sections {
			if off, ok := s.(map[string]any)["offset"].(map[string]any); ok {
				line, _ := off["line"].(float64)
				off["line"] = line + 1
			}
		}
		return nil
	}
	if mappings, ok := m["mappings"].(string); ok {
		m["mappings"] = ";" + mappings // ";" ends a line: line 1 now maps to nothing
		return nil
	}
	return errors.New(`source map has neither "mappings" nor "sections"`)
}

// stamp adds the snippet as a new first line, and a debugId comment at the
// end for people reading the file.
func stamp(js []byte, id string) []byte {
	// A file that starts with "use strict" is strict only while that is its
	// first statement. Putting the snippet before it would quietly turn the
	// whole bundle back to sloppy mode, so the new first line repeats the
	// directive, keeping it first.
	first := Snippet(id)
	if hasUseStrict(js) {
		first = `"use strict";` + first
	}
	var b bytes.Buffer
	b.Grow(len(js) + len(first) + 64)
	b.WriteString(first)
	b.WriteByte('\n')
	b.Write(js)
	if !bytes.HasSuffix(js, []byte("\n")) {
		b.WriteByte('\n')
	}
	b.WriteString("//# debugId=" + id + "\n")
	return b.Bytes()
}

// hasUseStrict reports whether the file's directive prologue (the strings
// at its very start, after comments) contains "use strict".
func hasUseStrict(js []byte) bool {
	s := string(js[:min(len(js), 64<<10)])
	for {
		s = strings.TrimLeft(s, " \t\r\n\ufeff")
		switch {
		case strings.HasPrefix(s, "//"):
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return false
			}
			s = s[i+1:]
		case strings.HasPrefix(s, "/*"):
			i := strings.Index(s, "*/")
			if i < 0 {
				return false
			}
			s = s[i+2:]
		case strings.HasPrefix(s, `"use strict"`), strings.HasPrefix(s, `'use strict'`):
			return true
		case strings.HasPrefix(s, `"`), strings.HasPrefix(s, `'`):
			// another directive, like "use client": skip it and keep looking
			q := s[0]
			i := strings.IndexByte(s[1:], q)
			if i < 0 {
				return false
			}
			s = strings.TrimLeft(s[i+2:], " \t")
			s = strings.TrimPrefix(s, ";")
		default:
			return false
		}
	}
}
