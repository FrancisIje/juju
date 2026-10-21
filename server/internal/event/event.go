// Package event defines the v1 wire format SDKs send to juju, and the rules
// for validating and repairing it.
//
// The guiding rule: reject only what we cannot use, repair what we can. A
// real crash report with a broken timestamp is still worth keeping.
package event

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Limits. These are starting guesses modelled on similar tools; tune them
// once there is real traffic.
const (
	MaxBodyBytes         = 1 << 20 // 1 MB, applied both before and after gzip
	MaxEventsPerEnvelope = 100
)

// Envelope is one HTTP request body: a batch of events from one SDK.
//
// Events are kept as raw JSON so that one malformed event (say, "lineno" sent
// as a string) can be dropped on its own instead of failing the whole batch.
type Envelope struct {
	SDK    SDK               `json:"sdk"`
	Events []json.RawMessage `json:"events"`
}

type SDK struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Event is one thing an SDK reports: an error, or (Phase 7) an analytics
// event or identify call. Fields below the first blank line are set by the
// server and never trusted from the client.
type Event struct {
	EventID     string            `json:"event_id"`
	Type        string            `json:"type"`
	Timestamp   string            `json:"timestamp"`
	Platform    string            `json:"platform"`
	Release     string            `json:"release,omitempty"`
	Environment string            `json:"environment"`
	Exception   *Exception        `json:"exception,omitempty"`
	Breadcrumbs []Breadcrumb      `json:"breadcrumbs,omitempty"`
	Context     map[string]any    `json:"context,omitempty"`
	User        *User             `json:"user,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
	// Analytics: "event" (name, distinct_id, properties) and "identify"
	// (distinct_id is the known user, anon_id the id the device had before).
	Name       string         `json:"name,omitempty"`
	DistinctID string         `json:"distinct_id,omitempty"`
	AnonID     string         `json:"anon_id,omitempty"`
	Properties map[string]any `json:"properties,omitempty"`

	SDK           SDK            `json:"sdk"`
	ReceivedAt    time.Time      `json:"received_at"`
	Symbolication *Symbolication `json:"symbolication,omitempty"`
	Repairs       []string       `json:"repairs,omitempty"` // what Normalize changed, kept for debugging SDKs
	ProjectID     int64          `json:"-"`
	Time          time.Time      `json:"-"` // parsed Timestamp
}

type Exception struct {
	Type       string      `json:"type"`
	Message    string      `json:"message"`
	Stacktrace *Stacktrace `json:"stacktrace,omitempty"`
}

// Stacktrace frames are ordered most recent call first: Frames[0] is where
// the error was thrown. This matches how Dart and JavaScript print traces.
type Stacktrace struct {
	Frames []Frame `json:"frames"`
}

type Frame struct {
	Function string `json:"function,omitempty"`
	Filename string `json:"filename,omitempty"`
	Lineno   int    `json:"lineno,omitempty"`
	Colno    int    `json:"colno,omitempty"`
	InApp    bool   `json:"in_app"`
	// Raw is the frame as the SDK sent it (minified or obfuscated), kept
	// when symbolication replaced it, so nothing the app sent is lost.
	Raw *RawFrame `json:"raw,omitempty"`
}

type RawFrame struct {
	Function string `json:"function,omitempty"`
	Filename string `json:"filename,omitempty"`
	Lineno   int    `json:"lineno,omitempty"`
	Colno    int    `json:"colno,omitempty"`
}

// Symbolication records what the server did to the stack trace, and why
// when it couldn't: the first thing to check when frames still look minified.
type Symbolication struct {
	Status string   `json:"status"` // "symbolicated", "partial", "missing", "failed"
	Notes  []string `json:"notes,omitempty"`
}

type Breadcrumb struct {
	Type      string         `json:"type,omitempty"`
	Message   string         `json:"message,omitempty"`
	Timestamp string         `json:"timestamp,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

type User struct {
	ID       string `json:"id,omitempty"`
	Email    string `json:"email,omitempty"`
	Username string `json:"username,omitempty"`
}

// FieldError is one problem with one field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError collects every problem found, not just the first, so an SDK
// author debugging a dropped event sees everything wrong at once.
type ValidationError []FieldError

func (v ValidationError) Error() string {
	parts := make([]string, len(v))
	for i, fe := range v {
		parts[i] = fe.Field + ": " + fe.Message
	}
	return strings.Join(parts, "; ")
}

// ParseEnvelope decodes and checks the batch wrapper. Individual events are
// checked later by Normalize.
func ParseEnvelope(body []byte) (*Envelope, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var env Envelope
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	// Decode stops after the first JSON value, so `{...}garbage` would pass
	// silently. Asking for one more token must hit the end of input.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid JSON: unexpected data after the top-level object")
	}

	var errs ValidationError
	if strings.TrimSpace(env.SDK.Name) == "" {
		errs = append(errs, FieldError{"sdk.name", "is required"})
	}
	switch n := len(env.Events); {
	case n == 0:
		errs = append(errs, FieldError{"events", "must contain at least one event"})
	case n > MaxEventsPerEnvelope:
		errs = append(errs, FieldError{"events", fmt.Sprintf("has %d events, the maximum is %d", n, MaxEventsPerEnvelope)})
	}
	if errs != nil {
		return nil, errs
	}
	return &env, nil
}
