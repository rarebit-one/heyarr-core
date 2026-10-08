package personalstate

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// goldenSpace is a fixed space id so the golden documents are stable.
const goldenSpace = "0199a0a0-0000-7000-8000-000000000103"

// apiAt builds the API over a store with a fixed clock and an optional
// authorizer, so the wire shapes below are byte-stable.
func apiAt(t *testing.T, authorizer RecipientAuthorizer) *API {
	t.Helper()
	db := newDB(t)
	clock := fixedClock{t: time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(store.Options{Writer: db.Writer(), Reader: db.Reader(), Events: log, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	api, err := New(Options{Store: st, Authorizer: authorizer})
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func idParam() map[string]string { return map[string]string{"id": goldenSpace} }

// revoking is a rotation's revoke set; the field is required, so even an empty
// one is a non-nil pointer.
func revoking(recipients ...string) *[]string {
	if recipients == nil {
		recipients = []string{}
	}
	return &recipients
}

// indent re-renders a JSON body indented, for a readable golden diff.
func indent(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Indent(&buf, body, "", "  "); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	return append(buf.Bytes(), '\n')
}

// TestKeyEpochWireShapes pins the ADR-0103 device-facing shapes: the keys list
// carries each copy's epoch and the space's key_epoch; the history lists the
// opaque rows oldest first; a rotation answers the new epoch. Hand-checked
// fields alongside, so the golden is not the only witness.
func TestKeyEpochWireShapes(t *testing.T) {
	t.Parallel()
	api := apiAt(t, nil)
	create := createSpaceRequest{ID: goldenSpace, Kind: "family", WrappedKeys: []wrappedKeyInput{
		{Recipient: enrolledKey, Wrapped: []byte("k0-for-enrolled")},
		{Recipient: strangerKey, Wrapped: []byte("k0-for-revoked")},
	}}
	if rec := call(t, api.createSpace, http.MethodPost, "/spaces", create, nil); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}

	rot := call(t, api.rotateKey, http.MethodPost, "/spaces/"+goldenSpace+"/rotate", rotateRequest{
		ExpectedEpoch: 0,
		SealedPrev:    []byte("k0-sealed-under-k1"),
		WrappedKeys:   []wrappedKeyInput{{Recipient: enrolledKey, Wrapped: []byte("k1-for-enrolled")}},
		Revoke:        revoking(strangerKey),
	}, idParam())
	if rot.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rot.Code, rot.Body)
	}
	testutil.Golden(t, "testdata/rotate.json", indent(t, rot.Body.Bytes()))

	keys := call(t, api.listWrappedKeys, http.MethodGet, "/spaces/"+goldenSpace+"/keys", nil, idParam())
	if keys.Code != http.StatusOK {
		t.Fatalf("keys: %d %s", keys.Code, keys.Body)
	}
	var kv wrappedKeysView
	mustJSON(t, keys, &kv)
	if kv.KeyEpoch != 1 || len(kv.WrappedKeys) != 1 || kv.WrappedKeys[0].Recipient != enrolledKey || kv.WrappedKeys[0].Epoch != 1 {
		t.Fatalf("keys after rotation = %+v, want only the enrolled recipient at epoch 1", kv)
	}
	testutil.Golden(t, "testdata/keys_after_rotation.json", indent(t, keys.Body.Bytes()))

	hist := call(t, api.listKeyHistory, http.MethodGet, "/spaces/"+goldenSpace+"/key-history", nil, idParam())
	if hist.Code != http.StatusOK {
		t.Fatalf("history: %d %s", hist.Code, hist.Body)
	}
	var hv keyHistoryView
	mustJSON(t, hist, &hv)
	if len(hv.Entries) != 1 || hv.Entries[0].Epoch != 1 || string(hv.Entries[0].SealedPrev) != "k0-sealed-under-k1" {
		t.Fatalf("history = %+v, want the one opaque row", hv)
	}
	testutil.Golden(t, "testdata/key_history.json", indent(t, hist.Body.Bytes()))
}

// TestKeyEpochRefusals: each refusal maps to the status and code a device
// branches on, and enrol-before-wrap guards a rotation like create and re-wrap.
func TestKeyEpochRefusals(t *testing.T) {
	t.Parallel()
	api := apiAt(t, fakeAuthorizer{allowed: map[string]bool{enrolledKey: true}})
	create := createSpaceRequest{ID: goldenSpace, Kind: "personal", WrappedKeys: []wrappedKeyInput{
		{Recipient: enrolledKey, Wrapped: []byte("k0")},
	}}
	if rec := call(t, api.createSpace, http.MethodPost, "/spaces", create, nil); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	enrolled := []wrappedKeyInput{{Recipient: enrolledKey, Wrapped: []byte("k1")}}

	steps := []struct {
		name     string
		handler  http.HandlerFunc
		body     any
		want     int
		wantCode string
	}{
		{
			"a rotation for an unenrolled recipient", api.rotateKey,
			rotateRequest{SealedPrev: []byte("p"), WrappedKeys: []wrappedKeyInput{{Recipient: strangerKey, Wrapped: []byte("k1")}}, Revoke: revoking()},
			http.StatusForbidden, "",
		},
		{
			"a wrap ahead of the space", api.rewrapKeys,
			rewrapRequest{WrappedKeys: []wrappedKeyInput{{Recipient: enrolledKey, Wrapped: []byte("k1"), Epoch: 1}}},
			http.StatusConflict, CodeKeyEpochAhead,
		},
		{
			"the rotation", api.rotateKey,
			rotateRequest{SealedPrev: []byte("p"), WrappedKeys: enrolled, Revoke: revoking()},
			http.StatusOK, "",
		},
		{
			"a racing rotation", api.rotateKey,
			rotateRequest{SealedPrev: []byte("q"), WrappedKeys: enrolled, Revoke: revoking()},
			http.StatusConflict, CodeKeyEpochConflict,
		},
		{
			"a wrap of the superseded key", api.rewrapKeys,
			rewrapRequest{WrappedKeys: []wrappedKeyInput{{Recipient: enrolledKey, Wrapped: []byte("k0")}}},
			http.StatusConflict, CodeKeyEpochStale,
		},
		{
			"a wrap at the current epoch", api.rewrapKeys,
			rewrapRequest{WrappedKeys: []wrappedKeyInput{{Recipient: enrolledKey, Wrapped: []byte("k1"), Epoch: 1}}},
			http.StatusOK, "",
		},
		{
			"a rotation revoking a recipient that holds no copy", api.rotateKey,
			rotateRequest{ExpectedEpoch: 1, SealedPrev: []byte("p"), WrappedKeys: enrolled, Revoke: revoking(strangerKey)},
			http.StatusConflict, CodeRotationRecipientsChanged,
		},
		{
			"a rotation both re-wrapping and revoking a recipient", api.rotateKey,
			rotateRequest{ExpectedEpoch: 1, SealedPrev: []byte("p"), WrappedKeys: enrolled, Revoke: revoking(enrolledKey)},
			http.StatusBadRequest, "",
		},
		{
			"a rotation that omits revoke", api.rotateKey,
			map[string]any{"expected_epoch": 1, "sealed_prev": []byte("p"), "wrapped_keys": enrolled},
			http.StatusBadRequest, "",
		},
		{
			"a rotation that names no revoke set", api.rotateKey,
			rotateRequest{ExpectedEpoch: 1, SealedPrev: []byte("p"), WrappedKeys: enrolled},
			http.StatusBadRequest, "",
		},
		{
			"a rotation with no sealed key", api.rotateKey,
			rotateRequest{ExpectedEpoch: 1, WrappedKeys: enrolled, Revoke: revoking()},
			http.StatusBadRequest, "",
		},
		{
			"a rotation with no wraps", api.rotateKey,
			rotateRequest{ExpectedEpoch: 1, SealedPrev: []byte("p"), Revoke: revoking()},
			http.StatusBadRequest, "",
		},
		{
			"a negative expected epoch", api.rotateKey,
			rotateRequest{ExpectedEpoch: -1, SealedPrev: []byte("p"), WrappedKeys: enrolled, Revoke: revoking()},
			http.StatusBadRequest, "",
		},
	}
	for _, step := range steps {
		rec := call(t, step.handler, http.MethodPost, "/spaces/"+goldenSpace, step.body, idParam())
		if rec.Code != step.want {
			t.Fatalf("%s: status %d, want %d: %s", step.name, rec.Code, step.want, rec.Body)
		}
		if step.wantCode != "" {
			var doc struct {
				Code   string `json:"code"`
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || doc.Code != step.wantCode {
				t.Fatalf("%s: code %q (%v), want %q: %s", step.name, doc.Code, err, step.wantCode, rec.Body)
			}
			if doc.Detail == "" {
				t.Fatalf("%s: a 409 with no detail", step.name)
			}
		}
	}
}

// TestCreateRefusesANonZeroEpochBeforeRecordingTheSpace: a new space has only its
// original key, so a create-time wrap naming another epoch is a 400 — and it is
// refused before the space is recorded, leaving no orphan behind.
func TestCreateRefusesANonZeroEpochBeforeRecordingTheSpace(t *testing.T) {
	t.Parallel()
	api := apiAt(t, nil)
	rec := call(t, api.createSpace, http.MethodPost, "/spaces", createSpaceRequest{
		ID: goldenSpace, Kind: "personal",
		WrappedKeys: []wrappedKeyInput{{Recipient: enrolledKey, Wrapped: []byte("k"), Epoch: 1}},
	}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with epoch 1: %d %s, want 400", rec.Code, rec.Body)
	}
	if list, err := api.store.ListSpaces(context.Background()); err != nil || len(list) != 0 {
		t.Fatalf("a refused create left %d space(s) behind (%v)", len(list), err)
	}
}

// TestRotationMustRewrapTheRecoveryKey (ADR-0022, ADR-0103): a rotation that
// leaves out a recovery key holding a copy of the current key is a 409 with a
// stable code, and writes nothing; one that re-wraps it lands.
func TestRotationMustRewrapTheRecoveryKey(t *testing.T) {
	t.Parallel()
	const recoveryKey = "x25519:3333333333333333333333333333333333333333333333333333333333333333"
	cases := []struct {
		name     string
		wraps    []wrappedKeyInput
		revoke   *[]string
		want     int
		wantCode string
	}{
		{
			name:     "the recovery key revoked",
			wraps:    []wrappedKeyInput{{Recipient: enrolledKey, Wrapped: []byte("k1")}},
			revoke:   revoking(recoveryKey),
			want:     http.StatusConflict,
			wantCode: CodeRotationDropsRecovery,
		},
		{
			name:     "the recovery key neither re-wrapped nor revoked",
			wraps:    []wrappedKeyInput{{Recipient: enrolledKey, Wrapped: []byte("k1")}},
			revoke:   revoking(),
			want:     http.StatusConflict,
			wantCode: CodeRotationRecipientsChanged,
		},
		{
			name: "the recovery key re-wrapped",
			wraps: []wrappedKeyInput{
				{Recipient: enrolledKey, Wrapped: []byte("k1")},
				{Recipient: recoveryKey, Wrapped: []byte("k1-recovery")},
			},
			revoke: revoking(),
			want:   http.StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api := apiAt(t, fakeAuthorizer{
				allowed:  map[string]bool{enrolledKey: true, recoveryKey: true},
				recovery: map[string]bool{recoveryKey: true},
			})
			create := createSpaceRequest{ID: goldenSpace, Kind: "personal", WrappedKeys: []wrappedKeyInput{
				{Recipient: enrolledKey, Wrapped: []byte("k0")},
				{Recipient: recoveryKey, Wrapped: []byte("k0-recovery")},
			}}
			if rec := call(t, api.createSpace, http.MethodPost, "/spaces", create, nil); rec.Code != http.StatusCreated {
				t.Fatalf("create: %d %s", rec.Code, rec.Body)
			}
			rec := call(t, api.rotateKey, http.MethodPost, "/spaces/"+goldenSpace+"/rotate",
				rotateRequest{SealedPrev: []byte("p"), WrappedKeys: tc.wraps, Revoke: tc.revoke}, idParam())
			if rec.Code != tc.want {
				t.Fatalf("rotate: %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
			epoch, keys, err := api.store.KeyState(context.Background(), goldenSpace)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantCode != "" {
				var doc struct {
					Code string `json:"code"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || doc.Code != tc.wantCode {
					t.Fatalf("code %q (%v), want %q", doc.Code, err, tc.wantCode)
				}
				if epoch != 0 || len(keys) != 2 {
					t.Fatalf("a refused rotation wrote: epoch %d, %d wraps", epoch, len(keys))
				}
				return
			}
			if epoch != 1 || len(keys) != 2 {
				t.Fatalf("after rotation: epoch %d, %d wraps; want 1 and 2", epoch, len(keys))
			}
		})
	}
}

// TestPutChangeKeyEpoch (#712): ?key_epoch= makes a change push conditional on
// the space's current epoch — a 409 with change_key_epoch_mismatch after a
// rotation, a 400 when malformed — and without it the push is unconditional.
func TestPutChangeKeyEpoch(t *testing.T) {
	t.Parallel()
	api := apiAt(t, fakeAuthorizer{allowed: map[string]bool{enrolledKey: true}})
	create := createSpaceRequest{ID: goldenSpace, Kind: "personal", WrappedKeys: []wrappedKeyInput{
		{Recipient: enrolledKey, Wrapped: []byte("k0")},
	}}
	if rec := call(t, api.createSpace, http.MethodPost, "/spaces", create, nil); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	push := func(query string, ct string) *httptest.ResponseRecorder {
		t.Helper()
		ch, err := protocol.NewChange(goldenSpace, nil, []byte(ct))
		if err != nil {
			t.Fatal(err)
		}
		return call(t, api.putChange, http.MethodPost, "/spaces/"+goldenSpace+"/changes"+query, ch, idParam())
	}

	if rec := push("?key_epoch=0", "a"); rec.Code != http.StatusCreated {
		t.Fatalf("push at epoch 0: %d %s", rec.Code, rec.Body)
	}
	rot := call(t, api.rotateKey, http.MethodPost, "/spaces/"+goldenSpace+"/rotate", rotateRequest{
		SealedPrev: []byte("p"), WrappedKeys: []wrappedKeyInput{{Recipient: enrolledKey, Wrapped: []byte("k1")}}, Revoke: revoking(),
	}, idParam())
	if rot.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rot.Code, rot.Body)
	}

	rec := push("?key_epoch=0", "b")
	var doc struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); rec.Code != http.StatusConflict || err != nil || doc.Code != CodeChangeKeyEpochMismatch {
		t.Fatalf("stale push: %d %s, want 409 %s", rec.Code, rec.Body, CodeChangeKeyEpochMismatch)
	}
	if rec := push("?key_epoch=0", "a"); rec.Code != http.StatusCreated {
		t.Fatalf("re-send of a held change: %d %s, want 201", rec.Code, rec.Body)
	}
	if rec := push("?key_epoch=1", "c"); rec.Code != http.StatusCreated {
		t.Fatalf("push at epoch 1: %d %s", rec.Code, rec.Body)
	}
	if rec := push("", "d"); rec.Code != http.StatusCreated {
		t.Fatalf("unconditional push: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []string{"?key_epoch=-1", "?key_epoch=one", "?key_epoch=1.0", "?key_epoch=", "?key_epoch", "?key_epoch=1&key_epoch=1"} {
		if rec := push(bad, "e"); rec.Code != http.StatusBadRequest {
			t.Fatalf("push %s: %d %s, want 400", bad, rec.Code, rec.Body)
		}
	}
}
