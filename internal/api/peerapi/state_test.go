package peerapi_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
)

// stubState is an in-memory StateStore for the route's own behaviour. It holds
// OPAQUE changes and never decrypts one — exactly what the peer surface is
// allowed to do. (It cannot import the encryption or CRDT packages any more than
// the surface can: the depguard boundary applies to this _test file too, which is
// why the "ciphertext" here is an opaque byte slice, not real AEAD output — the
// route's job is to move bytes verbatim, and the boundary test proves it has no
// path to read them.)
type stubState struct {
	changes   map[string][]protocol.EncryptedChange
	unknown   map[string]bool
	got       []protocol.EncryptedChange
	gotSpaces map[string]string // space id -> kind
	gotKeys   map[string]string // space id -> recipient (last)
	badKind   string            // a kind PutSpace rejects as invalid
}

func (s *stubState) HeadsFor(_ context.Context, spaceID string) ([]string, error) {
	if s.unknown[spaceID] {
		return nil, peerapi.ErrNoSuchSpace
	}
	return protocol.Heads(s.changes[spaceID]), nil
}

func (s *stubState) ChangesFor(_ context.Context, spaceID string) ([]protocol.EncryptedChange, error) {
	if s.unknown[spaceID] {
		return nil, peerapi.ErrNoSuchSpace
	}
	return s.changes[spaceID], nil
}

func (s *stubState) PutChange(_ context.Context, ch protocol.EncryptedChange) error {
	if s.unknown[ch.SpaceID] {
		return peerapi.ErrNoSuchSpace
	}
	if err := ch.Validate(); err != nil {
		return err
	}
	s.got = append(s.got, ch)
	return nil
}

func (s *stubState) PutSpace(_ context.Context, spaceID, kind string) error {
	if s.badKind != "" && kind == s.badKind {
		return peerapi.ErrInvalidState
	}
	if s.gotSpaces == nil {
		s.gotSpaces = map[string]string{}
	}
	s.gotSpaces[spaceID] = kind
	return nil
}

func (s *stubState) PutWrappedKey(_ context.Context, spaceID, recipient string, wrapped []byte) error {
	if s.unknown[spaceID] {
		return peerapi.ErrNoSuchSpace
	}
	if recipient == "" || len(wrapped) == 0 {
		return peerapi.ErrInvalidState
	}
	if s.gotKeys == nil {
		s.gotKeys = map[string]string{}
	}
	s.gotKeys[spaceID] = recipient
	return nil
}

// serveState constructs a peer surface with a State backend, mirroring
// serveLeases.
func serveState(t *testing.T, self *peerNode, members mtls.Membership, src peerapi.StateStore) *listener {
	t.Helper()
	logs := &syncBuffer{}
	srv, err := peerapi.New(peerapi.Options{
		Addr:       "127.0.0.1:0",
		Material:   self.material,
		Members:    members,
		SelfPeerID: self.peerID,
		State:      src,
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return &listener{srv: srv, self: self, addr: srv.Addr(), logs: logs}
}

func stateURL(l *listener, space, suffix string) string {
	return "https://" + l.addr + peerapi.Prefix + "/state/" + space + suffix
}

// mustChange mints an opaque change for a space, over the given parents.
func mustChange(t *testing.T, space string, parents []string, ciphertext []byte) protocol.EncryptedChange {
	t.Helper()
	ch, err := protocol.NewChange(space, parents, ciphertext)
	if err != nil {
		t.Fatalf("minting a change: %v", err)
	}
	return ch
}

// TestStateRouteServesOpaqueChangesToAMember: a member offers its heads and pulls
// the changes it is missing, as ciphertext moved verbatim, and pushes a new one.
func TestStateRouteServesOpaqueChangesToAMember(t *testing.T) {
	const space = "0199aaaa-0000-7000-8000-00000000cafe"
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())

	opaque := []byte("OPAQUE-CIPHERTEXT-not-a-playlist-name")
	chA := mustChange(t, space, nil, opaque)
	st := &stubState{changes: map[string][]protocol.EncryptedChange{space: {chA}}}
	l := serveState(t, a, root, st)
	client := dialler(t, b, root)

	// Heads.
	status, body, _, err := peerGet(t, client, stateURL(l, space, "/heads"))
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET heads: status=%d err=%v\n%s", status, err, body)
	}
	var heads struct {
		Heads []string `json:"heads"`
	}
	if err := json.Unmarshal([]byte(body), &heads); err != nil {
		t.Fatal(err)
	}
	if len(heads.Heads) != 1 || heads.Heads[0] != chA.ChangeID {
		t.Fatalf("heads = %v, want [%s]", heads.Heads, chA.ChangeID)
	}

	// Changes, served verbatim as ciphertext.
	status, body, _, err = peerGet(t, client, stateURL(l, space, "/changes"))
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET changes: status=%d err=%v\n%s", status, err, body)
	}
	var served struct {
		Changes []protocol.EncryptedChange `json:"changes"`
	}
	if err := json.Unmarshal([]byte(body), &served); err != nil {
		t.Fatal(err)
	}
	if len(served.Changes) != 1 {
		t.Fatalf("served %d changes, want 1", len(served.Changes))
	}
	if !bytes.Equal(served.Changes[0].Ciphertext, opaque) {
		t.Fatalf("the peer altered the change bytes in transit: got %q", served.Changes[0].Ciphertext)
	}

	// Push a new change; the peer accepts it after verifying its id.
	chB := mustChange(t, space, []string{chA.ChangeID}, []byte("OPAQUE-CIPHERTEXT-second"))
	payload, err := json.Marshal(chB)
	if err != nil {
		t.Fatal(err)
	}
	status, body, _, err = peerSend(t, client, http.MethodPost, stateURL(l, space, "/changes"), string(payload))
	if err != nil || status != http.StatusOK {
		t.Fatalf("POST change: status=%d err=%v\n%s", status, err, body)
	}
	if len(st.got) != 1 || st.got[0].ChangeID != chB.ChangeID {
		t.Fatalf("peer did not store the pushed change: %+v", st.got)
	}
}

// TestStateChangesMissingFiltersByHave: offering a head suppresses the changes it
// already covers — the incremental pull (protocol.Missing), computed without
// decryption.
func TestStateChangesMissingFiltersByHave(t *testing.T) {
	const space = "0199bbbb-0000-7000-8000-00000000beef"
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())

	chA := mustChange(t, space, nil, []byte("OPAQUE-A"))
	chB := mustChange(t, space, []string{chA.ChangeID}, []byte("OPAQUE-B"))
	st := &stubState{changes: map[string][]protocol.EncryptedChange{space: {chA, chB}}}
	l := serveState(t, a, root, st)
	client := dialler(t, b, root)

	// have=chB → the caller holds B and (transitively) A → nothing missing.
	status, body, _, err := peerGet(t, client, stateURL(l, space, "/changes?have="+chB.ChangeID))
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET changes?have=B: status=%d err=%v", status, err)
	}
	var caughtUp struct {
		Changes []protocol.EncryptedChange `json:"changes"`
	}
	if err := json.Unmarshal([]byte(body), &caughtUp); err != nil {
		t.Fatal(err)
	}
	if len(caughtUp.Changes) != 0 {
		t.Fatalf("a caller holding the head should be missing nothing, got %d", len(caughtUp.Changes))
	}

	// have=chA → the caller holds A but not B → B is missing.
	status, body, _, err = peerGet(t, client, stateURL(l, space, "/changes?have="+chA.ChangeID))
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET changes?have=A: status=%d err=%v", status, err)
	}
	var behind struct {
		Changes []protocol.EncryptedChange `json:"changes"`
	}
	if err := json.Unmarshal([]byte(body), &behind); err != nil {
		t.Fatal(err)
	}
	if len(behind.Changes) != 1 || behind.Changes[0].ChangeID != chB.ChangeID {
		t.Fatalf("a caller holding only A should be missing exactly B, got %+v", behind.Changes)
	}
}

// TestStatePushRejectsForgedID: a change whose stated id does not match its bytes
// is a 400, refused before storage (Invariant 1).
func TestStatePushRejectsForgedID(t *testing.T) {
	const space = "0199cccc-0000-7000-8000-00000000d00d"
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	st := &stubState{changes: map[string][]protocol.EncryptedChange{space: nil}}
	l := serveState(t, a, root, st)

	forged := protocol.EncryptedChange{
		SpaceID:    space,
		ChangeID:   "blake3:deadbeef",
		Ciphertext: []byte("OPAQUE-but-the-id-is-a-lie-and-long-enough"),
	}
	payload, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	status, _, _, err := peerSend(t, dialler(t, b, root), http.MethodPost, stateURL(l, space, "/changes"), string(payload))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("a forged change id should be a 400, got %d", status)
	}
	if len(st.got) != 0 {
		t.Fatal("a forged change reached the store — it must be refused before storage")
	}
}

// TestStateAnswers503WithNoBackend: a node with no personal-state store still
// MOUNTS the routes (so the OpenAPI parity walk sees them) and answers 503.
func TestStateAnswers503WithNoBackend(t *testing.T) {
	const space = "0199dddd-0000-7000-8000-00000000face"
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	l := serve(t, a, root) // no State backend wired

	status, _, _, err := peerGet(t, dialler(t, b, root), stateURL(l, space, "/heads"))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("a node that keeps no personal state should answer 503, got %d", status)
	}
}

// TestStateUnknownSpaceIs404: a space this peer has not been replicated is a 404,
// not a 500 — the not-found the wiring adapter translates.
func TestStateUnknownSpaceIs404(t *testing.T) {
	const space = "0199eeee-0000-7000-8000-000000001234"
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	st := &stubState{unknown: map[string]bool{space: true}}
	l := serveState(t, a, root, st)

	status, _, _, err := peerGet(t, dialler(t, b, root), stateURL(l, space, "/changes"))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if status != http.StatusNotFound {
		t.Fatalf("an unknown space should be a 404, got %d", status)
	}
}

// TestANonMemberCannotReachState: a stranger is refused at the mTLS handshake, so
// the state routes are unreachable to a non-member — like every peer route.
func TestANonMemberCannotReachState(t *testing.T) {
	const space = "0199ffff-0000-7000-8000-000000005678"
	a := newPeerNode(t, "peer-a-id", "peer-a")
	stranger := newPeerNode(t, "stranger-id", "stranger")
	root := newTrustRoot(a.member()) // stranger is NOT enrolled
	st := &stubState{changes: map[string][]protocol.EncryptedChange{space: nil}}
	l := serveState(t, a, root, st)

	if _, _, _, err := peerGet(t, dialler(t, stranger, root), stateURL(l, space, "/heads")); err == nil {
		t.Fatal("a non-member reached the state route; the mTLS handshake should have refused it")
	}
}

// TestStatePutSpaceAndWrappedKey: a sibling replicates a space's identity and a
// wrapped key to this peer over the metadata routes; both are stored (204), and a
// malformed push (bad kind, empty recipient) is a 400.
func TestStatePutSpaceAndWrappedKey(t *testing.T) {
	const space = "0199a0a0-0000-7000-8000-0000000000aa"
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	st := &stubState{badKind: "nonsense"}
	l := serveState(t, a, root, st)
	client := dialler(t, b, root)

	// Replicate the space.
	status, body, _, err := peerSend(t, client, http.MethodPost, stateURL(l, space, ""), `{"kind":"personal"}`)
	if err != nil || status != http.StatusNoContent {
		t.Fatalf("POST space: status=%d err=%v body=%s", status, err, body)
	}
	if st.gotSpaces[space] != "personal" {
		t.Fatalf("the space was not recorded: %+v", st.gotSpaces)
	}

	// A bad kind is a 400.
	status, _, _, err = peerSend(t, client, http.MethodPost, stateURL(l, space, ""), `{"kind":"nonsense"}`)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("a bad kind should be a 400, got %d", status)
	}

	// Replicate a wrapped key.
	keyBody := `{"recipient":"x25519:` + repeatHexTest("cc", 32) + `","wrapped":"` + base64Std("OPAQUE-WRAP") + `"}`
	status, body, _, err = peerSend(t, client, http.MethodPost, stateURL(l, space, "/keys"), keyBody)
	if err != nil || status != http.StatusNoContent {
		t.Fatalf("POST wrapped key: status=%d err=%v body=%s", status, err, body)
	}
	if st.gotKeys[space] == "" {
		t.Fatalf("the wrapped key was not recorded: %+v", st.gotKeys)
	}

	// An empty recipient is a 400.
	status, _, _, err = peerSend(t, client, http.MethodPost, stateURL(l, space, "/keys"), `{"recipient":"","wrapped":"AA=="}`)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("an empty recipient should be a 400, got %d", status)
	}
}

func repeatHexTest(unit string, n int) string {
	out := ""
	for len(out) < n*2 {
		out += unit
	}
	return out[:n*2]
}

func base64Std(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
