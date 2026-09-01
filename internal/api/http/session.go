package httpapi

import (
	"context"

	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// SessionPrincipal is the identity a browser or TV web-login session acts AS:
// the pinned user whose device approved the QR login (ADR-0053), and that
// approving device's signing key. A session token is anonymous liveness on its
// own; this is who the broker minted it for.
//
// It is heyarr's counterpart to All Thing's SessionPrincipal (ADR-0006). Both
// carry the pinned user and the approving device key so a route can, in future,
// scope authority to the SESSION identity rather than merely its liveness — a
// browser or television carries neither a device cert nor a grant header, but
// the session token it does carry maps back to this principal.
type SessionPrincipal struct {
	// UserID is the pinned user the approving device acts as, in rendered
	// "ed25519:<hex>" form — the principal's stable name.
	UserID string
	// DeviceKey is the approving device's signing key ("ed25519:<hex>").
	DeviceKey string
}

// SessionValidator authenticates an opaque web-login session token — the
// credential a browser or TV holds after a Voidbind QR login the broker approved
// (ADR-0053) — and resolves the principal it was minted for. It is the seam
// between this HTTP layer and the weblogin broker: the server need not know how a
// token is made or what it maps to, only who (if anyone) it currently stands
// for.
//
// It is optional, exactly like DeviceVerifier. A nil one disables the scheme,
// which is the correct state where no broker was stood up (this node names no
// external origin, or has no pinned users). A session token then falls through to
// a 401 like any other unrecognised bearer value — it is never a substitute for
// Verifier, which authenticates a service's own bearer token; the two schemes
// coexist on the one Authorization: Bearer header, tried in that order.
type SessionValidator interface {
	// Session returns the session's principal and true when token is live, or the
	// zero value and false when it is not.
	Session(token string) (SessionPrincipal, bool)
}

// ManagementAuthorizer answers whether an approving device key carries a
// follow-management grant (ADR-0061) — the interim, operator-issued
// authorization that lifts a web-login session from the read floor to write.
//
// It is optional, like SessionValidator and DeviceVerifier. A nil one means no
// device is ever management-authorized, which is the read-only-by-default state:
// every web-login session stays at the read floor, exactly as before the grant
// existed. Only when it is wired does a session for a granted approving device
// carry write — and a shared surface (a TV) whose approver was never granted
// still reads read-only, which is the whole point.
type ManagementAuthorizer interface {
	// ManagementAuthorized reports whether deviceKey is authorised for write. An
	// error is treated by the caller as "not authorised": a grant lookup that
	// cannot be answered must fail closed to the read floor, never open to write.
	ManagementAuthorized(ctx context.Context, deviceKey string) (bool, error)
}

// sessionIdentity is what a valid web-login session token authenticates as: the
// pinned user the approving device acts for. It carries the read floor by
// default — a browser or television that logged in by QR gets to browse and
// stream — and write only when the approving device holds a follow-management
// grant (authorized, ADR-0061), the interim path a trusted personal device takes
// to manage followed sources before device-cert enrolment (ADR-0048) converges
// the surface. Anything finer than write is a capability grant (internal/grant),
// not a scope.
func sessionIdentity(p SessionPrincipal, authorized bool) auth.Identity {
	scopes := []auth.Scope{auth.ScopeRead}
	if authorized {
		scopes = []auth.Scope{auth.ScopeRead, auth.ScopeWrite}
	}
	return auth.Identity{
		Principal: auth.Principal{ID: p.UserID, Kind: "user", Name: "session:" + p.DeviceKey},
		Token: auth.Token{
			Name:        "session:" + p.DeviceKey,
			PrincipalID: p.UserID,
			Scopes:      scopes,
		},
	}
}
