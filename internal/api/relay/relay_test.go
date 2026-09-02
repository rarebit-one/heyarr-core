package relay_test

import (
	"context"
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rarebit-one/voidbind-go/enrolment"
	"github.com/rarebit-one/voidbind-go/pairflow"
	"github.com/rarebit-one/voidbind-go/pairing"
	vbrelay "github.com/rarebit-one/voidbind-go/relay"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/relay"
	"github.com/rarebit-one/heyarr-core/internal/pairrelay"
)

// newNode mounts BOTH relays on one public router, the way the controller does,
// so the tests prove they coexist rather than that each works alone.
func newNode(t *testing.T) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	relay.New(relay.Options{}).Mount(r)
	pairrelay.NewHandler(pairrelay.HandlerOptions{}).Mount(r)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts
}

// TestVoidbindPairflowCompletesThroughTheNode is the acceptance: a voidbind-go
// initiator (the machine holding the user identity) and responder (the phone)
// pair through the node's /pair/v1 with voidbind-go's OWN relay client — the
// exact client code path voidbind-kmp mirrors — and the responder ends up with
// a cert the user key verifies. Nothing in this test touches heyarr's legacy
// pairflow: if the wire contract drifted from what the clients speak, the
// handshake fails here.
func TestVoidbindPairflowCompletesThroughTheNode(t *testing.T) {
	ts := newNode(t)
	// A Voidbind client's relay BASE is the node's /pair: voidbind-go's client
	// appends the /v1/... paths itself, which is how the routes land under
	// httpapi.RelayV1Prefix. Handing a client "<node>/pair/v1" would dial
	// /pair/v1/v1/sessions and 404 — the mistake this line exists to document.
	base := ts.URL + httpapi.RelayPrefix
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	session, err := vbrelay.CreateSession(ctx, ts.Client(), base)
	if err != nil {
		t.Fatalf("create session through the node: %v", err)
	}
	salt, err := pairing.NewSalt()
	if err != nil {
		t.Fatal(err)
	}
	// The invite is what the initiator shows the phone as a QR: it must carry the
	// node's /pair/v1 base intact so the phone dials the node, not a standalone relay.
	invite, err := pairflow.EncodeInvite(base, session, salt)
	if err != nil {
		t.Fatal(err)
	}
	gotBase, gotSession, gotSalt, err := pairflow.DecodeInvite(invite)
	if err != nil || gotBase != base || gotSession != session || string(gotSalt) != string(salt) {
		t.Fatalf("invite round trip: base=%q session=%q err=%v", gotBase, gotSession, err)
	}

	_, userPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	in, err := pairflow.NewInitiator(userPriv, salt, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := pairflow.NewResponder(gotSalt, now)
	if err != nil {
		t.Fatal(err)
	}
	poll := 10 * time.Millisecond
	initT := &vbrelay.Client{Base: base, Session: session, Role: string(pairflow.RoleInitiator), HTTP: ts.Client(), PollInterval: poll}
	respT := &vbrelay.Client{Base: gotBase, Session: gotSession, Role: string(pairflow.RoleResponder), HTTP: ts.Client(), PollInterval: poll}

	type sasResult struct {
		sas pairing.SAS
		err error
	}
	respSAS := make(chan sasResult, 1)
	go func() {
		s, err := resp.Handshake(ctx, respT)
		respSAS <- sasResult{s, err}
	}()
	initSAS, err := in.Handshake(ctx, initT)
	if err != nil {
		t.Fatalf("initiator handshake: %v", err)
	}
	rs := <-respSAS
	if rs.err != nil {
		t.Fatalf("responder handshake: %v", rs.err)
	}
	if initSAS != rs.sas {
		t.Fatalf("the two sides derived different SAS: %s vs %s", initSAS, rs.sas)
	}

	// The humans compared the strings; the initiator signs and seals, the
	// responder unseals and verifies.
	certCh := make(chan struct {
		cert string
		err  error
	}, 1)
	go func() {
		c, err := resp.Receive(ctx, respT)
		certCh <- struct {
			cert string
			err  error
		}{c, err}
	}()
	if err := in.Authorise(ctx, initT); err != nil {
		t.Fatalf("initiator authorise: %v", err)
	}
	got := <-certCh
	if got.err != nil {
		t.Fatalf("responder receive: %v", got.err)
	}
	cert, err := enrolment.VerifyCert(got.cert, userPriv.Public().(ed25519.PublicKey), now)
	if err != nil {
		t.Fatalf("the received cert does not verify against the user key: %v", err)
	}
	if cert.Device != resp.DeviceID() || cert.DeviceEnc != resp.DeviceEncID() {
		t.Fatalf("cert binds %s/%s, responder is %s/%s", cert.Device, cert.DeviceEnc, resp.DeviceID(), resp.DeviceEncID())
	}
	// And the cert is a possession-proof-able device credential — the Device
	// scheme's exact input — so what pairing produced is what /enrol consumes.
	if _, err := enrolment.SignPossession(resp.Signer(), got.cert, now, 0); err != nil {
		t.Fatalf("sign possession over the received cert: %v", err)
	}

	// The relay held only ciphertext for the cert: read the slot back as a third
	// party would and confirm the token is not in it.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+httpapi.RelayV1Prefix+"/sessions/"+session+"/initiator/cert", nil)
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	slot, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("reading the cert slot: %d", res.StatusCode)
	}
	if strings.Contains(string(slot), got.cert) {
		t.Fatal("the relay held the cert token in plaintext")
	}
}

// TestLegacyRelayStillServesBesideIt: the two protocols have disjoint paths and
// both answer on one router — `heyarr pair` keeps working after the mount.
func TestLegacyRelayStillServesBesideIt(t *testing.T) {
	ts := newNode(t)
	ctx := context.Background()
	put := func(url, body string) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(body))
		res, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		return res.StatusCode
	}
	get := func(url string) (int, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		res, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		return res.StatusCode, string(b)
	}
	legacy := ts.URL + httpapi.RelayPrefix + "/sessions/0123456789abcdef0123456789abcdef/slots/initiator_commit"
	if code := put(legacy, "commitment"); code != http.StatusNoContent {
		t.Fatalf("legacy PUT: %d", code)
	}
	if code, body := get(legacy); code != http.StatusOK || body != "commitment" {
		t.Fatalf("legacy GET: %d %q", code, body)
	}

	// The v1 relay is write-once per slot and refuses an unknown role/type — the
	// contract voidbind-kmp relies on — and does not answer the legacy shape.
	base := ts.URL + httpapi.RelayPrefix
	session, err := vbrelay.CreateSession(ctx, ts.Client(), base)
	if err != nil {
		t.Fatal(err)
	}
	slot := ts.URL + httpapi.RelayV1Prefix + "/sessions/" + session + "/responder/commit"
	if code := put(slot, "c1"); code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("v1 PUT: %d", code)
	}
	if code := put(slot, "c2"); code != http.StatusConflict {
		t.Fatalf("v1 second PUT of a slot: %d, want 409", code)
	}
	if code := put(ts.URL+httpapi.RelayV1Prefix+"/sessions/"+session+"/observer/commit", "x"); code/100 == 2 {
		t.Fatalf("v1 accepted an unknown role")
	}
	if code, _ := get(base + "/sessions/x/slots/cert"); code/100 == 2 {
		t.Fatal("the v1 mount answered a legacy-shaped path")
	}
	// The same per-message cap as the legacy relay.
	big := strings.Repeat("a", pairrelay.MaxSlotBytes+1)
	if code := put(ts.URL+httpapi.RelayV1Prefix+"/sessions/"+session+"/responder/reveal", big); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("v1 oversized PUT: %d, want 413", code)
	}
}
