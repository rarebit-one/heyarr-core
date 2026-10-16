package personalstate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

// The cast of a shared-space scene (ADR-0108): an owner with one device, bob
// with two, eve with one, carol with none, and the owner's recovery key.
const (
	ownerKey    = "x25519:aaaa111111111111111111111111111111111111111111111111111111111111"
	bobKey1     = "x25519:bbbb111111111111111111111111111111111111111111111111111111111111"
	bobKey2     = "x25519:bbbb222222222222222222222222222222222222222222222222222222222222"
	eveKey      = "x25519:eeee111111111111111111111111111111111111111111111111111111111111"
	recoveryKey = "x25519:cccc111111111111111111111111111111111111111111111111111111111111"

	membersSpace = "0199a0a0-0000-7000-8000-000000000108"
)

// fakeMembers resolves users by principal id or name and knows whose device
// each key is — the deviceauth store's two answers, without the enrolment.
type fakeMembers struct {
	users   map[string]deviceauth.User // by principal id and by name
	devices map[string]string          // recipient -> principal id
}

func (f fakeMembers) ResolveUser(_ context.Context, ref string) (deviceauth.User, error) {
	u, ok := f.users[ref]
	if !ok {
		return deviceauth.User{}, deviceauth.ErrUnknownUser
	}
	return u, nil
}

func (f fakeMembers) DeviceWrapRecipients(_ context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for recipient, principal := range f.devices {
		for _, id := range ids {
			if id == principal {
				out[recipient] = principal
			}
		}
	}
	return out, nil
}

func household() fakeMembers {
	users := map[string]deviceauth.User{}
	for _, u := range []deviceauth.User{
		{ID: "u-owner", PrincipalID: "p-owner", Name: "owner"},
		{ID: "u-bob", PrincipalID: "p-bob", Name: "bob"},
		{ID: "u-eve", PrincipalID: "p-eve", Name: "eve"},
		{ID: "u-carol", PrincipalID: "p-carol", Name: "carol"},
	} {
		users[u.PrincipalID] = u
		users[u.Name] = u
	}
	return fakeMembers{
		users: users,
		devices: map[string]string{
			ownerKey: "p-owner", bobKey1: "p-bob", bobKey2: "p-bob", eveKey: "p-eve",
		},
	}
}

// membersAPI is the API over a fixed clock with every household key enrolled,
// the recovery key known as such, and the household as the member resolver
// (or none, when members is nil).
func membersAPI(t *testing.T, members MemberResolver) *API {
	t.Helper()
	db := newDB(t)
	clock := fixedClock{t: time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(store.Options{Writer: db.Writer(), Reader: db.Reader(), Events: log, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	api, err := New(Options{
		Store: st,
		Authorizer: fakeAuthorizer{
			allowed:  map[string]bool{ownerKey: true, bobKey1: true, bobKey2: true, eveKey: true, recoveryKey: true},
			recovery: map[string]bool{recoveryKey: true},
		},
		Members: members,
	})
	if err != nil {
		t.Fatal(err)
	}
	return api
}

// device is the identity a Device credential resolves to for user principal p.
func device(p string) auth.Identity {
	return auth.Identity{
		DeviceKey: "ed25519:" + p,
		Principal: auth.Principal{ID: p, Kind: auth.KindUser, Name: strings.TrimPrefix(p, "p-")},
	}
}

// bearer is a write-scoped bearer token's identity: a principal, no device.
var bearer = auth.Identity{Principal: auth.Principal{ID: "p-owner", Kind: auth.KindUser}}

// as invokes a handler as the given identity, the way the middleware would
// have left it.
func as(t *testing.T, id auth.Identity, handler http.HandlerFunc, method, target string, body any, params map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return call(t, func(w http.ResponseWriter, r *http.Request) {
		handler(w, r.WithContext(httpapi.ContextWithIdentity(r.Context(), id)))
	}, method, target, body, params)
}

func spaceParams(id string) map[string]string { return map[string]string{"id": id} }

func memberParams(id, member string) map[string]string {
	return map[string]string{"id": id, "member": member}
}

// createAs creates a space of kind as the identity, wrapped for the given
// recipients at epoch 0.
func createAs(t *testing.T, api *API, id auth.Identity, spaceID, kind string, recipients ...string) {
	t.Helper()
	req := createSpaceRequest{ID: spaceID, Kind: kind}
	for _, r := range recipients {
		req.WrappedKeys = append(req.WrappedKeys, wrappedKeyInput{Recipient: r, Wrapped: []byte("k0-for-" + r[7:11])})
	}
	if rec := as(t, id, api.createSpace, http.MethodPost, "/spaces", req, nil); rec.Code != http.StatusCreated {
		t.Fatalf("create %s space: %d %s", kind, rec.Code, rec.Body)
	}
}

// rewrap pushes one opaque copy of the current key for recipient and returns
// the status and body.
func rewrap(t *testing.T, api *API, spaceID, recipient string, epoch int) *httptest.ResponseRecorder {
	t.Helper()
	return call(t, api.rewrapKeys, http.MethodPost, "/spaces/"+spaceID+"/keys", rewrapRequest{
		WrappedKeys: []wrappedKeyInput{{Recipient: recipient, Wrapped: []byte("k-for-" + recipient[7:11]), Epoch: epoch}},
	}, spaceParams(spaceID))
}

func wantCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status || (code != "" && !strings.Contains(rec.Body.String(), `"code":"`+code+`"`)) {
		t.Fatalf("got %d %s; want %d with code %q", rec.Code, rec.Body, status, code)
	}
}

// TestOwnerSharesASpaceAndTheGateFollowsMembership is the ADR-0108 scene end to
// end at the handler layer: the owner's device creates a shared space; a
// stranger's device and a bearer token cannot change its membership; the owner
// adds bob and is told which of bob's devices to wrap for; wraps for bob's
// devices are then admitted and wraps for eve's are not; bob cannot re-share;
// the owner removes bob and is told which recipients the rotation must revoke;
// from then on no wrap for bob's devices is admitted, including by a rotation,
// while a rotation that revokes them lands. The list is pinned as a golden.
func TestOwnerSharesASpaceAndTheGateFollowsMembership(t *testing.T) {
	t.Parallel()
	api := membersAPI(t, household())
	owner, bob, eve := device("p-owner"), device("p-bob"), device("p-eve")
	createAs(t, api, owner, membersSpace, "shared", ownerKey, recoveryKey)

	add := addMemberRequest{User: "bob"}
	wantCode(t, as(t, eve, api.addMember, http.MethodPost, "/m", add, spaceParams(membersSpace)), http.StatusForbidden, "")
	wantCode(t, as(t, bearer, api.addMember, http.MethodPost, "/m", add, spaceParams(membersSpace)), http.StatusForbidden, "")
	// Before bob is a member, a wrap for his device is refused: the space has
	// members (its owner) and he is not one.
	wantCode(t, rewrap(t, api, membersSpace, bobKey1, 0), http.StatusForbidden, CodeWrapRecipientNotMember)

	rec := as(t, owner, api.addMember, http.MethodPost, "/m", add, spaceParams(membersSpace))
	if rec.Code != http.StatusCreated {
		t.Fatalf("owner adds bob: %d %s", rec.Code, rec.Body)
	}
	var added memberAddedView
	mustJSON(t, rec, &added)
	if added.PrincipalID != "p-bob" || added.Role != "member" || added.KeyEpoch != 0 ||
		len(added.WrapFor) != 2 || added.WrapFor[0] != bobKey1 || added.WrapFor[1] != bobKey2 {
		t.Fatalf("added = %+v, want bob as member with both his devices to wrap for", added)
	}
	// Idempotent, and the second answer still names the devices to wrap for.
	if again := as(t, owner, api.addMember, http.MethodPost, "/m", add, spaceParams(membersSpace)); again.Code != http.StatusCreated {
		t.Fatalf("a second add: %d %s", again.Code, again.Body)
	}

	// The two-step: the owner's device wraps for bob's devices through /keys.
	wantCode(t, rewrap(t, api, membersSpace, bobKey1, 0), http.StatusOK, "")
	wantCode(t, rewrap(t, api, membersSpace, bobKey2, 0), http.StatusOK, "")
	// Not for eve's: enrolled, but not a member.
	wantCode(t, rewrap(t, api, membersSpace, eveKey, 0), http.StatusForbidden, CodeWrapRecipientNotMember)
	// The recovery key is not a member device and passes the gate as before.
	wantCode(t, rewrap(t, api, membersSpace, recoveryKey, 0), http.StatusOK, "")
	// An unenrolled key is still the older refusal, not a membership one.
	wantCode(t, rewrap(t, api, membersSpace, strangerKey, 0), http.StatusForbidden, CodeWrapRecipientNotAllowed)

	// Members may not re-share.
	wantCode(t, as(t, bob, api.addMember, http.MethodPost, "/m", addMemberRequest{User: "eve"}, spaceParams(membersSpace)),
		http.StatusForbidden, "")
	// Nor remove anyone, nor can the owner remove themselves.
	wantCode(t, as(t, bob, api.removeMember, http.MethodDelete, "/m", nil, memberParams(membersSpace, "p-owner")),
		http.StatusForbidden, "")
	wantCode(t, as(t, owner, api.removeMember, http.MethodDelete, "/m", nil, memberParams(membersSpace, "owner")),
		http.StatusConflict, CodeMemberIsOwner)
	// A user with no enrolled device cannot be added: there is nothing to wrap for.
	wantCode(t, as(t, owner, api.addMember, http.MethodPost, "/m", addMemberRequest{User: "carol"}, spaceParams(membersSpace)),
		http.StatusConflict, CodeMemberHasNoDevice)
	wantCode(t, as(t, owner, api.addMember, http.MethodPost, "/m", addMemberRequest{User: "nobody"}, spaceParams(membersSpace)),
		http.StatusNotFound, "")
	wantCode(t, as(t, owner, api.addMember, http.MethodPost, "/m", addMemberRequest{}, spaceParams(membersSpace)),
		http.StatusBadRequest, "")

	list := call(t, api.listMembers, http.MethodGet, "/m", nil, spaceParams(membersSpace))
	if list.Code != http.StatusOK {
		t.Fatalf("list: %d %s", list.Code, list.Body)
	}
	testutil.Golden(t, "testdata/members.json", indent(t, list.Body.Bytes()))

	// Removal: the tombstone, and the recipients the rotation must leave out —
	// bob's devices that hold the current key (both, after the wraps above).
	rec = as(t, owner, api.removeMember, http.MethodDelete, "/m", nil, memberParams(membersSpace, "bob"))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner removes bob: %d %s", rec.Code, rec.Body)
	}
	var removed memberRemovedView
	mustJSON(t, rec, &removed)
	if removed.PrincipalID != "p-bob" || removed.RemovedAt == "" || removed.KeyEpoch != 0 ||
		len(removed.Revoke) != 2 || removed.Revoke[0] != bobKey1 || removed.Revoke[1] != bobKey2 {
		t.Fatalf("removed = %+v, want bob's tombstone naming both his devices to revoke", removed)
	}
	wantCode(t, as(t, owner, api.removeMember, http.MethodDelete, "/m", nil, memberParams(membersSpace, "bob")),
		http.StatusNotFound, "")
	// Still a copy's holder, but a removed member: no new wrap, by re-wrap or rotation.
	wantCode(t, rewrap(t, api, membersSpace, bobKey1, 0), http.StatusForbidden, CodeWrapRecipientNotMember)
	rot := rotateRequest{
		ExpectedEpoch: 0, SealedPrev: []byte("k0-under-k1"),
		WrappedKeys: []wrappedKeyInput{
			{Recipient: ownerKey, Wrapped: []byte("k1-owner")},
			{Recipient: recoveryKey, Wrapped: []byte("k1-recovery")},
			{Recipient: bobKey1, Wrapped: []byte("k1-bob")},
		},
		Revoke: revoking(bobKey2),
	}
	wantCode(t, call(t, api.rotateKey, http.MethodPost, "/r", rot, spaceParams(membersSpace)), http.StatusForbidden, CodeWrapRecipientNotMember)
	rot.WrappedKeys = rot.WrappedKeys[:2]
	rot.Revoke = revoking(bobKey1, bobKey2)
	wantCode(t, call(t, api.rotateKey, http.MethodPost, "/r", rot, spaceParams(membersSpace)), http.StatusOK, "")
	// Re-admitted, bob's devices may be wrapped for again, at the new epoch.
	if rec := as(t, owner, api.addMember, http.MethodPost, "/m", add, spaceParams(membersSpace)); rec.Code != http.StatusCreated {
		t.Fatalf("re-add: %d %s", rec.Code, rec.Body)
	}
	wantCode(t, rewrap(t, api, membersSpace, bobKey1, 1), http.StatusOK, "")

	// The record: the owner and bob, current again.
	rows, err := api.store.Members(context.Background(), membersSpace)
	if err != nil || len(rows) != 2 || !rows[1].Current() {
		t.Fatalf("members = %+v, %v; want the owner and bob, current", rows, err)
	}
}

// TestMembershipGateSpares pins what the gate leaves alone: a family space (every
// household member's by definition), a space with no membership rows (a bearer
// created it, or it predates ADR-0108), a recipient the space was created for
// whose user never became a member (it holds a copy and is re-wrapped as
// before), and a node with no member resolver.
func TestMembershipGateSpares(t *testing.T) {
	t.Parallel()
	owner, eve := device("p-owner"), device("p-eve")

	t.Run("family space: any device changes it and no key is gated", func(t *testing.T) {
		t.Parallel()
		api := membersAPI(t, household())
		createAs(t, api, owner, membersSpace, "family", ownerKey)
		wantCode(t, as(t, eve, api.addMember, http.MethodPost, "/m", addMemberRequest{User: "bob"}, spaceParams(membersSpace)),
			http.StatusCreated, "")
		wantCode(t, rewrap(t, api, membersSpace, eveKey, 0), http.StatusOK, "")
		wantCode(t, as(t, eve, api.removeMember, http.MethodDelete, "/m", nil, memberParams(membersSpace, "owner")),
			http.StatusConflict, CodeMemberIsOwner)
	})

	t.Run("a space with no owner: anyone may share it, and it is gated only once shared", func(t *testing.T) {
		t.Parallel()
		api := membersAPI(t, household())
		createAs(t, api, bearer, membersSpace, "personal", ownerKey)
		wantCode(t, rewrap(t, api, membersSpace, eveKey, 0), http.StatusOK, "")
		list := call(t, api.listMembers, http.MethodGet, "/m", nil, spaceParams(membersSpace))
		var lv membersView
		mustJSON(t, list, &lv)
		if len(lv.Members) != 0 {
			t.Fatalf("an ownerless space has members: %+v", lv)
		}
		wantCode(t, as(t, eve, api.addMember, http.MethodPost, "/m", addMemberRequest{User: "bob"}, spaceParams(membersSpace)),
			http.StatusCreated, "")
		// Now gated: bob's devices in, a never-member's new device out, the
		// holders from before still re-wrapped.
		wantCode(t, rewrap(t, api, membersSpace, bobKey1, 0), http.StatusOK, "")
		wantCode(t, rewrap(t, api, membersSpace, eveKey, 0), http.StatusOK, "")
		wantCode(t, rewrap(t, api, membersSpace, bobKey2, 0), http.StatusOK, "")
		wantCode(t, rewrap(t, api, membersSpace, ownerKey, 0), http.StatusOK, "")
	})

	t.Run("a recipient from creation whose user never joined keeps being re-wrapped", func(t *testing.T) {
		t.Parallel()
		api := membersAPI(t, household())
		createAs(t, api, owner, membersSpace, "personal", ownerKey, eveKey, recoveryKey)
		wantCode(t, rewrap(t, api, membersSpace, eveKey, 0), http.StatusOK, "")
		rot := rotateRequest{
			ExpectedEpoch: 0, SealedPrev: []byte("k0-under-k1"),
			WrappedKeys: []wrappedKeyInput{
				{Recipient: ownerKey, Wrapped: []byte("k1")},
				{Recipient: eveKey, Wrapped: []byte("k1")},
				{Recipient: recoveryKey, Wrapped: []byte("k1")},
			},
			Revoke: revoking(),
		}
		wantCode(t, call(t, api.rotateKey, http.MethodPost, "/r", rot, spaceParams(membersSpace)), http.StatusOK, "")
		// But a new device of a non-member is not admitted.
		wantCode(t, rewrap(t, api, membersSpace, bobKey1, 1), http.StatusForbidden, CodeWrapRecipientNotMember)
	})

	t.Run("no resolver: the members API is unavailable and the gate is off", func(t *testing.T) {
		t.Parallel()
		api := membersAPI(t, nil)
		createAs(t, api, owner, membersSpace, "shared", ownerKey)
		wantCode(t, as(t, owner, api.addMember, http.MethodPost, "/m", addMemberRequest{User: "bob"}, spaceParams(membersSpace)),
			http.StatusServiceUnavailable, "")
		wantCode(t, rewrap(t, api, membersSpace, eveKey, 0), http.StatusOK, "")
		// Listing still works: the owner row is there.
		list := call(t, api.listMembers, http.MethodGet, "/m", nil, spaceParams(membersSpace))
		var lv membersView
		mustJSON(t, list, &lv)
		if len(lv.Members) != 1 || lv.Members[0].Role != "owner" {
			t.Fatalf("members = %+v", lv)
		}
	})

	t.Run("an unknown space is 404 everywhere", func(t *testing.T) {
		t.Parallel()
		api := membersAPI(t, household())
		unknown := mustUUID(t)
		wantCode(t, call(t, api.listMembers, http.MethodGet, "/m", nil, spaceParams(unknown)), http.StatusNotFound, "")
		wantCode(t, as(t, owner, api.addMember, http.MethodPost, "/m", addMemberRequest{User: "bob"}, spaceParams(unknown)),
			http.StatusNotFound, "")
		wantCode(t, as(t, owner, api.removeMember, http.MethodDelete, "/m", nil, memberParams(unknown, "bob")),
			http.StatusNotFound, "")
		wantCode(t, rewrap(t, api, unknown, bobKey1, 0), http.StatusNotFound, "")
	})
}
