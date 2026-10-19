// Package fingerprint decides which events are "the same bug".
//
// Events with equal fingerprints are grouped into one issue. The hash uses
// the parts of an event that identify a bug (exception type, the functions
// and files on the stack) and ignores the parts that vary between
// occurrences (line numbers, IDs inside messages, build paths).
//
// Changing this algorithm regroups every existing issue, so any change must
// show up in the golden files under testdata/.
package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"juju/internal/event"
)

// version is hashed in, so a future v2 algorithm can never collide with v1.
const version = "v1"

// maxFrames caps how much of the stack counts. Frames far from the crash
// site rarely identify the bug and make grouping fragile.
const maxFrames = 20

const maxTitle = 200

type Result struct {
	Hash    string // hex SHA-256, stored as issues.fingerprint
	Title   string // e.g. "StateError: Bad state: no element"
	Culprit string // where it broke, e.g. "CartPage.build (package:shop/cart.dart)"
}

func Compute(e *event.Event) Result {
	ex := e.Exception
	if ex == nil {
		ex = &event.Exception{}
	}
	frames := stackFrames(ex)

	parts := []string{version, strings.TrimSpace(ex.Type)}
	if len(frames) > 0 {
		parts = append(parts, frames...)
	} else {
		// No usable stack: fall back to the message, with variable data
		// (IDs, numbers) masked so "user 812" and "user 4471" still match.
		parts = append(parts, normalizeMessage(ex.Message))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))

	culprit := ""
	if len(frames) > 0 {
		culprit = frames[0]
	}
	return Result{Hash: hex.EncodeToString(sum[:]), Title: title(ex), Culprit: culprit}
}

// stackFrames returns normalized "function (file)" strings, crash site first.
func stackFrames(ex *event.Exception) []string {
	if ex.Stacktrace == nil {
		return nil
	}
	all := ex.Stacktrace.Frames
	// Prefer the app's own code. Framework frames (Flutter, React) appear in
	// almost every trace, so they say nothing about *this* bug. If the SDK
	// marked nothing as in-app, use everything rather than nothing.
	var chosen []event.Frame
	for _, f := range all {
		if f.InApp {
			chosen = append(chosen, f)
		}
	}
	if len(chosen) == 0 {
		chosen = all
	}

	var out []string
	for _, f := range chosen {
		fn, file := strings.TrimSpace(f.Function), normalizeFilename(f.Filename)
		if fn == "" && file == "" {
			continue
		}
		s := fn + " (" + file + ")"
		// Collapse recursion: a function calling itself 3 or 300 times is
		// the same bug.
		if len(out) > 0 && out[len(out)-1] == s {
			continue
		}
		out = append(out, s)
		if len(out) == maxFrames {
			break
		}
	}
	return out
}

var contentHash = regexp.MustCompile(`[.-][0-9a-f]{8,}(\.[A-Za-z0-9]+)$`)

// normalizeFilename removes the parts of a file path that change between
// builds or machines but do not change which code it is.
func normalizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.IndexAny(name, "?#"); i >= 0 {
		name = name[:i] // cache-busting query strings: app.js?v=123
	}
	switch {
	case strings.HasPrefix(name, "file://"):
		name = path.Base(name) // a local path, differs per machine
	case strings.Contains(name, "://"):
		// https://cdn.example.com/static/main.js -> /static/main.js
		// The host differs between staging and production; the path does not.
		rest := name[strings.Index(name, "://")+3:]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			name = rest[i:]
		} else {
			name = "/"
		}
	case strings.HasPrefix(name, "/"), isWindowsPath(name):
		// /Users/mac/build/abc/cart.dart and /home/ci/work/cart.dart are the
		// same file built on two machines.
		name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	}
	// main.3f2a9c1b.js -> main.js: bundlers put a content hash in the name
	// that changes on every deploy.
	return contentHash.ReplaceAllString(name, "$1")
}

func isWindowsPath(s string) bool {
	return len(s) >= 3 && s[1] == ':' && (s[2] == '\\' || s[2] == '/')
}

var (
	uuidPattern   = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	hexPattern    = regexp.MustCompile(`(?i)\b(0x[0-9a-f]+|[0-9a-f]{8,})\b`)
	numberPattern = regexp.MustCompile(`\d+`)
)

func normalizeMessage(msg string) string {
	msg = uuidPattern.ReplaceAllString(msg, "<uuid>")
	msg = hexPattern.ReplaceAllString(msg, "<hex>")
	msg = numberPattern.ReplaceAllString(msg, "<n>")
	return strings.TrimSpace(msg)
}

func title(ex *event.Exception) string {
	typ := strings.TrimSpace(ex.Type)
	msg, _, _ := strings.Cut(strings.TrimSpace(ex.Message), "\n")
	t := typ
	switch {
	case typ == "":
		t = msg
	case msg != "":
		t = typ + ": " + msg
	}
	if len(t) > maxTitle {
		n := maxTitle
		for n > 0 && !utf8.RuneStart(t[n]) {
			n--
		}
		t = t[:n] + "…"
	}
	return t
}
