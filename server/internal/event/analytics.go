package event

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Event types. "error" goes to issues; "event" and "identify" go to product
// analytics (ClickHouse).
const (
	TypeError    = "error"
	TypeEvent    = "event"
	TypeIdentify = "identify"
)

const (
	maxEventName     = 200
	maxDistinctID    = 200
	maxProperties    = 100
	maxPropertyKey   = 64
	maxPropertyValue = 1 << 10
)

// IsAnalytics reports whether e is for product analytics, not issues.
func (e *Event) IsAnalytics() bool { return e.Type == TypeEvent || e.Type == TypeIdentify }

func validateAnalytics(e *Event) ValidationError {
	var errs ValidationError
	// An analytics event never becomes an issue, whatever else it carries.
	e.Exception, e.Breadcrumbs, e.User, e.Tags = nil, nil, nil, nil
	e.DistinctID = strings.TrimSpace(e.DistinctID)
	if e.DistinctID == "" {
		errs = append(errs, FieldError{"distinct_id", "is required: who did this (an anonymous id or your user id)"})
	} else if len(e.DistinctID) > maxDistinctID {
		errs = append(errs, FieldError{"distinct_id", fmt.Sprintf("must be at most %d bytes", maxDistinctID)})
	}
	switch e.Type {
	case TypeEvent:
		e.AnonID = ""
		e.Name = strings.TrimSpace(e.Name)
		if e.Name == "" {
			errs = append(errs, FieldError{"name", "is required, e.g. \"checkout_started\""})
		} else if len(e.Name) > maxEventName {
			errs = append(errs, FieldError{"name", fmt.Sprintf("must be at most %d bytes", maxEventName)})
		}
	case TypeIdentify:
		e.Name = "$identify"
		e.AnonID = strings.TrimSpace(e.AnonID)
		if len(e.AnonID) > maxDistinctID {
			errs = append(errs, FieldError{"anon_id", fmt.Sprintf("must be at most %d bytes", maxDistinctID)})
		}
	}
	return errs
}

// repairAnalytics turns property values into text, the one type every
// breakdown and filter can handle the same way: 42 -> "42", true -> "true",
// a nested object -> its JSON. Too many or too long properties are cut, as
// with tags.
func (e *Event) repairAnalytics() {
	if len(e.Properties) == 0 {
		e.Properties = nil
		return
	}
	keys := make([]string, 0, len(e.Properties))
	for k := range e.Properties {
		keys = append(keys, k)
	}
	slices.Sort(keys) // deterministic: the same event is always kept the same way
	kept := make(map[string]any, min(len(keys), maxProperties))
	dropped := 0
	for _, k := range keys {
		v, ok := propertyText(e.Properties[k])
		key := strings.TrimSpace(k)
		if !ok || key == "" || len(key) > maxPropertyKey || len(kept) == maxProperties {
			dropped++
			continue
		}
		v, _ = truncate(v, maxPropertyValue)
		kept[key] = v
	}
	e.Properties = kept
	if dropped > 0 {
		e.repair(fmt.Sprintf("properties: dropped %d (max %d, keys ≤%d bytes, no nulls)", dropped, maxProperties, maxPropertyKey))
	}
}

func propertyText(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	default:
		b, err := json.Marshal(x)
		return string(b), err == nil
	}
}

// StringProperties returns the (already normalized) properties as text.
func (e *Event) StringProperties() map[string]string {
	out := make(map[string]string, len(e.Properties))
	for k, v := range e.Properties {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}
