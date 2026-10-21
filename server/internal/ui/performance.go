package ui

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"juju/internal/httpx"
	"juju/internal/store"
)

// Release health and performance pages (Phase 8).

func (a *API) registerPerformance(mux *http.ServeMux) {
	p := func(h projectHandler) http.Handler { return a.Auth.Middleware(a.member(h)) }
	mux.Handle("GET /api/ui/projects/{project}/releases", p(a.releases))
	mux.Handle("GET /api/ui/projects/{project}/transactions", p(a.transactions))
	// The name goes in the query string, not the path: transaction names
	// are often URL paths themselves ("/cart/:id").
	mux.Handle("GET /api/ui/projects/{project}/transactions/summary", p(a.transactionDetail))
	mux.Handle("GET /api/ui/projects/{project}/transactions/events/{event}", p(a.transactionEvent))
}

// periods the pages offer. Short windows chart by hour, longer ones by day,
// so a chart always has between 7 and 30 or so points.
var periods = map[string]struct {
	d      time.Duration
	bucket string
}{
	"24h": {24 * time.Hour, "hour"},
	"7d":  {7 * 24 * time.Hour, "day"},
	"14d": {14 * 24 * time.Hour, "day"},
	"30d": {30 * 24 * time.Hour, "day"},
}

// window reads ?period= and ?environment=. It answers 400 itself and
// returns false for an unknown period.
func window(w http.ResponseWriter, r *http.Request) (store.Window, bool) {
	q := r.URL.Query()
	name := q.Get("period")
	if name == "" {
		name = "7d"
	}
	p, ok := periods[name]
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, "period must be 24h, 7d, 14d or 30d")
		return store.Window{}, false
	}
	return store.Window{Since: time.Now().Add(-p.d), Bucket: p.bucket, Environment: q.Get("environment")}, true
}

func (a *API) releases(w http.ResponseWriter, r *http.Request, pid int64, _ store.User) {
	win, ok := window(w, r)
	if !ok {
		return
	}
	h, err := a.Store.ReleaseHealth(r.Context(), pid, win)
	if err != nil {
		a.fail(w, "release health", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h)
}

func (a *API) transactions(w http.ResponseWriter, r *http.Request, pid int64, _ store.User) {
	win, ok := window(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	list, err := a.Store.Transactions(r.Context(), pid, store.TransactionFilter{Window: win, Op: q.Get("op"), Sort: q.Get("sort")})
	if err != nil {
		a.fail(w, "list transactions", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}

func (a *API) transactionDetail(w http.ResponseWriter, r *http.Request, pid int64, _ store.User) {
	win, ok := window(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if q.Get("name") == "" {
		httpx.WriteError(w, http.StatusBadRequest, "name is required")
		return
	}
	d, err := a.Store.TransactionDetail(r.Context(), pid, q.Get("name"), q.Get("op"), win, q.Get("samples"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, "no transactions with this name in this period")
		return
	}
	if err != nil {
		a.fail(w, "transaction detail", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, d)
}

func (a *API) transactionEvent(w http.ResponseWriter, r *http.Request, pid int64, _ store.User) {
	// Check the id's shape here: Postgres would reject a non-UUID with an
	// error, and that must be a 404, not a 500.
	id, err := uuid.Parse(r.PathValue("event"))
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "transaction not found")
		return
	}
	payload, err := a.Store.TransactionEvent(r.Context(), pid, id.String())
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, "transaction not found")
		return
	}
	if err != nil {
		a.fail(w, "load transaction", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, payload)
}
