package ui

import (
	"net/http"
	"strconv"
	"time"

	"juju/internal/auth"
	"juju/internal/httpx"
	"juju/internal/store"
)

func (a *API) registerAdmin(mux *http.ServeMux) {
	admin := func(h http.HandlerFunc) http.Handler { return a.Auth.Middleware(a.superAdmin(h)) }
	mux.Handle("GET /api/ui/admin/overview", admin(a.adminOverview))
	mux.Handle("GET /api/ui/admin/orgs/{org}", admin(a.adminOrg))
}

// superAdmin lets super admins through. Everyone else gets the same 404 as
// any path that doesn't exist. The flag comes from the session lookup on each
// request, so revoking it takes effect on the next request, without waiting
// for sessions to end. Each access is logged: this view crosses every
// organization's boundary, so there should be a record of who looked.
func (a *API) superAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, _ := auth.UserFrom(r.Context())
		if !u.IsSuperAdmin {
			httpx.WriteError(w, http.StatusNotFound, "no such API route")
			return
		}
		a.Log.Info("super admin access", "user_id", u.ID, "path", r.URL.Path)
		next(w, r)
	}
}

func (a *API) adminOverview(w http.ResponseWriter, r *http.Request) {
	ov, err := a.Store.AdminOverview(r.Context(), time.Now())
	if err != nil {
		a.fail(w, "admin overview", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ov)
}

// adminOrg is one organization with its projects and members, including
// their email addresses: the people to contact when one of its projects has
// a problem.
func (a *API) adminOrg(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("org"), 10, 64)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "organization not found")
		return
	}
	ov, err := a.Store.AdminOverview(r.Context(), time.Now())
	if err != nil {
		a.fail(w, "admin overview", err)
		return
	}
	for _, o := range ov.Organizations {
		if o.ID != id {
			continue
		}
		projects := []store.AdminProject{}
		for _, p := range ov.Projects {
			if p.OrgID == id {
				projects = append(projects, p)
			}
		}
		members, err := a.Store.Members(r.Context(), id)
		if err != nil {
			a.fail(w, "list members", err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"organization": o, "projects": projects, "members": members})
		return
	}
	httpx.WriteError(w, http.StatusNotFound, "organization not found")
}
