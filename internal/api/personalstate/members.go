package personalstate

// members.go is shared-space membership on the device-facing API (ADR-0108,
// §47): which users a space is shared with, changed only from the owner's
// device (any management-authorised device on a family space, or on a legacy
// space with no owner — ADR-0104's rule), and consulted by enrol-before-wrap so
// that a space key is wrapped only for a member's enrolled devices.
//
// Adding a member is two steps, and this route is the first: it records the
// membership and tells the caller which enrolled device keys the member holds.
// The caller's device then wraps the space's current key for each of them and
// pushes the copies through POST /spaces/{id}/keys, which from now on admits
// them. The server never wraps: it holds no key to wrap with (Invariant 6).
//
// Removing a member is a tombstone: no new wrap for the user's devices is
// admitted from then on, including on a rotation, which must name them in its
// revoke list. The rotation that makes the removal forward-secret (ADR-0103) is
// the owner's device's to run; the response names the recipients it must
// leave out.

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

// MemberResolver resolves the users a membership change names and the device
// keys they hold (ADR-0108). *deviceauth.Store satisfies it. The API learns
// which keys belong to which user — public material — never any ciphertext or
// plaintext, so the Invariant-6 boundary holds.
//
// nil is legitimate: with no resolver wired the members routes answer 503 and
// membership does not narrow the wrap gate (enrol-before-wrap alone, as before
// ADR-0108). A controller always wires one.
type MemberResolver interface {
	// ResolveUser resolves a user reference — a principal id or name, or a
	// user identity id — to the pinned user identity. deviceauth.ErrUnknownUser
	// when none matches; deviceauth.ErrAmbiguousUser when more than one does.
	ResolveUser(ctx context.Context, ref string) (deviceauth.User, error)
	// DeviceWrapRecipients is the enrolled, unrevoked device encryption keys of
	// the given user principals, keyed by recipient to the principal it speaks
	// for.
	DeviceWrapRecipients(ctx context.Context, principalIDs []string) (map[string]string, error)
}

// The problem codes a membership refusal carries, so a device branches on the
// code rather than the prose.
const (
	// CodeWrapRecipientNotMember (403): the recipient is an enrolled device,
	// but its user is not a current member of the space — never added, or
	// removed. Add the user first, or leave the device out of the rotation.
	CodeWrapRecipientNotMember = "wrap_recipient_not_member"
	// CodeMemberHasNoDevice (409): the user named has no enrolled device with
	// an encryption key, so there is nothing to wrap the space key for. They
	// enrol first (ADR-0108).
	CodeMemberHasNoDevice = "member_has_no_enrolled_device"
	// CodeMemberIsOwner (409): the space's owner cannot be removed from it.
	CodeMemberIsOwner = "member_is_owner"
)

// addMemberRequest is POST /spaces/{id}/members: the user to admit, by
// principal id or name, or user identity id.
type addMemberRequest struct {
	User string `json:"user"`
}

// memberView is one membership row.
type memberView struct {
	SpaceID       string `json:"space_id"`
	PrincipalID   string `json:"principal_id"`
	Role          string `json:"role"`
	AddedAt       string `json:"added_at"`
	AddedByDevice string `json:"added_by_device,omitempty"`
	RemovedAt     string `json:"removed_at,omitempty"`
}

func viewMember(m store.SpaceMember) memberView {
	v := memberView{
		SpaceID: m.SpaceID, PrincipalID: m.PrincipalID, Role: string(m.Role),
		AddedAt: m.AddedAt.UTC().Format(timeFormat), AddedByDevice: m.AddedByDevice,
	}
	if m.RemovedAt != nil {
		v.RemovedAt = m.RemovedAt.UTC().Format(timeFormat)
	}
	return v
}

// membersView is GET /spaces/{id}/members: every row, current and removed.
type membersView struct {
	SpaceID string       `json:"space_id"`
	Members []memberView `json:"members"`
}

// memberAddedView acks an add with what the caller's device wraps for next:
// the member's enrolled device keys, at the space's current key epoch.
type memberAddedView struct {
	memberView
	// KeyEpoch is the space's current key epoch (ADR-0103) — what the wraps
	// for WrapFor must seal.
	KeyEpoch int `json:"key_epoch"`
	// WrapFor are the member's enrolled device encryption keys, sorted: the
	// recipients the caller's device now wraps the space key for.
	WrapFor []string `json:"wrap_for"`
}

// memberRemovedView acks a removal with the recipients a rotation must leave
// out: the removed member's device keys that hold a copy of the current key.
type memberRemovedView struct {
	memberView
	// Revoke are the removed member's device keys holding a copy of the
	// current key, sorted — the `revoke` list of the rotation that makes the
	// removal forward-secret. Empty when they hold none.
	Revoke []string `json:"revoke"`
	// KeyEpoch is the space's current key epoch, the one that rotation starts
	// from.
	KeyEpoch int `json:"key_epoch"`
}

func (a *API) listMembers(w http.ResponseWriter, r *http.Request) {
	spaceID := chi.URLParam(r, "id")
	list, err := a.store.Members(r.Context(), spaceID)
	if err != nil {
		a.failMember(w, r, "listing members", err)
		return
	}
	out := membersView{SpaceID: spaceID, Members: make([]memberView, 0, len(list))}
	for _, m := range list {
		out.Members = append(out.Members, viewMember(m))
	}
	a.write(w, r, http.StatusOK, out)
}

func (a *API) addMember(w http.ResponseWriter, r *http.Request) {
	id, ok := granter(w, r)
	if !ok {
		return
	}
	if !a.membersWired(w, r) {
		return
	}
	var req addMemberRequest
	if err := httpapi.DecodeJSON(w, r, &req, maxRequestBody); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	user, ok := a.resolveMember(w, r, req.User)
	if !ok {
		return
	}
	devices, err := a.members.DeviceWrapRecipients(r.Context(), []string{user.PrincipalID})
	if err != nil {
		a.log.Error("listing a member's devices", "request_id", httpapi.RequestIDFrom(r.Context()), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if len(devices) == 0 {
		httpapi.Fail(w, r, problem.Conflict(
			"user "+req.User+" has no enrolled device with an encryption key, so there is nothing to wrap the space key for; "+
				"they enrol a device first (ADR-0108)").WithCode(CodeMemberHasNoDevice))
		return
	}
	spaceID := chi.URLParam(r, "id")
	m, err := a.store.AddMember(r.Context(), spaceID, user.PrincipalID, id.Principal.ID, id.DeviceKey)
	if err != nil {
		a.failMember(w, r, "adding a member", err)
		return
	}
	epoch, err := a.store.KeyEpoch(r.Context(), spaceID)
	if err != nil {
		a.failMember(w, r, "reading the key epoch", err)
		return
	}
	wrapFor := make([]string, 0, len(devices))
	for k := range devices {
		wrapFor = append(wrapFor, k)
	}
	slices.Sort(wrapFor)
	a.log.Info("added a space member", "space", spaceID, "principal", m.PrincipalID, "devices", len(wrapFor))
	a.write(w, r, http.StatusCreated, memberAddedView{memberView: viewMember(m), KeyEpoch: epoch, WrapFor: wrapFor})
}

func (a *API) removeMember(w http.ResponseWriter, r *http.Request) {
	id, ok := granter(w, r)
	if !ok {
		return
	}
	if !a.membersWired(w, r) {
		return
	}
	user, ok := a.resolveMember(w, r, chi.URLParam(r, "member"))
	if !ok {
		return
	}
	spaceID := chi.URLParam(r, "id")
	m, err := a.store.RemoveMember(r.Context(), spaceID, user.PrincipalID, id.Principal.ID, id.DeviceKey)
	if err != nil {
		a.failMember(w, r, "removing a member", err)
		return
	}
	// The recipients the rotation must leave out: the removed member's device
	// keys that hold a copy of the current key. Read after the tombstone, so a
	// wrap racing the removal is either refused by it or listed here.
	devices, err := a.members.DeviceWrapRecipients(r.Context(), []string{user.PrincipalID})
	if err != nil {
		a.log.Error("listing a member's devices", "request_id", httpapi.RequestIDFrom(r.Context()), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	epoch, held, err := a.store.KeyState(r.Context(), spaceID)
	if err != nil {
		a.failMember(w, r, "reading the key state", err)
		return
	}
	revoke := []string{}
	for _, k := range held {
		if _, theirs := devices[k.Recipient]; theirs {
			revoke = append(revoke, k.Recipient)
		}
	}
	slices.Sort(revoke)
	a.log.Info("removed a space member", "space", spaceID, "principal", m.PrincipalID, "holding", len(revoke))
	a.write(w, r, http.StatusOK, memberRemovedView{memberView: viewMember(m), Revoke: revoke, KeyEpoch: epoch})
}

// membersWired writes a 503 and returns false when no resolver is wired.
func (a *API) membersWired(w http.ResponseWriter, r *http.Request) bool {
	if a.members == nil {
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeServiceUnavailable,
			"Service Unavailable", "this node resolves no user identities"))
		return false
	}
	return true
}

// resolveMember resolves the user a membership change names. No match is a
// 404; a reference matching more than one identity is a 400 — name the user
// by principal id.
func (a *API) resolveMember(w http.ResponseWriter, r *http.Request, ref string) (deviceauth.User, bool) {
	if ref == "" {
		httpapi.Fail(w, r, problem.BadRequest("a membership change names the user, by principal id or name"))
		return deviceauth.User{}, false
	}
	u, err := a.members.ResolveUser(r.Context(), ref)
	switch {
	case errors.Is(err, deviceauth.ErrUnknownUser):
		httpapi.Fail(w, r, problem.NotFound("no pinned user identity matches "+ref))
		return deviceauth.User{}, false
	case errors.Is(err, deviceauth.ErrAmbiguousUser):
		httpapi.Fail(w, r, problem.BadRequest(ref+" matches more than one user identity; name the user by principal id"))
		return deviceauth.User{}, false
	case err != nil:
		a.log.Error("resolving a user", "request_id", httpapi.RequestIDFrom(r.Context()), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return deviceauth.User{}, false
	}
	return u, true
}

func (a *API) failMember(w http.ResponseWriter, r *http.Request, doing string, err error) {
	switch {
	case errors.Is(err, store.ErrNotSpaceOwner):
		httpapi.Fail(w, r, problem.Forbidden(
			"only the space owner's device changes who is a member of it (ADR-0108, ADR-0104)"))
	case errors.Is(err, store.ErrNotMember):
		httpapi.Fail(w, r, problem.NotFound(err.Error()))
	case errors.Is(err, store.ErrOwnerNotRemovable):
		httpapi.Fail(w, r, problem.Conflict(err.Error()).WithCode(CodeMemberIsOwner))
	case errors.Is(err, store.ErrEmptyMember):
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
	default:
		a.failStore(w, r, doing, err)
	}
}

// memberGate is what recipientsAllowed learns about a space's membership: the
// wrap recipients that are a current member's device, those that are a
// removed member's, and those already holding a copy of the current key.
type memberGate struct {
	current, removed, held map[string]bool
}

// memberGateFor reads the membership gate for spaceID, or nil when membership
// does not narrow the gate there: no resolver is wired, the space is a family
// space, or it has no membership rows (a space from before ADR-0108, or one a
// bearer token created).
func (a *API) memberGateFor(ctx context.Context, spaceID string) (*memberGate, error) {
	if a.members == nil {
		return nil, nil
	}
	ms, err := a.store.SpaceMembership(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	if !ms.Gated() {
		return nil, nil
	}
	principals := make([]string, 0, len(ms.Current)+len(ms.Removed))
	for p := range ms.Current {
		principals = append(principals, p)
	}
	for p := range ms.Removed {
		principals = append(principals, p)
	}
	devices, err := a.members.DeviceWrapRecipients(ctx, principals)
	if err != nil {
		return nil, err
	}
	g := &memberGate{current: map[string]bool{}, removed: map[string]bool{}, held: map[string]bool{}}
	for recipient, principal := range devices {
		if ms.Current[principal] {
			g.current[recipient] = true
		} else {
			g.removed[recipient] = true
		}
	}
	_, keys, err := a.store.KeyState(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		g.held[k.Recipient] = true
	}
	return g, nil
}

// admits reports whether the gate admits a wrap for recipient, with the reason
// it does not. A current member's device is admitted; a removed member's never
// is, even while it still holds a copy (the rotation names it in revoke
// instead); a device whose user has no membership row is admitted only while
// it already holds a copy — a recipient the space was created for, or added
// before ADR-0108 — never as a new recipient.
func (g *memberGate) admits(recipient string) (bool, string) {
	switch {
	case g.current[recipient]:
		return true, ""
	case g.removed[recipient]:
		return false, "its user was removed from the space; a rotation names it in revoke instead of re-wrapping it"
	case g.held[recipient]:
		return true, ""
	default:
		return false, "its user is not a member of the space; add them first (POST /spaces/{id}/members)"
	}
}

// compile-time: the device store resolves members and their devices.
var _ MemberResolver = (*deviceauth.Store)(nil)
