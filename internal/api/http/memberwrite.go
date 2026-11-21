package httpapi

// memberwrite.go lifts an enrolled device to write on the spaces it is a
// member of (ADR-0128). A Device credential authenticates at the read floor
// (ADR-0067); on the few routes that opt in, a device whose user is a member
// of the space, and which holds a copy of the space's current key, may write
// anyway. The lift is per request and per space: it is never carried on the
// identity's scopes, so nothing outside those routes sees a wider device.

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// CodeSpaceNotMember is the stable code on the refusal a read-floor device
// gets on a member-writable route when it is not a writing member of the
// space (or, on the vault routes, of any space).
const CodeSpaceNotMember = "space_not_member"

// MemberWrites answers whether a device may write to a space as one of its
// members (ADR-0128): its user is admitted by the space's membership and the
// device holds a copy of the space's current key. spaceID "" asks about any
// space — the vault routes' question, since a blob and a pin carry no space
// (ADR-0096). An unknown space is false, not an error.
type MemberWrites interface {
	MemberMayWrite(ctx context.Context, principalID, deviceKey, spaceID string) (bool, error)
}

type memberLiftKey struct{}

// MemberLifted reports whether the request was let through by membership
// rather than by the write scope. A handler holds such a caller to the
// narrower rules ADR-0128 sets: a change push must name its key epoch, and an
// upload pin is confirmed only by the user who uploaded it.
func MemberLifted(ctx context.Context) bool {
	v, _ := ctx.Value(memberLiftKey{}).(bool)
	return v
}

// ContextWithMemberLift marks a context as member-lifted, for a test that
// calls a handler without the middleware in front of it.
func ContextWithMemberLift(ctx context.Context) context.Context {
	return context.WithValue(ctx, memberLiftKey{}, true)
}

// RequireWriteOrMember guards a route a member's device may write to. A caller
// whose identity carries write passes untouched — a bearer token, an
// ADR-0065-authorised or lease-lifted device, a restricted executor (whose own
// grant gate follows). Otherwise only a Device credential of a user principal,
// neither restricted nor a guest, is asked about: m decides on the space named
// by the spaceParam route parameter ("" for any space). A nil m, a lookup
// error, or a no all refuse — the lift fails closed, and a guest or a read
// token gets exactly the refusal RequireScope(write) gives it.
func RequireWriteOrMember(m MemberWrites, spaceParam string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := IdentityFrom(r.Context())
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer realm="heyarr"`)
				Fail(w, r, problem.Unauthorized("this endpoint requires a credential"))
				return
			}
			if id.Allows(auth.ScopeWrite) {
				next.ServeHTTP(w, r)
				return
			}
			if id.Guest {
				Fail(w, r, guestCapabilityRefusal(auth.ScopeWrite))
				return
			}
			if m == nil || id.DeviceKey == "" || id.Restricted || id.Anonymous ||
				id.Principal.Kind != auth.KindUser || id.Principal.ID == "" {
				Fail(w, r, problem.Forbidden("this token does not carry the "+string(auth.ScopeWrite)+" scope"))
				return
			}
			spaceID := ""
			if spaceParam != "" {
				if spaceID = chi.URLParam(r, spaceParam); spaceID == "" {
					Fail(w, r, problem.Forbidden("this token does not carry the "+string(auth.ScopeWrite)+" scope"))
					return
				}
			}
			may, err := m.MemberMayWrite(r.Context(), id.Principal.ID, id.DeviceKey, spaceID)
			if err != nil {
				Fail(w, r, problem.Internal())
				return
			}
			if !may {
				Fail(w, r, memberRefusal(spaceID))
				return
			}
			next.ServeHTTP(w, r.WithContext(ContextWithMemberLift(r.Context())))
		})
	}
}

func memberRefusal(spaceID string) *problem.Problem {
	detail := "this device writes only to a space its user is a member of and whose current key it holds (ADR-0128)"
	if spaceID == "" {
		detail = "this device stores vault content only while it is a writing member of some space — " +
			"its user a member, and the device holding the space's current key (ADR-0128)"
	}
	return problem.Forbidden(detail).WithCode(CodeSpaceNotMember)
}
