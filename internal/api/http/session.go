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

// DeviceMembership reports whether a device key is STILL a member — enrolled
// here and not revoked. It is the revocation check the web-login path was
// missing (#420, ADR-0053).
//
// A session pins the device key that approved it, and that pin was only ever
// carried, never re-checked: a session minted before its approving device was
// revoked went on authenticating until it expired. Post-ADR-0065 that exposure
// is read-scope for the session's remaining life, which is small and is not
// nothing — revoking a device is the one action an operator takes when they
// believe it is in someone else's hands, and it should not leave a live
// credential behind it.
//
// It is a separate, optional interface rather than a method on
// SessionValidator because the broker mints sessions and the identity store
// owns membership; asking the broker would make it re-derive an answer another
// component already holds. deviceauth.Store satisfies it.
type DeviceMembership interface {
	// DeviceActive returns nil when deviceKey is enrolled and unrevoked, and an
	// error otherwise — including "no such device", which a removed pin
	// produces.
	DeviceActive(ctx context.Context, deviceKey string) error
}

// ManagementAuthorizer answers whether a device key is authorised for write —
// the admin-issued, durable authorization that lifts an enrolled device from the
// read floor to write (ADR-0065, subsuming ADR-0061's interim grant).
//
// It authorises a DEVICE, and it is the device credential path that consults it
// (authenticateDevice), not the session path: a web-login session never lifts
// (see sessionIdentity). The authorization is inherently gated on enrolment
// because only an enrolled device can present the credential this lifts — an
// authorization for a device key that is not enrolled is inert, never a way in.
//
// It is optional, like SessionValidator and DeviceVerifier. A nil one means no
// device is ever authorised for write, the read-only-by-default state: every
// device authenticates at the read floor, exactly as before the grant existed.
// Break-glass write does not depend on it at all — an admin-scoped bearer token
// (ADR-0011) always carries write and is how the first device is authorised, so
// removing the session-lift can never lock an operator out.
type ManagementAuthorizer interface {
	// ManagementAuthorized reports whether deviceKey is authorised for write. An
	// error is treated by the caller as "not authorised": a lookup that cannot be
	// answered must fail closed to the read floor, never open to write.
	ManagementAuthorized(ctx context.Context, deviceKey string) (bool, error)
}

// sessionIdentity is what a valid web-login session token authenticates as: the
// pinned user the approving device acts for, at the READ floor — a browser or
// television that logged in by QR gets to browse and stream, and nothing more.
//
// A session token is a replayable bearer credential (ADR-0053), so it never
// carries write: that is the whole of ADR-0065's subsume. Write is earned by a
// DEVICE presenting its per-request, non-replayable device credential (ADR-0048)
// — the strong path that superseded the interim session-lift (ADR-0061), rather
// than coexisting beside it and capping the floor at the weaker credential. A
// browser or TV that needs to manage sources does so from an authorised device,
// not by lifting its own session. Anything finer than write is a capability
// grant (internal/grant), not a scope.
func sessionIdentity(p SessionPrincipal) auth.Identity {
	return auth.Identity{
		Principal: auth.Principal{ID: p.UserID, Kind: "user", Name: "session:" + p.DeviceKey},
		Token: auth.Token{
			Name:        "session:" + p.DeviceKey,
			PrincipalID: p.UserID,
			Scopes:      []auth.Scope{auth.ScopeRead},
		},
	}
}
