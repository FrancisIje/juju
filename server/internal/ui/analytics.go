package ui

import (
	"net/http"
	"strings"
	"time"

	"juju/internal/analytics"
	"juju/internal/httpx"
	"juju/internal/store"
)

func (a *API) registerAnalytics(mux *http.ServeMux) {
	p := func(h projectHandler) http.Handler { return a.Auth.Middleware(a.member(h)) }
	mux.Handle("GET /api/ui/projects/{project}/analytics", p(a.analyticsSummary))
	mux.Handle("GET /api/ui/projects/{project}/analytics/properties", p(a.analyticsProperties))
	mux.Handle("GET /api/ui/projects/{project}/analytics/trend", p(a.analyticsTrend))
	mux.Handle("GET /api/ui/projects/{project}/analytics/funnel", p(a.analyticsFunnel))
	mux.Handle("GET /api/ui/projects/{project}/analytics/retention", p(a.analyticsRetention))
}

const analyticsOff = "product analytics is off on this server: it needs ClickHouse (set JUJU_CLICKHOUSE_URL)"

// lastDays is the range "the last n days", ending now.
func lastDays(r *http.Request, def, maxDays int) analytics.Range {
	n := clamp(atoi(r.URL.Query().Get("days"), def), 1, maxDays)
	now := time.Now().UTC()
	return analytics.Range{From: now.AddDate(0, 0, -n), To: now.Add(time.Minute)}
}

// analyticsSummary says whether analytics is on and which events exist:
// what the dashboard needs to show either the charts or how to start.
func (a *API) analyticsSummary(w http.ResponseWriter, r *http.Request, pid int64, _ store.User) {
	if a.Analytics == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"enabled": false, "reason": analyticsOff, "events": []any{}})
		return
	}
	names, err := a.Analytics.EventNames(r.Context(), pid, lastDays(r, 30, 365))
	if err != nil {
		a.analyticsFail(w, err)
		return
	}
	if names == nil {
		names = []analytics.EventName{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"enabled": true, "events": names})
}

func (a *API) analyticsProperties(w http.ResponseWriter, r *http.Request, pid int64, _ store.User) {
	if !a.analyticsOn(w) {
		return
	}
	keys, err := a.Analytics.PropertyKeys(r.Context(), pid, r.URL.Query().Get("event"), lastDays(r, 30, 365))
	if err != nil {
		a.analyticsFail(w, err)
		return
	}
	if keys == nil {
		keys = []string{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"properties": keys})
}

func (a *API) analyticsTrend(w http.ResponseWriter, r *http.Request, pid int64, _ store.User) {
	if !a.analyticsOn(w) {
		return
	}
	q := r.URL.Query()
	rng := lastDays(r, 7, 365)
	iv := analytics.Interval(q.Get("interval"))
	if iv == "" {
		iv = analytics.Day
	}
	// Hourly over a year would be 8760 points: more than a chart can show.
	if iv == analytics.Hour && rng.To.Sub(rng.From) > 15*24*time.Hour {
		httpx.WriteError(w, http.StatusBadRequest, "hourly trends cover at most 14 days; choose daily or weekly")
		return
	}
	res, err := a.Analytics.Trend(r.Context(), pid, analytics.TrendQuery{
		Event: q.Get("event"), Range: rng, Interval: iv, People: q.Get("math") == "people", Breakdown: q.Get("breakdown"),
	})
	if err != nil {
		a.analyticsFail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (a *API) analyticsFunnel(w http.ResponseWriter, r *http.Request, pid int64, _ store.User) {
	if !a.analyticsOn(w) {
		return
	}
	q := r.URL.Query()
	var steps []string
	// One parameter per step: an event name can contain a comma.
	for _, s := range q["step"] {
		if s = strings.TrimSpace(s); s != "" {
			steps = append(steps, s)
		}
	}
	hours := clamp(atoi(q.Get("window_hours"), 24), 1, 24*30)
	res, err := a.Analytics.Funnel(r.Context(), pid, analytics.FunnelQuery{Steps: steps, Range: lastDays(r, 30, 365), Window: time.Duration(hours) * time.Hour})
	if err != nil {
		a.analyticsFail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"steps": res, "window_hours": hours})
}

func (a *API) analyticsRetention(w http.ResponseWriter, r *http.Request, pid int64, _ store.User) {
	if !a.analyticsOn(w) {
		return
	}
	q := r.URL.Query()
	days := clamp(atoi(q.Get("days"), 14), 1, 90)
	periods := clamp(atoi(q.Get("periods"), 7), 1, 31)
	// Cohorts are whole days, ending with today.
	today := time.Now().UTC().Truncate(24 * time.Hour)
	rng := analytics.Range{From: today.AddDate(0, 0, -(days - 1)), To: today.AddDate(0, 0, 1)}
	res, err := a.Analytics.Retention(r.Context(), pid, analytics.RetentionQuery{Start: q.Get("start"), Return: q.Get("return"), Range: rng, Periods: periods})
	if err != nil {
		a.analyticsFail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"cohorts": res, "periods": periods})
}

func (a *API) analyticsOn(w http.ResponseWriter) bool {
	if a.Analytics == nil {
		httpx.WriteError(w, http.StatusConflict, analyticsOff)
		return false
	}
	return true
}

func (a *API) analyticsFail(w http.ResponseWriter, err error) {
	if analytics.IsBadQuery(err) {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.Log.Error("analytics query", "err", err)
	httpx.WriteError(w, http.StatusBadGateway, "couldn't reach ClickHouse for analytics; check that it's running")
}
