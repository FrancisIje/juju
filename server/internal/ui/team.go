package ui

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"juju/internal/auth"
	"juju/internal/httpx"
	"juju/internal/store"
)

// inviteTTL: long enough for someone on holiday to get to it, short enough
// that a link forgotten in an old chat stops working.
const inviteTTL = 7 * 24 * time.Hour

func (a *API) registerTeam(mux *http.ServeMux) {
	in := func(h http.HandlerFunc) http.Handler { return a.Auth.Middleware(h) }
	mux.Handle("GET /api/ui/orgs/{org}/members", in(a.inOrg(false, a.members)))
	mux.Handle("POST /api/ui/orgs/{org}/invites", in(a.inOrg(true, a.createInvite)))
	mux.Handle("DELETE /api/ui/orgs/{org}/invites/{invite}", in(a.inOrg(true, a.revokeInvite)))
	mux.Handle("DELETE /api/ui/orgs/{org}/members/{user}", in(a.inOrg(true, a.removeMember)))
	// Opening an invite link works logged out: it shows who invited you.
	mux.HandleFunc("GET /api/ui/invites/{token}", a.invite)
	mux.Handle("POST /api/ui/invites/{token}/accept", in(a.acceptInvite))
}

type orgHandler func(w http.ResponseWriter, r *http.Request, orgID int64, u store.User, role string)

// inOrg checks the user belongs to the organization, and is an owner when
// ownerOnly. Like projects, an organization you're not in is "not found".
func (a *API) inOrg(ownerOnly bool, next orgHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, _ := auth.UserFrom(r.Context())
		orgID, err := strconv.ParseInt(r.PathValue("org"), 10, 64)
		if err != nil {
			httpx.WriteError(w, http.StatusNotFound, "organization not found")
			return
		}
		role, err := a.Store.OrgRole(r.Context(), u.ID, orgID)
		if errors.Is(err, store.ErrNotFound) {
			httpx.WriteError(w, http.StatusNotFound, "organization not found")
			return
		}
		if err != nil {
			a.fail(w, "check membership", err)
			return
		}
		if ownerOnly && role != "owner" {
			httpx.WriteError(w, http.StatusForbidden, "only owners can invite or remove people; ask an owner of this organization")
			return
		}
		next(w, r, orgID, u, role)
	}
}

func (a *API) members(w http.ResponseWriter, r *http.Request, orgID int64, u store.User, role string) {
	members, err := a.Store.Members(r.Context(), orgID)
	if err != nil {
		a.fail(w, "list members", err)
		return
	}
	resp := map[string]any{"members": members, "your_role": role, "you": u.ID}
	if role == "owner" {
		invites, err := a.Store.PendingInvites(r.Context(), orgID)
		if err != nil {
			a.fail(w, "list invites", err)
			return
		}
		resp["invites"] = invites
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (a *API) createInvite(w http.ResponseWriter, r *http.Request, orgID int64, u store.User, _ string) {
	token, err := auth.NewToken()
	if err != nil {
		a.fail(w, "make invite token", err)
		return
	}
	inv, err := a.Store.CreateInvite(r.Context(), orgID, u.ID, auth.HashToken(token), time.Now().Add(inviteTTL))
	if err != nil {
		a.fail(w, "create invite", err)
		return
	}
	// The link holds the only copy of the token: shown now, never again.
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"invite": inv, "url": a.PublicURL + "/invite/" + token})
}

func (a *API) revokeInvite(w http.ResponseWriter, r *http.Request, orgID int64, _ store.User, _ string) {
	id, err := strconv.ParseInt(r.PathValue("invite"), 10, 64)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "invite not found")
		return
	}
	ok, err := a.Store.RevokeInvite(r.Context(), orgID, id)
	if err != nil {
		a.fail(w, "revoke invite", err)
		return
	}
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "invite not found; it may have been used already")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) removeMember(w http.ResponseWriter, r *http.Request, orgID int64, _ store.User, _ string) {
	id, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "member not found")
		return
	}
	switch err := a.Store.RemoveMember(r.Context(), orgID, id); {
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "member not found")
	case errors.Is(err, store.ErrLastOwner):
		httpx.WriteError(w, http.StatusConflict, "an organization needs at least one owner; make someone else an owner first")
	case err != nil:
		a.fail(w, "remove member", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

const inviteGone = "this invite link has expired or was already used; ask for a new one"

func (a *API) invite(w http.ResponseWriter, r *http.Request) {
	info, err := a.Store.InviteByToken(r.Context(), auth.HashToken(r.PathValue("token")))
	if errors.Is(err, store.ErrInviteInvalid) {
		httpx.WriteError(w, http.StatusNotFound, inviteGone)
		return
	}
	if err != nil {
		a.fail(w, "load invite", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, info)
}

func (a *API) acceptInvite(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	orgID, already, err := a.Store.AcceptInvite(r.Context(), auth.HashToken(r.PathValue("token")), u.ID)
	if errors.Is(err, store.ErrInviteInvalid) {
		httpx.WriteError(w, http.StatusNotFound, inviteGone)
		return
	}
	if err != nil {
		a.fail(w, "accept invite", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"organization_id": orgID, "already_member": already})
}
