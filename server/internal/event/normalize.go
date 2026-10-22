package event

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	maxFutureSkew   = time.Hour // phones with wrong clocks are common
	maxEventAge     = 30 * 24 * time.Hour
	maxFrames       = 250
	keepTopFrames   = 50  // nearest the crash site (Frames[0] is the throw)
	keepRootFrames  = 200 // nearest the program's entry point
	maxBreadcrumbs  = 100
	maxTags         = 50
	maxTagKey       = 32
	maxTagValue     = 200
	maxMessageBytes = 8 << 10
	maxTypeBytes    = 256
	maxFrameField   = 512
	maxCrumbMessage = 1 << 10
	maxRelease      = 200
	maxEnvironment  = 64
)

var knownPlatforms = []string{"javascript", "dart", "other"}

// Normalize decodes one event, rejects it if it is unusable, and otherwise
// repairs it in place. now is passed in (instead of calling time.Now) so
// tests are deterministic.
func Normalize(raw json.RawMessage, now time.Time) (*Event, error) {
	var e Event
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, ValidationError{{"event", "invalid JSON: " + err.Error()}}
	}
	// Server-owned fields: whatever the client sent is discarded.
	e.SDK, e.Repairs, e.ProjectID, e.Symbolication = SDK{}, nil, 0, nil
	e.ReceivedAt = now.UTC()

	if errs := validateRequired(&e); errs != nil {
		return nil, errs
	}
	clientEnd, _ := time.Parse(time.RFC3339Nano, e.Timestamp) // before repair: transactions shift by the difference
	e.repairTimestamp(now)
	e.repairFields()
	switch e.Type {
	case TypeSession:
		e.repairSession()
	case TypeTransaction:
		e.repairTransaction(clientEnd)
	}
	return &e, nil
}

// validateRequired checks the fields we cannot invent. Missing any of these
// makes the event useless, so it is dropped.
func validateRequired(e *Event) ValidationError {
	var errs ValidationError
	if u, err := uuid.Parse(e.EventID); err != nil {
		errs = append(errs, FieldError{"event_id", "must be a UUID"})
	} else if u == uuid.Nil {
		errs = append(errs, FieldError{"event_id", "must not be the nil UUID"})
	} else {
		e.EventID = u.String() // canonical lowercase form, so dedupe matches regardless of case
	}

	// Each type carries its own payload; payloads for other types are
	// dropped, so a session update can never create an issue or an
	// analytics event, and the other way round.
	analytics := func() { e.Name, e.DistinctID, e.AnonID, e.Properties = "", "", "", nil }
	switch e.Type {
	case TypeError:
		e.Session, e.Transaction = nil, nil
		analytics()
		if e.Exception == nil || (strings.TrimSpace(e.Exception.Type) == "" && strings.TrimSpace(e.Exception.Message) == "") {
			errs = append(errs, FieldError{"exception", "needs a type or a message"})
		}
	case TypeEvent, TypeIdentify:
		e.Session, e.Transaction = nil, nil
		errs = append(errs, validateAnalytics(e)...)
	case TypeSession:
		e.Exception, e.Transaction = nil, nil
		analytics()
		errs = append(errs, validateSession(e)...)
	case TypeTransaction:
		e.Exception, e.Session = nil, nil
		analytics()
		errs = append(errs, validateTransaction(e)...)
	default:
		errs = append(errs, FieldError{"type", fmt.Sprintf("%q is not supported (expected error, event, identify, session or transaction)", e.Type)})
	}
	return errs
}

func (e *Event) repairTimestamp(now time.Time) {
	t, err := time.Parse(time.RFC3339Nano, e.Timestamp)
	switch {
	case e.Timestamp == "":
		e.repair("timestamp: missing, used receive time")
		t = now
	case err != nil:
		e.repair("timestamp: not RFC 3339, used receive time")
		t = now
	case t.After(now.Add(maxFutureSkew)):
		e.repair("timestamp: more than 1h in the future, used receive time")
		t = now
	case t.Before(now.Add(-maxEventAge)):
		e.repair("timestamp: older than 30 days, used receive time")
		t = now
	}
	e.Time = t.UTC()
	e.Timestamp = e.Time.Format(time.RFC3339Nano)
}

func (e *Event) repairFields() {
	e.Platform = strings.ToLower(strings.TrimSpace(e.Platform))
	if !slices.Contains(knownPlatforms, e.Platform) {
		if e.Platform != "" {
			e.repair(fmt.Sprintf("platform: %q is unknown, stored as \"other\"", e.Platform))
		}
		e.Platform = "other"
	}
	if e.Environment = strings.TrimSpace(e.Environment); e.Environment == "" {
		e.Environment = "production"
	}
	e.Environment = e.truncate("environment", e.Environment, maxEnvironment)
	e.Release = e.truncate("release", e.Release, maxRelease)

	// Analytics events carry no stack, breadcrumbs or tags to repair. Ask by
	// type: sessions and transactions have no exception either.
	if e.IsAnalytics() {
		e.repairAnalytics()
		return
	}
	if ex := e.Exception; ex != nil {
		e.repairException(ex)
	}

	if n := len(e.Breadcrumbs); n > maxBreadcrumbs {
		e.Breadcrumbs = e.Breadcrumbs[n-maxBreadcrumbs:] // the most recent ones matter most
		e.repair(fmt.Sprintf("breadcrumbs: %d sent, kept the last %d", n, maxBreadcrumbs))
	}
	for i := range e.Breadcrumbs {
		e.Breadcrumbs[i].Message, _ = truncate(e.Breadcrumbs[i].Message, maxCrumbMessage)
	}

	e.repairTags()
	e.repairDebugMeta()
}

const maxDebugImages = 100

// repairDebugMeta keeps the images the server can use and drops the rest:
// a malformed one would only mean a lookup that can never succeed.
func (e *Event) repairDebugMeta() {
	dm := e.DebugMeta
	if dm == nil {
		return
	}
	kept := dm.Images[:0]
	dropped := 0
	for _, im := range dm.Images {
		id, err := uuid.Parse(im.DebugID)
		if im.Type != "sourcemap" || err != nil || id == uuid.Nil || im.CodeFile == "" || len(im.CodeFile) > maxFrameField || len(kept) == maxDebugImages {
			dropped++
			continue
		}
		im.DebugID = id.String() // one spelling: lowercase, with dashes
		kept = append(kept, im)
	}
	if dropped > 0 {
		e.repair(fmt.Sprintf("debug_meta: dropped %d image(s) that weren't a sourcemap with a code_file and a UUID debug_id, or were over %d", dropped, maxDebugImages))
	}
	if len(kept) == 0 {
		e.DebugMeta = nil
		return
	}
	dm.Images = kept
}

func (e *Event) repairException(ex *Exception) {
	ex.Type = e.truncate("exception.type", ex.Type, maxTypeBytes)
	ex.Message = e.truncate("exception.message", ex.Message, maxMessageBytes)
	st := ex.Stacktrace
	if st == nil {
		return
	}
	if n := len(st.Frames); n > maxFrames {
		// Keep both ends: the crash site and the entry point. Deep
		// recursion in the middle is the least useful part.
		st.Frames = slices.Concat(st.Frames[:keepTopFrames], st.Frames[n-keepRootFrames:])
		e.repair(fmt.Sprintf("exception.stacktrace: %d frames, kept first %d and last %d", n, keepTopFrames, keepRootFrames))
	}
	for i := range st.Frames {
		f := &st.Frames[i]
		f.Raw = nil // server-owned: set only by symbolication
		f.Function, _ = truncate(f.Function, maxFrameField)
		f.Filename, _ = truncate(f.Filename, maxFrameField)
	}
}

func (e *Event) repairTags() {
	if len(e.Tags) == 0 {
		e.Tags = nil
		return
	}
	// Map iteration order is random in Go. Sorting makes "which tags survive"
	// deterministic, so the same event is always stored the same way.
	keys := make([]string, 0, len(e.Tags))
	for k := range e.Tags {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	kept := make(map[string]string, min(len(keys), maxTags))
	dropped := 0
	for _, k := range keys {
		v := e.Tags[k]
		if k == "" || len(k) > maxTagKey || len(v) > maxTagValue || len(kept) == maxTags {
			dropped++
			continue
		}
		kept[k] = v
	}
	e.Tags = kept
	if dropped > 0 {
		e.repair(fmt.Sprintf("tags: dropped %d (max %d tags, keys ≤%d bytes, values ≤%d bytes)", dropped, maxTags, maxTagKey, maxTagValue))
	}
}

func (e *Event) truncate(field, s string, n int) string {
	out, cut := truncate(s, n)
	if cut {
		e.repair(fmt.Sprintf("%s: truncated to %d bytes", field, n))
	}
	return out
}

func (e *Event) repair(note string) { e.Repairs = append(e.Repairs, note) }

// truncate cuts s to at most n bytes without splitting a multi-byte UTF-8
// character (cutting "é" in half would store invalid text).
func truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}
