// Package auth handles dashboard logins: password hashing, session cookies,
// CSRF protection and login rate limiting.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"juju/internal/httpx"
	"juju/internal/store"
)

const (
	CookieName = "juju_session"
	// Sessions last 30 days and are extended while in use, so people who
	// open the dashboard daily never get logged out mid-week.
	sessionTTL    = 30 * 24 * time.Hour
	extendWhenAge = 24 * time.Hour
	// CSRFHeader must be sent with every request that changes something.
	// A malicious site can't add custom headers to a cross-site request
	// without a CORS preflight, and the dashboard API grants none.
	CSRFHeader = "X-Juju-CSRF"
)

var ErrBadLogin = errors.New("wrong email or password")

// bcryptCost makes each password check take about a quarter second: slow
// enough that guessing millions of passwords from a stolen database is
// impractical, fast enough that a login doesn't feel slow. Tests lower it.
var bcryptCost = 12

// A bcrypt hash of a random password, compared against when the email
// doesn't exist. Doing the same slow work either way means response time
// can't reveal which emails have accounts.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not-a-real-password-"+time.Now().String()), bcryptCost)

func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	return string(b), err
}

// ValidatePassword enforces a minimum, with a message people can act on.
func ValidatePassword(pw string) error {
	if len(pw) < 10 {
		return errors.New("use at least 10 characters for your password")
	}
	if len(pw) > 72 {
		return errors.New("use at most 72 characters for your password") // bcrypt's limit
	}
	return nil
}

// Store is the slice of the database auth needs.
type Store interface {
	UserByEmail(ctx context.Context, email string) (store.User, string, error)
	CreateSession(ctx context.Context, tokenHash []byte, userID int64, expires time.Time) error
	SessionUser(ctx context.Context, tokenHash []byte) (store.User, time.Time, error)
	ExtendSession(ctx context.Context, tokenHash []byte, expires time.Time) error
	DeleteSession(ctx context.Context, tokenHash []byte) error
}

type Service struct {
	Store Store
	// SecureCookies sets the cookie's Secure flag (https only). On for any
	// https deployment; off only for http://localhost development.
	SecureCookies bool
	limiter       limiter
}

// Login checks the password and starts a session, writing the cookie.
func (s *Service) Login(ctx context.Context, w http.ResponseWriter, email, password, clientIP string) (store.User, error) {
	email = NormalizeEmail(email)
	if !s.limiter.allow(clientIP + "|" + email) {
		return store.User{}, ErrTooManyAttempts
	}
	u, hash, err := s.Store.UserByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return store.User{}, ErrBadLogin
	}
	if err != nil {
		return store.User{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return store.User{}, ErrBadLogin
	}
	s.limiter.reset(clientIP + "|" + email)
	return u, s.StartSession(ctx, w, u.ID)
}

// StartSession creates a session for a user and sets its cookie.
func (s *Service) StartSession(ctx context.Context, w http.ResponseWriter, userID int64) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().Add(sessionTTL)
	if err := s.Store.CreateSession(ctx, hash(token), userID, expires); err != nil {
		return err
	}
	s.setCookie(w, token, expires)
	return nil
}

// Logout ends the session in the request and clears the cookie.
func (s *Service) Logout(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	if c, err := r.Cookie(CookieName); err == nil {
		if err := s.Store.DeleteSession(ctx, hash(c.Value)); err != nil {
			return err
		}
	}
	s.setCookie(w, "", time.Unix(0, 0))
	return nil
}

type ctxKey struct{}

// UserFrom returns the logged-in user set by Middleware.
func UserFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(ctxKey{}).(store.User)
	return u, ok
}

// Middleware rejects requests without a valid session (401) and, for
// anything that changes data, without the CSRF header (403).
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(CSRFHeader) == "" {
			writeError(w, http.StatusForbidden, "missing "+CSRFHeader+" header")
			return
		}
		c, err := r.Cookie(CookieName)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "log in to continue")
			return
		}
		u, expires, err := s.Store.SessionUser(r.Context(), hash(c.Value))
		if errors.Is(err, store.ErrNotFound) {
			s.setCookie(w, "", time.Unix(0, 0))
			writeError(w, http.StatusUnauthorized, "your session has ended, log in again")
			return
		}
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "temporarily unavailable")
			return
		}
		// Slide the expiry forward at most once a day, not on every request.
		if time.Until(expires) < sessionTTL-extendWhenAge {
			newExpiry := time.Now().Add(sessionTTL)
			if s.Store.ExtendSession(r.Context(), hash(c.Value), newExpiry) == nil {
				s.setCookie(w, c.Value, newExpiry)
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
	})
}

// RequireCSRF is the CSRF check alone, for the login and sign-up forms,
// which run before there's a session.
func RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(CSRFHeader) == "" {
			writeError(w, http.StatusForbidden, "missing "+CSRFHeader+" header")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Service) setCookie(w http.ResponseWriter, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true, // page scripts can't read it, so an XSS bug can't steal it
		Secure:   s.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

func NormalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// NewToken returns 32 random bytes as URL-safe text: unguessable, so it can
// stand in for a password in a link.
func NewToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// HashToken is what's stored for a token: its SHA-256.
func HashToken(token string) []byte { return hash(token) }

func hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func writeError(w http.ResponseWriter, status int, msg string) {
	httpx.WriteError(w, status, msg)
}

var ErrTooManyAttempts = errors.New("too many login attempts, wait a few minutes and try again")

// limiter allows 10 failed logins per key in 15 minutes. In memory: fine
// for one server, and forgetting on restart only helps a legitimate user.
type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

const (
	maxAttempts = 10
	window      = 15 * time.Minute
)

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	now := time.Now()
	recent := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < window {
			recent = append(recent, t)
		}
	}
	if len(recent) >= maxAttempts {
		l.hits[key] = recent
		return false
	}
	l.hits[key] = append(recent, now)
	return true
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}
