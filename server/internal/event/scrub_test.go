package event

import (
	"reflect"
	"testing"
)

func TestScrubText(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"email", "no user ada.lovelace+test@example.co.uk found", "no user [email] found"},
		{"two emails", "a@b.io and c.d@e.org", "[email] and [email]"},
		{"token param", "GET https://api.shop.com/cart?token=abc123&page=2", "GET https://api.shop.com/cart?token=[Filtered]&page=2"},
		{"access_token in fragment", "https://app.com/cb#access_token=xyz&state=1", "https://app.com/cb#access_token=[Filtered]&state=1"},
		{"oauth code", "/callback?code=4/0AY0e&scope=email", "/callback?code=[Filtered]&scope=email"},
		{"password param", "POST /login?user=ada&Password=hunter2", "POST /login?user=ada&Password=[Filtered]"},
		{"s3 signed url", "https://b.s3.amazonaws.com/f?X-Amz-Credential=AKIA%2F&X-Amz-Signature=deadbeef", "https://b.s3.amazonaws.com/f?X-Amz-Credential=[Filtered]&X-Amz-Signature=[Filtered]"},
		{"api_key param", "fetch failed: /v1?api_key=sk_live_123", "fetch failed: /v1?api_key=[Filtered]"},
		{"bearer", "header was Bearer eyJhbGciOiJIUzI1NiJ9xx.payload.sig", "header was Bearer [Filtered]"},
		{"jwt alone", "bad jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefgh", "bad jwt [jwt]"},
		{"github token", "auth: token ghp_16C7e42F292c6912E7710c838347Ae178B4a", "auth: token [Filtered]"},

		// Must not change: over-scrubbing destroys exactly the details
		// people debug with.
		{"plain message", "Cannot read properties of undefined (reading 'id')", "Cannot read properties of undefined (reading 'id')"},
		{"harmless params", "/search?q=shoes&page=2&sort=price", "/search?q=shoes&page=2&sort=price"},
		{"token inside a word", "/tokenizer?mode=fast", "/tokenizer?mode=fast"},
		{"key as a word", "missing key in map", "missing key in map"},
		{"npm scope", "Cannot find module '@babel/core'", "Cannot find module '@babel/core'"},
		{"already filtered", "?token=[Filtered]", "?token=[Filtered]"},
		{"empty value", "?token=&page=1", "?token=&page=1"},
		{"bearer word", "the bearer of bad news", "the bearer of bad news"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := scrubber{}
			if got := s.text(tc.in); got != tc.want {
				t.Errorf("\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestScrubEvent(t *testing.T) {
	e := &Event{
		Exception: &Exception{Type: "AuthError", Message: "login failed for bob@shop.com"},
		Breadcrumbs: []Breadcrumb{
			{Type: "http", Message: "GET /me?session_id=s3cr3t", Data: map[string]any{"url": "/me?token=t0k", "status": 401.0}},
		},
		Tags:    map[string]string{"route": "/login", "api_key": "sk_live_1"},
		Context: map[string]any{"request": map[string]any{"headers": map[string]any{"Authorization": "Basic YWxhZGRpbjpvcGVu", "Accept": "json"}}, "list": []any{"x@y.com", 3.0}},
		User:    &User{ID: "42", Email: "bob@shop.com"},
		Properties: map[string]any{
			"$current_url": "https://shop.com/reset?token=abc",
			"plan":         "pro",
			"password":     "hunter2",
		},
		DistinctID: "bob@shop.com",
		Transaction: &Transaction{
			Name:  "/reset",
			Spans: []Span{{Description: "GET https://api.com/u?email=bob@shop.com", Data: map[string]any{"cookie": "sid=1"}}},
		},
	}
	n := e.Scrub()

	want := &Event{
		Exception: &Exception{Type: "AuthError", Message: "login failed for [email]"},
		Breadcrumbs: []Breadcrumb{
			{Type: "http", Message: "GET /me?session_id=[Filtered]", Data: map[string]any{"url": "/me?token=[Filtered]", "status": 401.0}},
		},
		Tags:    map[string]string{"route": "/login", "api_key": "[Filtered]"},
		Context: map[string]any{"request": map[string]any{"headers": map[string]any{"Authorization": "[Filtered]", "Accept": "json"}}, "list": []any{"[email]", 3.0}},
		// Set on purpose to identify the user: kept.
		User: &User{ID: "42", Email: "bob@shop.com"},
		Properties: map[string]any{
			"$current_url": "https://shop.com/reset?token=[Filtered]",
			"plan":         "pro",
			"password":     "[Filtered]",
		},
		DistinctID: "bob@shop.com",
		Transaction: &Transaction{
			Name:  "/reset",
			Spans: []Span{{Description: "GET https://api.com/u?email=[email]", Data: map[string]any{"cookie": "[Filtered]"}}},
		},
		Repairs: []string{"scrubbed 10 sensitive value(s)"},
	}
	if !reflect.DeepEqual(e, want) {
		t.Errorf("\n got %+v\nwant %+v", e, want)
		t.Errorf("context: %+v", e.Context)
		t.Errorf("breadcrumbs: %+v", e.Breadcrumbs)
	}
	if n != 10 {
		t.Errorf("n = %d, want 10", n)
	}
	if e.Scrub() != 0 {
		t.Error("scrubbing twice changed more: Scrub must be idempotent")
	}
}

// Whatever an SDK sends, Scrub must not panic, and a second pass must find
// nothing left: if it did, the first pass left something half-scrubbed.
func FuzzScrub(f *testing.F) {
	for _, s := range []string{"a@b.co", "?token=x&code=y", "Bearer abcdefghijk", "eyJaaaaaa.bbbbbbb.ccccccc", "&=&=?", "@@..", "x?a=b@c.de&token="} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		s := scrubber{}
		once := s.text(in)
		s2 := scrubber{}
		if twice := s2.text(once); twice != once {
			t.Errorf("not idempotent:\n in    %q\n once  %q\n twice %q", in, once, twice)
		}
	})
}
