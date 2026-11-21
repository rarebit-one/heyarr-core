package personalstate

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
)

const (
	memberWriteShared = "0199a0a0-0000-7000-8000-000000000128"
	memberWriteEves   = "0199a0a0-0000-7000-8000-000000000129"
	// revokedEnc is the encryption key of bob's revoked phone.
	revokedEnc = "x25519:dddd111111111111111111111111111111111111111111111111111111111111"
)

// readDevice is the identity a Device credential resolves to at the read
// floor (ADR-0067): the device whose encryption key is recipient, of user p.
func readDevice(p, recipient string) auth.Identity {
	return auth.Identity{
		DeviceKey: "ed25519:" + recipient[7:15],
		Principal: auth.Principal{ID: p, Kind: auth.KindUser},
		Token:     auth.Token{Name: "device", PrincipalID: p, Scopes: []auth.Scope{auth.ScopeRead}},
	}
}

// routed serves the API's real routes, as the identity, the way the
// authenticated /api/v1 router would have left it.
func routed(t *testing.T, api *API, id auth.Identity, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(httpapi.ContextWithIdentity(req.Context(), id)))
		})
	})
	api.Mount(r)
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestAMembersDeviceWritesOnlyTheSpacesItIsAMemberOf is ADR-0128 through the
// real routes: a read-floor device pushes a change to a space whose current
// key it holds and whose membership admits its user, and to nothing else; it
// never reaches a snapshot, compaction, rotation, key deletion, re-wrap,
// replication, grant or membership change; and the write scope behaves
// exactly as before.
func TestAMembersDeviceWritesOnlyTheSpacesItIsAMemberOf(t *testing.T) {
	t.Parallel()
	revokedAt := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	members := household()
	members.extra = map[string][]deviceauth.Device{"p-bob": {
		{
			ID: "d-old", Name: "old phone", DeviceKey: "ed25519:" + revokedEnc[7:15], EncryptionKey: revokedEnc,
			EnrolledAt: revokedAt.AddDate(0, -6, 0), RevokedAt: &revokedAt,
		},
	}}
	api := membersAPI(t, members)
	owner, eve := readDevice("p-owner", ownerKey), readDevice("p-eve", eveKey)
	bob, bob2 := readDevice("p-bob", bobKey1), readDevice("p-bob", bobKey2)
	createAs(t, api, device("p-owner"), memberWriteShared, "shared", ownerKey, recoveryKey)
	createAs(t, api, device("p-eve"), memberWriteEves, "personal", eveKey)
	if rec := as(t, device("p-owner"), api.addMember, http.MethodPost, "/m", addMemberRequest{User: "bob"},
		spaceParams(memberWriteShared)); rec.Code != http.StatusCreated {
		t.Fatalf("owner adds bob: %d %s", rec.Code, rec.Body)
	}
	wantCode(t, rewrap(t, api, memberWriteShared, bobKey1, 0), http.StatusOK, "")

	n := 0
	push := func(id auth.Identity, spaceID, query string) *httptest.ResponseRecorder {
		t.Helper()
		n++
		ch, err := protocol.NewChange(spaceID, nil, []byte{byte(n), 0xc1})
		if err != nil {
			t.Fatal(err)
		}
		return routed(t, api, id, http.MethodPost, "/spaces/"+spaceID+"/changes"+query, ch)
	}

	// Allowed: the owner's device and a member's device, each holding the key.
	wantCode(t, push(owner, memberWriteShared, "?key_epoch=0"), http.StatusCreated, "")
	wantCode(t, push(bob, memberWriteShared, "?key_epoch=0"), http.StatusCreated, "")
	wantCode(t, push(eve, memberWriteEves, "?key_epoch=0"), http.StatusCreated, "")

	// Refused: a member's device that holds no copy, a stranger, another
	// space, and a revoked device whose copy is still there.
	wantCode(t, push(bob2, memberWriteShared, "?key_epoch=0"), http.StatusForbidden, httpapi.CodeSpaceNotMember)
	wantCode(t, push(eve, memberWriteShared, "?key_epoch=0"), http.StatusForbidden, httpapi.CodeSpaceNotMember)
	wantCode(t, push(bob, memberWriteEves, "?key_epoch=0"), http.StatusForbidden, httpapi.CodeSpaceNotMember)
	if _, err := api.store.PutWrappedKey(context.Background(), memberWriteShared, revokedEnc, []byte("k"), 0); err != nil {
		t.Fatal(err)
	}
	wantCode(t, push(readDevice("p-bob", revokedEnc), memberWriteShared, "?key_epoch=0"),
		http.StatusForbidden, httpapi.CodeSpaceNotMember)

	// Every member write is checked against the key epoch: it must name one,
	// and a stale one is the mismatch every writer gets.
	wantCode(t, push(bob, memberWriteShared, ""), http.StatusPreconditionRequired, CodeKeyEpochRequired)
	wantCode(t, push(bob, memberWriteShared, "?key_epoch=1"), http.StatusConflict, CodeChangeKeyEpochMismatch)

	// The write scope is unchanged: unconditional pushes still land, a read
	// bearer and a guest are refused as before.
	writer := auth.Identity{
		Principal: auth.Principal{ID: "p-owner", Kind: auth.KindUser},
		Token:     auth.Token{Scopes: []auth.Scope{auth.ScopeRead, auth.ScopeWrite}},
	}
	wantCode(t, push(writer, memberWriteShared, ""), http.StatusCreated, "")
	reader := auth.Identity{
		Principal: auth.Principal{ID: "p-owner", Kind: auth.KindUser},
		Token:     auth.Token{Scopes: []auth.Scope{auth.ScopeRead}},
	}
	if rec := push(reader, memberWriteShared, "?key_epoch=0"); rec.Code != http.StatusForbidden ||
		bytes.Contains(rec.Body.Bytes(), []byte(httpapi.CodeSpaceNotMember)) {
		t.Fatalf("read bearer: %d %s, want the plain write-scope refusal", rec.Code, rec.Body)
	}
	guest := auth.Identity{Guest: true, Token: auth.Token{Scopes: []auth.Scope{auth.ScopeRead}}}
	wantCode(t, push(guest, memberWriteShared, "?key_epoch=0"), http.StatusForbidden, "")

	// Removing bob closes the space to his device, though his copy remains
	// until a rotation; re-adding him opens it again.
	if rec := as(t, device("p-owner"), api.removeMember, http.MethodDelete, "/m", nil,
		memberParams(memberWriteShared, "bob")); rec.Code != http.StatusOK {
		t.Fatalf("owner removes bob: %d %s", rec.Code, rec.Body)
	}
	wantCode(t, push(bob, memberWriteShared, "?key_epoch=0"), http.StatusForbidden, httpapi.CodeSpaceNotMember)
	if rec := as(t, device("p-owner"), api.addMember, http.MethodPost, "/m", addMemberRequest{User: "bob"},
		spaceParams(memberWriteShared)); rec.Code != http.StatusCreated {
		t.Fatalf("owner re-adds bob: %d %s", rec.Code, rec.Body)
	}
	wantCode(t, push(bob, memberWriteShared, "?key_epoch=0"), http.StatusCreated, "")

	// Nothing else opens: not even for the space's owner's own device.
	sp := "/spaces/" + memberWriteShared
	for _, rt := range []struct{ method, path string }{
		{http.MethodPost, "/spaces"},
		{http.MethodPost, sp + "/snapshots"},
		{http.MethodPost, sp + "/compact"},
		{http.MethodPost, sp + "/rotate"},
		{http.MethodPost, sp + "/keys"},
		{http.MethodDelete, sp + "/keys/" + bobKey1},
		{http.MethodPost, "/state/replicate"},
		{http.MethodPost, sp + "/grants"},
		{http.MethodDelete, sp + "/grants/p-exec"},
		{http.MethodPost, sp + "/members"},
		{http.MethodDelete, sp + "/members/p-bob"},
		{http.MethodPost, "/service-recipients"},
	} {
		if rec := routed(t, api, owner, rt.method, rt.path, map[string]any{}); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as the owner's read-floor device: %d %s, want 403", rt.method, rt.path, rec.Code, rec.Body)
		}
	}
}

// A node that wires no member-write authority keeps every push at the write
// scope, and says so on own-devices with member_write false.
func TestNoMemberWriteAuthorityKeepsTheWriteScope(t *testing.T) {
	t.Parallel()
	api := membersAPI(t, household())
	api.memberW = nil
	createAs(t, api, device("p-owner"), memberWriteShared, "personal", ownerKey)
	ch, err := protocol.NewChange(memberWriteShared, nil, []byte("c"))
	if err != nil {
		t.Fatal(err)
	}
	owner := readDevice("p-owner", ownerKey)
	if rec := routed(t, api, owner, http.MethodPost, "/spaces/"+memberWriteShared+"/changes?key_epoch=0", ch); rec.Code != http.StatusForbidden {
		t.Fatalf("push with no authority: %d %s, want 403", rec.Code, rec.Body)
	}
	rec := as(t, owner, api.listOwnDevices, http.MethodGet, "/d", nil, spaceParams(memberWriteShared))
	var v ownDevicesView
	mustJSON(t, rec, &v)
	if v.MemberWrite {
		t.Fatalf("own-devices = %+v, want member_write false", v)
	}
}
