package personalstate

import (
	"context"
	"net/http"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

type fakeAuthorizer struct{ allowed map[string]bool }

func (f fakeAuthorizer) AllowedWrapRecipients(context.Context) (map[string]bool, error) {
	return f.allowed, nil
}

func apiWithAuthorizer(t *testing.T, allowed map[string]bool) (*API, *store.Store) {
	t.Helper()
	db := newDB(t)
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(store.Options{Writer: db.Writer(), Reader: db.Reader(), Events: log})
	if err != nil {
		t.Fatal(err)
	}
	api, err := New(Options{Store: st, Authorizer: fakeAuthorizer{allowed: allowed}})
	if err != nil {
		t.Fatal(err)
	}
	return api, st
}

const (
	enrolledKey = "x25519:1111111111111111111111111111111111111111111111111111111111111111"
	strangerKey = "x25519:2222222222222222222222222222222222222222222222222222222222222222"
)

// TestCreateSpaceEnforcesEnrolBeforeWrap: a space key may be wrapped only for a
// pinned recipient (ADR-0049). An enrolled recipient is accepted; an unenrolled
// one is refused with 403 and leaves no orphan space behind.
func TestCreateSpaceEnforcesEnrolBeforeWrap(t *testing.T) {
	api, st := apiWithAuthorizer(t, map[string]bool{enrolledKey: true})

	ok := call(t, api.createSpace, http.MethodPost, "/spaces", createSpaceRequest{
		ID: mustUUID(t), Kind: "personal",
		WrappedKeys: []wrappedKeyInput{{Recipient: enrolledKey, Wrapped: []byte("wrapped")}},
	}, nil)
	if ok.Code != http.StatusCreated {
		t.Fatalf("an enrolled recipient should be accepted, got %d: %s", ok.Code, ok.Body.String())
	}

	badID := mustUUID(t)
	bad := call(t, api.createSpace, http.MethodPost, "/spaces", createSpaceRequest{
		ID: badID, Kind: "personal",
		WrappedKeys: []wrappedKeyInput{{Recipient: strangerKey, Wrapped: []byte("wrapped")}},
	}, nil)
	if bad.Code != http.StatusForbidden {
		t.Fatalf("an unenrolled recipient must be refused with 403, got %d: %s", bad.Code, bad.Body.String())
	}

	// The refused create must not have left a space behind (the check runs before
	// the space is recorded).
	list, err := st.ListSpaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range list {
		if sp.ID == badID {
			t.Fatal("a refused create left an orphan space")
		}
	}
}

// TestRewrapEnforcesEnrolBeforeWrap: the rewrap path is guarded too, so a
// rotation cannot smuggle in an unenrolled recipient.
func TestRewrapEnforcesEnrolBeforeWrap(t *testing.T) {
	api, _ := apiWithAuthorizer(t, map[string]bool{enrolledKey: true})

	id := mustUUID(t)
	create := call(t, api.createSpace, http.MethodPost, "/spaces", createSpaceRequest{
		ID: id, Kind: "personal",
		WrappedKeys: []wrappedKeyInput{{Recipient: enrolledKey, Wrapped: []byte("wrapped")}},
	}, nil)
	if create.Code != http.StatusCreated {
		t.Fatalf("setup create failed: %d: %s", create.Code, create.Body.String())
	}

	bad := call(t, api.rewrapKeys, http.MethodPost, "/spaces/"+id+"/keys", rewrapRequest{
		WrappedKeys: []wrappedKeyInput{{Recipient: strangerKey, Wrapped: []byte("wrapped")}},
	}, map[string]string{"id": id})
	if bad.Code != http.StatusForbidden {
		t.Fatalf("re-wrapping for an unenrolled recipient must be refused with 403, got %d: %s", bad.Code, bad.Body.String())
	}
}

// TestNoAuthorizerLeavesTheCheckOff documents that a nil authorizer is the
// pre-M9 behaviour — any recipient is accepted — so the check is opt-in via wiring.
func TestNoAuthorizerLeavesTheCheckOff(t *testing.T) {
	api := newAPI(t) // built with no authorizer
	rec := call(t, api.createSpace, http.MethodPost, "/spaces", createSpaceRequest{
		ID: mustUUID(t), Kind: "personal",
		WrappedKeys: []wrappedKeyInput{{Recipient: strangerKey, Wrapped: []byte("wrapped")}},
	}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("with no authorizer any recipient is accepted, got %d: %s", rec.Code, rec.Body.String())
	}
}
