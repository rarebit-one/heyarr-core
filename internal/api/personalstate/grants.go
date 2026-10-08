package personalstate

// grants.go is the per-space access list on the device-facing API (ADR-0104):
// the owner's consent act that lets an executor principal fetch a space's
// ciphertext, and the gate every space route applies to a restricted caller.
//
// Two gates stay orthogonal (ADR-0049): a grant decides whether a restricted
// caller may FETCH (or push) a space's ciphertext; whether it can READ it is
// still a wrapped key it separately holds. Neither implies the other.

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

// grantRequest is POST /spaces/{id}/grants: the executor principal (id or
// name), its capabilities, and an optional expiry.
//
// WrappedKeys optionally carries copies of the space's CURRENT key wrapped for
// the executor's registered service recipients. They are recorded in the same
// transaction as the grant, so the fetch gate and the decryption gate open
// together or not at all. Each must name a live service recipient of that
// executor and seal the current key epoch.
type grantRequest struct {
	Principal   string            `json:"principal"`
	Caps        string            `json:"caps"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
	WrappedKeys []wrappedKeyInput `json:"wrapped_keys,omitempty"`
}

// grantView is a recorded grant.
type grantView struct {
	SpaceID         string `json:"space_id"`
	PrincipalID     string `json:"principal_id"`
	Caps            string `json:"caps"`
	GrantedByDevice string `json:"granted_by_device"`
	GrantedAt       string `json:"granted_at"`
	ExpiresAt       string `json:"expires_at,omitempty"`
	// WrappedFor names the service recipients the grant wrapped the key for.
	WrappedFor []string `json:"wrapped_for,omitempty"`
}

// granter is the caller of a grant or revoke: only a Device credential carrying
// write — an enrolled device an admin authorised (ADR-0065) — may consent. A
// bearer token or a web-login session is replayable and is refused, as is an
// anonymous identity on an unauthenticated loopback node.
func granter(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	id, ok := httpapi.IdentityFrom(r.Context())
	if !ok || id.DeviceKey == "" || id.Principal.ID == "" {
		httpapi.Fail(w, r, problem.Forbidden(
			"granting access to a space is done from a management-authorised device credential, not a bearer token or session (ADR-0104)"))
		return auth.Identity{}, false
	}
	return id, true
}

func (a *API) grantAccess(w http.ResponseWriter, r *http.Request) {
	id, ok := granter(w, r)
	if !ok {
		return
	}
	if a.principals == nil {
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeServiceUnavailable,
			"Service Unavailable", "this node resolves no executor principals"))
		return
	}
	var req grantRequest
	if err := httpapi.DecodeJSON(w, r, &req, maxRequestBody); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	principal, ok := a.resolveExecutor(w, r, req.Principal)
	if !ok {
		return
	}
	wraps := make([]store.GrantWrap, 0, len(req.WrappedKeys))
	var wrappedFor []string
	for _, k := range req.WrappedKeys {
		wraps = append(wraps, store.GrantWrap{Recipient: k.Recipient, Wrapped: k.Wrapped, Epoch: k.Epoch})
		wrappedFor = append(wrappedFor, k.Recipient)
	}
	g, err := a.store.GrantAccess(r.Context(), chi.URLParam(r, "id"), principal.ID, req.Caps,
		id.Principal.ID, id.DeviceKey, req.ExpiresAt, wraps...)
	if err != nil {
		a.failGrant(w, r, "granting space access", err)
		return
	}
	a.log.Info("granted space access", "space", g.SpaceID, "principal", g.PrincipalID, "caps", g.Caps,
		"wraps", len(wraps))
	out := grantView{
		SpaceID: g.SpaceID, PrincipalID: g.PrincipalID, Caps: g.Caps,
		GrantedByDevice: g.GrantedByDevice, GrantedAt: g.GrantedAt.UTC().Format(timeFormat),
		WrappedFor: wrappedFor,
	}
	if g.ExpiresAt != nil {
		out.ExpiresAt = g.ExpiresAt.UTC().Format(timeFormat)
	}
	a.write(w, r, http.StatusCreated, out)
}

func (a *API) revokeAccess(w http.ResponseWriter, r *http.Request) {
	id, ok := granter(w, r)
	if !ok {
		return
	}
	if a.principals == nil {
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeServiceUnavailable,
			"Service Unavailable", "this node resolves no executor principals"))
		return
	}
	principal, ok := a.resolveExecutor(w, r, chi.URLParam(r, "principal"))
	if !ok {
		return
	}
	spaceID := chi.URLParam(r, "id")
	dropped, err := a.store.RevokeAccess(r.Context(), spaceID, principal.ID, id.Principal.ID, id.DeviceKey)
	if err != nil {
		a.failGrant(w, r, "revoking space access", err)
		return
	}
	a.log.Info("revoked space access", "space", spaceID, "principal", principal.ID, "wraps_deleted", len(dropped))
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) resolveExecutor(w http.ResponseWriter, r *http.Request, ref string) (auth.Principal, bool) {
	if ref == "" {
		httpapi.Fail(w, r, problem.BadRequest("a grant names the executor principal it admits"))
		return auth.Principal{}, false
	}
	p, err := a.principals.ExecutorPrincipal(r.Context(), ref)
	switch {
	case errors.Is(err, auth.ErrNotFound):
		httpapi.Fail(w, r, problem.NotFound("no such principal"))
		return auth.Principal{}, false
	case errors.Is(err, auth.ErrPrincipalKind):
		httpapi.Fail(w, r, problem.BadRequest(
			"only an executor principal is granted space access; a user or service principal is not confined by a grant"))
		return auth.Principal{}, false
	case err != nil:
		a.log.Error("resolving a principal", "request_id", httpapi.RequestIDFrom(r.Context()), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return auth.Principal{}, false
	}
	return p, true
}

func (a *API) failGrant(w http.ResponseWriter, r *http.Request, doing string, err error) {
	switch {
	case errors.Is(err, store.ErrNotSpaceOwner):
		httpapi.Fail(w, r, problem.Forbidden(err.Error()))
	case errors.Is(err, store.ErrNoGrant):
		httpapi.Fail(w, r, problem.NotFound(err.Error()))
	case errors.Is(err, store.ErrInvalidCaps), errors.Is(err, store.ErrInvalidExpiry):
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
	case errors.Is(err, store.ErrNotServiceRecipient):
		httpapi.Fail(w, r, problem.Forbidden(err.Error()+
			" — register the executor's key first (`heyarr recipient add`); a space key is wrapped only for a registered, granted recipient (ADR-0049, ADR-0104)").
			WithCode(CodeWrapRecipientNotAllowed))
	default:
		a.failStore(w, r, doing, err)
	}
}

// spaceOwner is the principal a newly created space is recorded as owned by:
// the user behind a Device credential. Any other caller records no owner — a
// legacy household space, exactly as every space was before ADR-0104.
func spaceOwner(r *http.Request) string {
	id, ok := httpapi.IdentityFrom(r.Context())
	if !ok || id.DeviceKey == "" || id.Principal.Kind != auth.KindUser {
		return ""
	}
	return id.Principal.ID
}

// restrictedCaller returns the identity when the caller is restricted.
func restrictedCaller(r *http.Request) (auth.Identity, bool) {
	id, ok := httpapi.IdentityFrom(r.Context())
	if !ok || !id.Restricted {
		return auth.Identity{}, false
	}
	return id, true
}

// spaceAccess applies the grant to a restricted caller: without an active grant
// the space answers 404, exactly as an unknown one does, so a restricted caller
// cannot learn which spaces exist; a push additionally needs a write grant, or
// 403. Every other caller passes, as before. It writes the failure and returns
// false when the request must stop.
func (a *API) spaceAccess(w http.ResponseWriter, r *http.Request, spaceID string, write bool) bool {
	id, restricted := restrictedCaller(r)
	if !restricted {
		return true
	}
	g, ok, err := a.store.ActiveGrant(r.Context(), spaceID, id.Principal.ID)
	if err != nil {
		a.failStore(w, r, "reading a grant", err)
		return false
	}
	if !ok {
		httpapi.Fail(w, r, problem.NotFound(store.ErrUnknownSpace.Error()))
		return false
	}
	if write && !g.Writes() {
		httpapi.Fail(w, r, problem.Forbidden("this credential's grant on the space is read-only (ADR-0104)").
			WithCode(httpapi.CodeRestrictedPrincipal))
		return false
	}
	return true
}

// grantedSpaces is the restricted caller's granted space set; restricted is
// false (and the set nil) for every other caller.
func (a *API) grantedSpaces(r *http.Request) (map[string]bool, bool, error) {
	id, restricted := restrictedCaller(r)
	if !restricted {
		return nil, false, nil
	}
	set, err := a.store.GrantedSpaceIDs(r.Context(), id.Principal.ID)
	return set, true, err
}

// ownRecipients is the restricted caller's own wrap recipients; restricted is
// false for every other caller, who sees every wrap as before.
func (a *API) ownRecipients(r *http.Request) (map[string]bool, bool, error) {
	id, restricted := restrictedCaller(r)
	if !restricted {
		return nil, false, nil
	}
	if a.recipients == nil {
		return map[string]bool{}, true, nil
	}
	set, err := a.recipients.RecipientsOf(r.Context(), id.Principal.ID)
	return set, true, err
}

// compile-time: the store is what the vault routes ask about write grants.
var _ httpapi.SpaceWriteGrants = (*store.Store)(nil)

// compile-time: the store names a restricted caller's own wrap recipients.
var _ PrincipalRecipients = (*store.Store)(nil)

// compile-time: the auth store resolves grant principals.
var _ PrincipalResolver = (*auth.Store)(nil)
