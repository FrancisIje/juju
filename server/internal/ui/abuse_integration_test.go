//go:build integration

package ui_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"

	"juju/internal/alert"
	"juju/internal/auth"
	"juju/internal/config"
	"juju/internal/httpx"
	"juju/internal/server"
	"juju/internal/store"
)

// With sign-up open, everything an account can do, a stranger can do. These
// check the limits that stop one person from turning that into abuse, the
// way a real deployment sees requests: through a reverse proxy on this
// machine that adds X-Forwarded-For.
func TestAbuseLimitsBehindAProxy(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.New(pool)
	loopback, _ := httpx.ParsePrefixes("127.0.0.1,::1")
	cfg := config.Config{PublicURL: "https://juju.test", AllowSignup: true, TrustedProxies: loopback}
	hook := &slack{}
	slackSrv := httptest.NewServer(hook)
	defer slackSrv.Close()
	srv := httptest.NewServer(server.Routes(server.Deps{Pool: pool, Store: st, Queue: newQueue(t), MaxUpload: 1 << 20, Auth: &auth.Service{Store: st},
		Alerts: &alert.Notifier{Settings: st, AllowHTTPWebhooks: true, Log: log}, Config: cfg, Log: log}))
	defer srv.Close()

	// call sends a request as if from client address ip, via the proxy.
	call := func(jar http.CookieJar, ip, method, path string, body any) *http.Response {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(auth.CSRFHeader, "1")
		req.Header.Set("X-Forwarded-For", ip)
		res, err := (&http.Client{Jar: jar}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	signup := func(ip, email string) *http.Response {
		jar, _ := cookiejar.New(nil)
		return call(jar, ip, "POST", "/api/ui/auth/signup", map[string]string{"email": email, "password": "correct horse battery", "organization": email})
	}

	t.Run("sign-ups per address", func(t *testing.T) {
		for i := 1; i <= 5; i++ {
			if res := signup("203.0.113.1", fmt.Sprintf("s%d@abuse.test", i)); res.StatusCode != http.StatusCreated {
				t.Fatalf("sign-up %d: %d", i, res.StatusCode)
			}
		}
		res := signup("203.0.113.1", "s6@abuse.test")
		if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
			t.Fatalf("6th sign-up from one address: %d, Retry-After %q; want 429 with Retry-After", res.StatusCode, res.Header.Get("Retry-After"))
		}
		if res := signup("203.0.113.2", "other@abuse.test"); res.StatusCode != http.StatusCreated {
			t.Fatalf("another address must still sign up: %d", res.StatusCode)
		}
	})

	t.Run("someone else's wrong guesses don't lock you out", func(t *testing.T) {
		jar, _ := cookiejar.New(nil)
		for i := 0; i < 10; i++ {
			call(jar, "203.0.113.66", "POST", "/api/ui/auth/login", map[string]string{"email": "s1@abuse.test", "password": "wrong password"})
		}
		if res := call(jar, "203.0.113.66", "POST", "/api/ui/auth/login", map[string]string{"email": "s1@abuse.test", "password": "correct horse battery"}); res.StatusCode != http.StatusTooManyRequests {
			t.Errorf("attacker's 11th try: %d, want 429", res.StatusCode)
		}
		owner, _ := cookiejar.New(nil)
		if res := call(owner, "198.51.100.7", "POST", "/api/ui/auth/login", map[string]string{"email": "s1@abuse.test", "password": "correct horse battery"}); res.StatusCode != http.StatusOK {
			t.Errorf("the account's owner, from their own address: %d, want 200", res.StatusCode)
		}
	})

	t.Run("test alerts per project", func(t *testing.T) {
		jar, _ := cookiejar.New(nil)
		call(jar, "198.51.100.20", "POST", "/api/ui/auth/signup", map[string]string{"email": "alerts@abuse.test", "password": "correct horse battery", "organization": "alerts-org"})
		var me struct{ Organizations []struct{ ID int64 } }
		get, _ := http.NewRequest("GET", srv.URL+"/api/ui/me", nil)
		res, _ := (&http.Client{Jar: jar}).Do(get)
		json.NewDecoder(res.Body).Decode(&me)
		res.Body.Close()
		var p struct{ ID int64 }
		b, _ := json.Marshal(map[string]string{"name": "p"})
		req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/ui/orgs/%d/projects", srv.URL, me.Organizations[0].ID), bytes.NewReader(b))
		req.Header.Set(auth.CSRFHeader, "1")
		res, _ = (&http.Client{Jar: jar}).Do(req)
		json.NewDecoder(res.Body).Decode(&p)
		res.Body.Close()
		call(jar, "198.51.100.20", "PUT", fmt.Sprintf("/api/ui/projects/%d/alerts", p.ID), map[string]any{"slack_webhook_url": slackSrv.URL})
		for i := 1; i <= 5; i++ {
			if res := call(jar, "198.51.100.20", "POST", fmt.Sprintf("/api/ui/projects/%d/alerts/test", p.ID), nil); res.StatusCode != http.StatusOK {
				t.Fatalf("test alert %d: %d", i, res.StatusCode)
			}
		}
		if res := call(jar, "198.51.100.20", "POST", fmt.Sprintf("/api/ui/projects/%d/alerts/test", p.ID), nil); res.StatusCode != http.StatusTooManyRequests {
			t.Errorf("6th test alert in 10 minutes: %d, want 429", res.StatusCode)
		}
		if n := len(hook.texts()); n != 5 {
			t.Errorf("slack received %d test alerts, want 5", n)
		}
	})

	t.Run("security headers on API responses", func(t *testing.T) {
		res, err := http.Get(srv.URL + "/api/ui/me")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("Strict-Transport-Security") == "" {
			t.Errorf("headers: nosniff %q, HSTS %q", res.Header.Get("X-Content-Type-Options"), res.Header.Get("Strict-Transport-Security"))
		}
	})
}
