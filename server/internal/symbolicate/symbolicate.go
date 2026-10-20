// Package symbolicate turns minified (JavaScript) and obfuscated (Dart)
// stack frames back into source files, lines and functions, using the
// debug files apps upload at build time.
//
// It runs in the worker before fingerprinting, so grouping uses real names
// and files. Those stay the same across builds, while minified names like
// "o" and bundle hashes change every deploy.
//
// It never drops an event. When a file is missing or a lookup fails, the
// original frames are kept and Event.Symbolication says why.
package symbolicate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"juju/internal/event"
)

// ErrNotFound means no debug file was uploaded for that release, bundle
// or build.
var ErrNotFound = errors.New("debug file not found")

// Files gives the worker the uploaded debug files' bytes.
type Files interface {
	SourceMap(ctx context.Context, projectID int64, release, name string) ([]byte, error)
	DartSymbols(ctx context.Context, projectID int64, buildID string) ([]byte, error)
}

type Service struct {
	Files Files
	Log   *slog.Logger
	cache *cache
	once  sync.Once
}

const (
	cacheSize = 32 // parsed debug files kept in memory
	// How long "not uploaded" is remembered. An error storm shouldn't mean
	// one database query per event; a new upload applies within this time.
	missTTL = 30 * time.Second
)

// Symbolicate rewrites e's frames in place.
func (s *Service) Symbolicate(ctx context.Context, e *event.Event) {
	s.once.Do(func() { s.cache = newCache(cacheSize) })
	if e.Exception == nil || e.Exception.Stacktrace == nil || len(e.Exception.Stacktrace.Frames) == 0 {
		return
	}
	var r result
	if buildID := dartBuildID(e); buildID != "" {
		r = s.dart(ctx, e, buildID)
	} else if e.Platform == "javascript" {
		r = s.javascript(ctx, e)
	} else {
		return
	}
	if r.status != "" {
		e.Symbolication = &event.Symbolication{Status: r.status, Notes: r.notes}
	}
}

type result struct {
	status string
	notes  []string
}

func outcome(done, wanted int, notes []string) result {
	switch {
	case wanted == 0:
		return result{}
	case done == wanted:
		return result{status: "symbolicated", notes: notes}
	case done > 0:
		return result{status: "partial", notes: notes}
	default:
		return result{status: "missing", notes: notes}
	}
}

func (s *Service) javascript(ctx context.Context, e *event.Event) result {
	frames := e.Exception.Stacktrace.Frames
	var notes []string
	noted := map[string]bool{}
	note := func(n string) {
		if !noted[n] {
			noted[n] = true
			notes = append(notes, n)
		}
	}

	// Look up every frame first: the function name of frame i can come
	// from frame i+1, its caller (see below).
	type mapped struct {
		loc  location
		name string // the original identifier at this position
		ok   bool
		sm   *sourceMap
	}
	looked := make([]mapped, len(frames))
	wanted, done := 0, 0
	for i, f := range frames {
		if f.Filename == "" || f.Lineno == 0 || f.Colno == 0 {
			continue // native frames like "Array.map (<anonymous>)"
		}
		bundle := bundleName(f.Filename)
		if bundle == "" {
			continue
		}
		wanted++
		sm, err := s.sourceMap(ctx, e.ProjectID, e.Release, bundle)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				note(fmt.Sprintf("no source map uploaded for %q in release %q", bundle, e.Release))
			} else {
				note(fmt.Sprintf("source map for %q could not be used: %v", bundle, err))
			}
			continue
		}
		loc, name, ok := sm.lookup(f.Lineno, f.Colno)
		if !ok {
			note(fmt.Sprintf("%s:%d:%d is not covered by its source map", bundle, f.Lineno, f.Colno))
			continue
		}
		looked[i] = mapped{loc: loc, name: name, ok: true, sm: sm}
	}

	for i := range frames {
		m := looked[i]
		if !m.ok {
			continue
		}
		f := &frames[i]
		// Which function is this frame in? Two ways, best first:
		//  1. The caller's call site. Frame i+1 is paused on the call into
		//     this function, and the source map's name there is this
		//     function's original name (cartTotal in "cartTotal(cart)").
		//  2. Read the original source upward to the enclosing function.
		//     Needed for the top frame when its caller is native code.
		fn := ""
		if i+1 < len(frames) && looked[i+1].ok && looked[i+1].name != "" {
			fn = looked[i+1].name
		}
		if fn == "" {
			fn = m.sm.enclosingFunction(m.loc.File, m.loc.Line, m.loc.Col)
		}
		if fn == "" {
			fn = f.Function // keep the minified name rather than none
		}
		f.Raw = &event.RawFrame{Function: f.Function, Filename: f.Filename, Lineno: f.Lineno, Colno: f.Colno}
		f.Function, f.Filename, f.Lineno, f.Colno = fn, m.loc.File, m.loc.Line, m.loc.Col
		// Now that frames point at real source files, dependencies can be
		// told apart from app code, even inside a single bundle.
		f.InApp = !strings.Contains(f.Filename, "node_modules/")
		done++
	}
	return outcome(done, wanted, notes)
}

func (s *Service) dart(ctx context.Context, e *event.Event, buildID string) result {
	syms, err := s.dartSymbols(ctx, e.ProjectID, buildID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return result{status: "missing", notes: []string{fmt.Sprintf("no Dart symbols uploaded for build id %s", buildID)}}
		}
		return result{status: "failed", notes: []string{fmt.Sprintf("Dart symbols for build id %s could not be used: %v", buildID, err)}}
	}
	in := e.Exception.Stacktrace.Frames
	out := make([]event.Frame, 0, len(in))
	wanted, done := 0, 0
	var notes []string
	for i, f := range in {
		if f.Filename != "<non-symbolic>" {
			out = append(out, f)
			continue
		}
		wanted++
		addr, ok := syms.address(f.Function)
		var locs []location
		found := false
		if ok {
			locs, found = syms.find(addr, i == 0)
		}
		if !found {
			notes = append(notes, fmt.Sprintf("no debug info for %s", f.Function))
			out = append(out, f)
			continue
		}
		done++ // found; it may expand to several frames, or to none (hidden compiler helpers)
		raw := &event.RawFrame{Function: f.Function, Filename: f.Filename}
		for _, l := range locs { // one address can be several frames, when functions were inlined
			out = append(out, event.Frame{
				Function: l.Function, Filename: l.File, Lineno: l.Line, Colno: l.Col,
				InApp: dartInApp(l.File),
				Raw:   raw,
			})
		}
	}
	e.Exception.Stacktrace.Frames = out
	return outcome(done, wanted, notes)
}

// dartInApp tells app code from the SDK, Flutter and dependencies by the
// file paths Dart's debug info uses, for example:
//
//	third_party/dart/sdk/lib/async/future.dart                Dart SDK
//	/Users/me/flutter/packages/flutter/lib/src/widgets/…      Flutter framework
//	…/flutter/lib/ui/hooks.dart                               dart:ui
//	/Users/me/.pub-cache/hosted/pub.dev/http-1.6.0/lib/…      a dependency
//	/Users/me/shop/lib/main.dart                              the app
func dartInApp(file string) bool {
	for _, marker := range []string{"dart:", "package:flutter/", "third_party/dart/", "/dart-sdk/", "/packages/flutter/lib/", "flutter/lib/ui/", "/.pub-cache/", "/pub-cache/"} {
		if strings.HasPrefix(file, marker) || strings.Contains(file, marker) {
			return false
		}
	}
	return file != ""
}

// bundleName is how frames are matched to uploaded source maps: the file
// name the browser or app reports, without host, path, query or fragment.
// https://shop.example.com/assets/app.min.js?v=2 → app.min.js
// /var/containers/…/App.app/main.jsbundle → main.jsbundle
func bundleName(filename string) string {
	if i := strings.IndexAny(filename, "?#"); i >= 0 {
		filename = filename[:i]
	}
	// Engine code never has a source map: native functions and Hermes's
	// own built-ins. Counting them as missing would make every React
	// Native event look partly unsymbolicated.
	base := path.Base(filename)
	if strings.HasPrefix(filename, "<") || strings.HasPrefix(filename, "[native") || filename == "native" || base == "InternalBytecode.js" {
		return ""
	}
	return base
}

// dartBuildID reads the build id the Flutter SDK puts in the event's
// context for non-symbolic traces.
func dartBuildID(e *event.Event) string {
	ds, ok := e.Context["dart_stack"].(map[string]any)
	if !ok {
		return ""
	}
	id, _ := ds["build_id"].(string)
	return id
}

func (s *Service) sourceMap(ctx context.Context, projectID int64, release, name string) (*sourceMap, error) {
	key := fmt.Sprintf("sm|%d|%s|%s", projectID, release, name)
	v, err := s.cache.get(key, func() (any, error) {
		data, err := s.Files.SourceMap(ctx, projectID, release, name)
		if err != nil {
			return nil, err
		}
		return parseSourceMap(name, data)
	})
	if err != nil {
		return nil, err
	}
	return v.(*sourceMap), nil
}

func (s *Service) dartSymbols(ctx context.Context, projectID int64, buildID string) (*dartSymbols, error) {
	key := fmt.Sprintf("dart|%d|%s", projectID, buildID)
	v, err := s.cache.get(key, func() (any, error) {
		data, err := s.Files.DartSymbols(ctx, projectID, buildID)
		if err != nil {
			return nil, err
		}
		return parseDartSymbols(data)
	})
	if err != nil {
		return nil, err
	}
	return v.(*dartSymbols), nil
}

// cache keeps recently used parsed files, since parsing a large source map
// or DWARF file for every event would be slow, and remembers recent
// "not found" answers for missTTL. Workers share it, hence the mutex.
type cache struct {
	mu    sync.Mutex
	max   int
	order []string
	items map[string]any
	miss  map[string]time.Time
}

func newCache(max int) *cache {
	return &cache{max: max, items: map[string]any{}, miss: map[string]time.Time{}}
}

func (c *cache) get(key string, load func() (any, error)) (any, error) {
	c.mu.Lock()
	if v, ok := c.items[key]; ok {
		c.mu.Unlock()
		return v, nil
	}
	if t, ok := c.miss[key]; ok && time.Since(t) < missTTL {
		c.mu.Unlock()
		return nil, ErrNotFound
	}
	c.mu.Unlock()

	// Loading happens outside the lock: a slow parse mustn't block workers
	// handling other files. Two workers may load the same file at once;
	// that only costs a little time.
	v, err := load()

	c.mu.Lock()
	defer c.mu.Unlock()
	if errors.Is(err, ErrNotFound) {
		c.miss[key] = time.Now()
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if _, ok := c.items[key]; !ok {
		c.order = append(c.order, key)
		if len(c.order) > c.max { // evict the oldest
			delete(c.items, c.order[0])
			c.order = c.order[1:]
		}
	}
	c.items[key] = v
	delete(c.miss, key)
	return v, nil
}
