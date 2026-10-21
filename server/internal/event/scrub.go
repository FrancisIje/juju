package event

import (
	"regexp"
	"strconv"
	"strings"
)

// Scrub removes personal data and secrets that apps send by accident: an
// email in an error message, a token in a URL a breadcrumb recorded, an
// Authorization header copied into context. It runs at ingest, before the
// event is queued, so what it removes never reaches disk anywhere.
//
// Scrubbed:
//   - free text everywhere (messages, breadcrumbs, tags, context, span
//     descriptions, analytics properties): email addresses, secret-looking
//     URL parameters (?token=, ?password=, OAuth ?code= ...), Bearer/Basic
//     credentials and JWTs
//   - any value under a sensitive key (password, authorization, cookie ...)
//
// Not scrubbed: the fields an app fills in on purpose to say who the user
// is (user.id, user.email, user.username, distinct_id, anon_id), and
// stack frames, which are code locations that source maps are matched by.
//
// It returns how many values it changed.
func (e *Event) Scrub() int {
	s := scrubber{}
	if x := e.Exception; x != nil {
		x.Message = s.text(x.Message)
	}
	for i := range e.Breadcrumbs {
		e.Breadcrumbs[i].Message = s.text(e.Breadcrumbs[i].Message)
		s.object(e.Breadcrumbs[i].Data)
	}
	for k, v := range e.Tags {
		e.Tags[k] = s.value(k, v).(string)
	}
	s.object(e.Context)
	s.object(e.Properties)
	if tx := e.Transaction; tx != nil {
		tx.Name = s.text(tx.Name)
		for i := range tx.Spans {
			tx.Spans[i].Description = s.text(tx.Spans[i].Description)
			s.object(tx.Spans[i].Data)
		}
	}
	if s.n > 0 {
		e.Repairs = append(e.Repairs, "scrubbed "+strconv.Itoa(s.n)+" sensitive value(s)")
	}
	return s.n
}

const filtered = "[Filtered]"

type scrubber struct{ n int }

// object scrubs a JSON object in place, however deeply nested.
func (s *scrubber) object(m map[string]any) {
	for k, v := range m {
		m[k] = s.value(k, v)
	}
}

func (s *scrubber) value(key string, v any) any {
	if sensitiveKey(key) {
		if v == nil || v == "" || v == filtered {
			return v
		}
		s.n++
		return filtered
	}
	switch v := v.(type) {
	case string:
		return s.text(v)
	case map[string]any:
		s.object(v)
	case []any:
		for i := range v {
			v[i] = s.value("", v[i])
		}
	}
	return v
}

var (
	// Deliberately loose: a false positive costs a word in a message, a
	// miss leaks an address.
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}`)
	// A URL parameter: separator, name, "=", value up to the next one.
	paramRe  = regexp.MustCompile(`([?&;#])([^=&#?\s"'<>]+)=([^&#\s"'<>]*)`)
	authRe   = regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9\-._~+/]{8,}=*`)
	jwtRe    = regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}`)
	keyWords = []string{"password", "passwd", "secret", "token", "authorization", "cookie", "credential", "api_key", "apikey", "api-key", "private_key", "privatekey", "session_id", "sessionid", "card_number", "cardnumber", "credit_card", "creditcard", "cvv", "ssn", "jwt"}
	// Short names that are secrets as URL parameters but too common to
	// match inside longer words: ?code= is an OAuth code, ?sig= a signed
	// URL's signature, ?key= an API key.
	exactParams = map[string]bool{"code": true, "sig": true, "signature": true, "key": true, "auth": true, "pwd": true, "pass": true, "sid": true, "x-amz-signature": true, "x-amz-credential": true, "x-amz-security-token": true}
)

// sensitiveKey is true for names that always hold secrets, like
// "password", "Authorization" or "X-Api-Key".
func sensitiveKey(k string) bool {
	if k == "" {
		return false
	}
	k = strings.ToLower(k)
	for _, w := range keyWords {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

func (s *scrubber) text(t string) string {
	// Cheap checks first: most strings have none of these characters, and
	// this runs on every event at ingest.
	if !strings.ContainsAny(t, "@=") && !strings.Contains(t, "eyJ") && !containsFold(t, "bearer", "basic", "token") {
		return t
	}
	out := emailRe.ReplaceAllStringFunc(t, func(string) string { s.n++; return "[email]" })
	out = jwtRe.ReplaceAllStringFunc(out, func(string) string { s.n++; return "[jwt]" })
	out = authRe.ReplaceAllStringFunc(out, func(m string) string {
		s.n++
		return strings.Fields(m)[0] + " " + filtered
	})
	out = paramRe.ReplaceAllStringFunc(out, func(m string) string {
		p := paramRe.FindStringSubmatch(m)
		name, val := p[2], p[3]
		if val == "" || val == filtered || !(sensitiveKey(name) || exactParams[strings.ToLower(name)]) {
			return m
		}
		s.n++
		return p[1] + name + "=" + filtered
	})
	return out
}

func containsFold(s string, words ...string) bool {
	s = strings.ToLower(s)
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}
