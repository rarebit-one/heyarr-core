package resources

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// The session surface (ADR-0061, M12) — how a browser/TV client discovers its
// own authority, and how an operator lifts a trusted personal device from the
// read floor to write.
//
// # The interim write-authorization path, in one place
//
// A web-login session (ADR-0053) is minted read-only, so POST/DELETE
// /followed-sources 403 from a browser or TV. Device-cert enrolment (ADR-0048)
// is the eventual convergence that gives the surface its own authenticated
// identity; until the voidbind-client artifact lands, a follow-management grant
// is the interim, non-gated path: an operator authorises a specific approving
// device key, and every web-login session that device approves then carries
// `write`. The default — no grant — is read-only, so a shared surface stays
// read-only until its approver is explicitly authorised.
//
// A client drives it with two reads and one operator action:
//  1. GET /session — learn this session's scope and its approving device_key.
//  2. If can_write is false, show the device_key and prompt the operator to
//     authorise this device.
//  3. The operator issues POST /session/management-grants {device_key} (admin).
//  4. GET /session again — can_write is now true; follow/unfollow succeed.

// SessionView is a caller's own authority, as GET /session reports it. It is the
// one endpoint a client reads to decide whether to show management UI, and — when
// it may not yet — which device key an operator must authorise.
type SessionView struct {
	// Kind is how the caller authenticated: "session" (web-login/QR, ADR-0053),
	// "device" (device cert, ADR-0048), "service" (a bearer token), or
	// "anonymous" (authentication disabled on a loopback listener).
	Kind string `json:"kind"`
	// PrincipalID is the identity the caller acts as ("ed25519:<hex>" for a user),
	// or empty for the anonymous identity.
	PrincipalID string `json:"principal_id,omitempty"`
	// DeviceKey is the approving/authenticated device key for a session or device
	// caller — the value an operator names to grant management. Empty otherwise.
	DeviceKey string `json:"device_key,omitempty"`
	// Scopes is the effective authority this credential carries.
	Scopes []string `json:"scopes"`
	// CanWrite is the convenience a client wires on: whether Scopes admits write,
	// i.e. whether follow/unfollow will succeed for this caller as it stands.
	CanWrite bool `json:"can_write"`
	// ManagementAuthorized is true when this is a session whose approving device
	// holds a follow-management grant (ADR-0061) — the reason a session, unusually,
	// can write. It is false for a read-only session, and for a service token
	// (whose write authority, when it has it, is its own scope, not a grant).
	ManagementAuthorized bool `json:"management_authorized"`
}

// ManagementGrantRequest issues a follow-management grant (ADR-0061) — the
// operator's explicit consent that a specific approving device may manage
// followed sources from a web-login session.
type ManagementGrantRequest struct {
	// DeviceKey is the approving device key to authorise, as GET /session reports
	// it for that device ("ed25519:<hex>").
	DeviceKey string `json:"device_key"`
	// Reason is a free-text operator note ("the operator's phone"). Optional.
	Reason string `json:"reason"`
}

// handleSession is GET /api/v1/session — a caller's own authority. It reads the
// identity the middleware already resolved, so it needs only the read floor the
// router requires.
func (a *API) handleSession(w http.ResponseWriter, r *http.Request) {
	id, ok := httpapi.IdentityFrom(r.Context())
	if !ok {
		// The router's read floor means this endpoint is never reached without an
		// identity; this is the belt-and-braces answer, not an expected path.
		httpapi.Fail(w, r, problem.Unauthorized("this endpoint requires a credential"))
		return
	}

	kind, deviceKey := classifyIdentity(id)
	view := SessionView{
		Kind:                 kind,
		PrincipalID:          id.Principal.ID,
		DeviceKey:            deviceKey,
		Scopes:               scopeStrings(id.Token.Scopes),
		CanWrite:             id.Allows(auth.ScopeWrite),
		ManagementAuthorized: kind == "session" && id.Allows(auth.ScopeWrite),
	}
	a.write(w, r, http.StatusOK, view)
}

// classifyIdentity derives the caller kind and its device key from the resolved
// identity. The synthetic Token.Name the auth layer stamps ("session:<key>",
// "device:<key>") is the seam: it is set exactly where the credential kind is
// known, so reading it back here needs no second lookup.
func classifyIdentity(id auth.Identity) (kind, deviceKey string) {
	switch {
	case id.Anonymous:
		return "anonymous", ""
	case strings.HasPrefix(id.Token.Name, "session:"):
		return "session", strings.TrimPrefix(id.Token.Name, "session:")
	case strings.HasPrefix(id.Token.Name, "device:"):
		return "device", strings.TrimPrefix(id.Token.Name, "device:")
	default:
		return "service", ""
	}
}

func scopeStrings(scopes []auth.Scope) []string {
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		out = append(out, string(s))
	}
	return out
}

// listManagementGrants is GET /api/v1/session/management-grants (admin).
func (a *API) listManagementGrants(w http.ResponseWriter, r *http.Request) {
	grants, err := a.catalog.ListManagementGrants(r.Context())
	if err != nil {
		a.fail(w, r, "management grant", err)
		return
	}
	a.write(w, r, http.StatusOK, map[string]any{"management_grants": grants})
}

// createManagementGrant is POST /api/v1/session/management-grants (admin) — the
// operator's explicit consent step. Idempotent: re-granting an authorised device
// updates its note rather than failing.
func (a *API) createManagementGrant(w http.ResponseWriter, r *http.Request) {
	var body ManagementGrantRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	deviceKey := strings.TrimSpace(body.DeviceKey)
	if deviceKey == "" {
		httpapi.Fail(w, r, problem.BadRequest(
			"a management grant needs a device_key — the approving device to authorise, "+
				"as GET /session reports it for that device"))
		return
	}
	grant, err := a.catalog.GrantManagement(r.Context(), deviceKey, strings.TrimSpace(body.Reason))
	if err != nil {
		a.fail(w, r, "management grant", err)
		return
	}
	w.Header().Set("Location", httpapi.APIPrefix+"/session/management-grants/"+grant.DeviceKey)
	a.write(w, r, http.StatusCreated, grant)
}

// deleteManagementGrant is DELETE /api/v1/session/management-grants/{device_key}
// (admin) — revoke a device's write authorization. A revoke of a device that was
// never granted is a 404, not a silent success.
func (a *API) deleteManagementGrant(w http.ResponseWriter, r *http.Request) {
	deviceKey := strings.TrimSpace(chi.URLParam(r, "device_key"))
	existed, err := a.catalog.RevokeManagement(r.Context(), deviceKey)
	if err != nil {
		a.fail(w, r, "management grant", err)
		return
	}
	if !existed {
		httpapi.Fail(w, r, problem.NotFound("no management grant exists for that device key"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mountSession registers the session surface. Introspection is a read (the
// router's floor); issuing and revoking a management grant elevates a device's
// authority, so — like token and peer management — they need `admin`, not `write`.
func (a *API) mountSession(r chi.Router) {
	r.Get("/session", a.handleSession)
	if a.catalog != nil {
		r.With(httpapi.RequireScope(auth.ScopeAdmin)).Get("/session/management-grants", a.listManagementGrants)
		r.With(httpapi.RequireScope(auth.ScopeAdmin)).Post("/session/management-grants", a.createManagementGrant)
		r.With(httpapi.RequireScope(auth.ScopeAdmin)).Delete("/session/management-grants/{device_key}", a.deleteManagementGrant)
	}
}

// compile-time assertion the catalog satisfies the authorizer seam the auth
// layer consults (ADR-0061). It keeps this interim path's two halves — the
// grants written here and the scope resolved in internal/api/http — from
// drifting apart silently: the same *catalog.Catalog is handed to
// httpapi.Options.ManagementAuthorizer in the controller.
var _ httpapi.ManagementAuthorizer = (*catalog.Catalog)(nil)
