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
	e.SDK, e.Repairs, e.ProjectID = SDK{}, nil, 0
	e.ReceivedAt = now.UTC()

	if errs := validateRequired(&e); errs != nil {
		return nil, errs
	}
	e.repairTimestamp(now)
	e.repairFields()
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

	if e.Type != "error" {
		errs = append(errs, FieldError{"type", fmt.Sprintf("%q is not supported (expected \"error\")", e.Type)})
	} else if e.Exception == nil || (strings.TrimSpace(e.Exception.Type) == "" && strings.TrimSpace(e.Exception.Message) == "") {
		errs = append(errs, FieldError{"exception", "needs a type or a message"})
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

	ex := e.Exception
	ex.Type = e.truncate("exception.type", ex.Type, maxTypeBytes)
	ex.Message = e.truncate("exception.message", ex.Message, maxMessageBytes)
	if st := ex.Stacktrace; st != nil {
		if n := len(st.Frames); n > maxFrames {
			// Keep both ends: the crash site and the entry point. Deep
			// recursion in the middle is the least useful part.
			st.Frames = slices.Concat(st.Frames[:keepTopFrames], st.Frames[n-keepRootFrames:])
			e.repair(fmt.Sprintf("exception.stacktrace: %d frames, kept first %d and last %d", n, keepTopFrames, keepRootFrames))
		}
		for i := range st.Frames {
			f := &st.Frames[i]
			f.Function, _ = truncate(f.Function, maxFrameField)
			f.Filename, _ = truncate(f.Filename, maxFrameField)
		}
	}

	if n := len(e.Breadcrumbs); n > maxBreadcrumbs {
		e.Breadcrumbs = e.Breadcrumbs[n-maxBreadcrumbs:] // the most recent ones matter most
		e.repair(fmt.Sprintf("breadcrumbs: %d sent, kept the last %d", n, maxBreadcrumbs))
	}
	for i := range e.Breadcrumbs {
		e.Breadcrumbs[i].Message, _ = truncate(e.Breadcrumbs[i].Message, maxCrumbMessage)
	}

	e.repairTags()
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
