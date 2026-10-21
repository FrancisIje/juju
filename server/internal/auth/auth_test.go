package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"juju/internal/store"
)

func TestMain(m *testing.M) {
	// Production strength isn't what these tests check; speed is welcome.
	bcryptCost = bcrypt.MinCost
	// dummyHash (checked for unknown emails) was made at production cost
	// when the package loaded; remake it, or each unknown email costs a
	// real quarter second.
	dummyHash, _ = bcrypt.GenerateFromPassword([]byte("test"), bcryptCost)
	m.Run()
}

type fakeStore struct {
	mu       sync.Mutex
	users    map[string]string // email -> bcrypt hash
	sessions map[string]time.Time
}

func newFake(t *testing.T) *fakeStore {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	return &fakeStore{users: map[string]string{"ada@example.com": h}, sessions: map[string]time.Time{}}
}

func (f *fakeStore) UserByEmail(_ context.Context, email string) (store.User, string, error) {
	h, ok := f.users[email]
	if !ok {
		return store.User{}, "", store.ErrNotFound
	}
	return store.User{ID: 1, Email: email}, h, nil
}

func (f *fakeStore) CreateSession(_ context.Context, th []byte, _ int64, exp time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[string(th)] = exp
	return nil
}

func (f *fakeStore) SessionUser(_ context.Context, th []byte) (store.User, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	exp, ok := f.sessions[string(th)]
	if !ok || time.Now().After(exp) {
		return store.User{}, time.Time{}, store.ErrNotFound
	}
	return store.User{ID: 1, Email: "ada@example.com"}, exp, nil
}

func (f *fakeStore) ExtendSession(_ context.Context, th []byte, exp time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[string(th)] = exp
	return nil
}

func (f *fakeStore) DeleteSession(_ context.Context, th []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, string(th))
	return nil
}

func login(t *testing.T, s *Service, email, pw string) (*http.Cookie, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	_, err := s.Login(context.Background(), rec, email, pw, "10.0.0.1")
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName {
			return c, err
		}
	}
	return nil, err
}

func TestLoginSetsAProtectedCookie(t *testing.T) {
	s := &Service{Store: newFake(t), SecureCookies: true}
	c, err := login(t, s, " Ada@Example.com ", "correct horse battery") // email is normalized
	if err != nil {
		t.Fatal(err)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie = %+v: must be HttpOnly, Secure and SameSite=Lax", c)
	}
	if len(c.Value) < 40 {
		t.Errorf("token %q is too short to be unguessable", c.Value)
	}
	// Only the token's hash is stored, never the token itself.
	for k := range s.Store.(*fakeStore).sessions {
		if k == c.Value {
			t.Fatal("the raw token is stored")
		}
	}
}

func TestWrongPasswordAndUnknownEmailLookTheSame(t *testing.T) {
	s := &Service{Store: newFake(t)}
	_, err1 := login(t, s, "ada@example.com", "wrong password!")
	_, err2 := login(t, s, "nobody@example.com", "wrong password!")
	if err1 != ErrBadLogin || err2 != ErrBadLogin {
		t.Fatalf("errors %v / %v: both must be ErrBadLogin so emails can't be probed", err1, err2)
	}
}

func TestLoginRateLimit(t *testing.T) {
	s := &Service{Store: newFake(t)}
	for range 10 {
		_, _ = loginFrom(t, s, "ada@example.com", "guess", "203.0.113.66")
	}
	if _, err := loginFrom(t, s, "ada@example.com", "correct horse battery", "203.0.113.66"); err != ErrTooManyAttempts {
		t.Fatalf("err = %v: the 11th attempt must be refused even with the right password", err)
	}
	// The attacker's guesses must not lock Ada out: she logs in from her
	// own address. (Keyed on the email alone, they did.)
	if _, err := loginFrom(t, s, "ada@example.com", "correct horse battery", "198.51.100.7"); err != nil {
		t.Fatalf("err = %v: someone else's wrong guesses locked the real user out", err)
	}
}

// One address trying one password against many accounts (spraying) is
// stopped too, though it never tries any one account twice.
func TestLoginRateLimitPerAddress(t *testing.T) {
	s := &Service{Store: newFake(t)}
	for i := range 50 {
		if _, err := loginFrom(t, s, fmt.Sprintf("user%d@example.com", i), "Password123!", "203.0.113.66"); err != ErrBadLogin {
			t.Fatalf("attempt %d: err = %v", i+1, err)
		}
	}
	if _, err := loginFrom(t, s, "user99@example.com", "Password123!", "203.0.113.66"); err != ErrTooManyAttempts {
		t.Fatalf("err = %v: the 51st failure from one address must be refused", err)
	}
	if _, err := loginFrom(t, s, "ada@example.com", "correct horse battery", "198.51.100.7"); err != nil {
		t.Fatalf("another address must be unaffected: %v", err)
	}
}

// Successful logins don't count: a whole office behind one address can
// log in all day.
func TestSuccessfulLoginsDontCount(t *testing.T) {
	s := &Service{Store: newFake(t)}
	for i := range 100 {
		if _, err := loginFrom(t, s, "ada@example.com", "correct horse battery", "203.0.113.10"); err != nil {
			t.Fatalf("login %d: %v", i+1, err)
		}
	}
}

func loginFrom(t *testing.T, s *Service, email, pw, ip string) (*http.Cookie, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	_, err := s.Login(context.Background(), rec, email, pw, ip)
	return nil, err
}

func TestMiddleware(t *testing.T) {
	s := &Service{Store: newFake(t)}
	cookie, _ := login(t, s, "ada@example.com", "correct horse battery")
	h := s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := UserFrom(r.Context())
		_, _ = w.Write([]byte(u.Email))
	}))
	do := func(method string, c *http.Cookie, csrf bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/ui/me", nil)
		if c != nil {
			req.AddCookie(c)
		}
		if csrf {
			req.Header.Set(CSRFHeader, "1")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := do("GET", cookie, false); rec.Code != 200 || rec.Body.String() != "ada@example.com" {
		t.Errorf("logged in GET: %d %s", rec.Code, rec.Body)
	}
	if rec := do("GET", nil, false); rec.Code != 401 {
		t.Errorf("no cookie: %d", rec.Code)
	}
	if rec := do("POST", cookie, false); rec.Code != 403 || !strings.Contains(rec.Body.String(), CSRFHeader) {
		t.Errorf("POST without the CSRF header must be refused: %d", rec.Code)
	}
	if rec := do("POST", cookie, true); rec.Code != 200 {
		t.Errorf("POST with the header: %d", rec.Code)
	}
	if rec := do("GET", &http.Cookie{Name: CookieName, Value: "forged"}, false); rec.Code != 401 {
		t.Errorf("forged cookie: %d", rec.Code)
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	s := &Service{Store: newFake(t)}
	cookie, _ := login(t, s, "ada@example.com", "correct horse battery")
	req := httptest.NewRequest("POST", "/api/ui/auth/logout", nil)
	req.AddCookie(cookie)
	if err := s.Logout(context.Background(), httptest.NewRecorder(), req); err != nil {
		t.Fatal(err)
	}
	if len(s.Store.(*fakeStore).sessions) != 0 {
		t.Fatal("session still stored after logout")
	}
}

func TestValidatePassword(t *testing.T) {
	if ValidatePassword("short") == nil {
		t.Error("short password accepted")
	}
	if ValidatePassword(strings.Repeat("a", 73)) == nil {
		t.Error("bcrypt only uses 72 bytes; longer must be refused, not silently truncated")
	}
	if ValidatePassword("long enough password") != nil {
		t.Error("good password refused")
	}
}
