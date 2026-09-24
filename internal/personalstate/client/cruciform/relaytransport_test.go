package cruciform

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rarebit-one/voidbind-go/encryption"
	"github.com/rarebit-one/voidbind-go/relay"
)

// TestOffloadOverRealRelay drives the whole offload backend over a REAL voidbind
// relay (Server + Client) against a fake phone that answers by polling the relay —
// the live path minus the actual device. It proves the RelayTransport carries the
// signed request out and the sealed reply back, and that the recovered space key
// decrypts a canary. The relay is stood up with the offload message types
// (RelayUnwrapTypes) alongside the pairing defaults.
func TestOffloadOverRealRelay(t *testing.T) {
	p := newPhone(t)
	desktopPub, desktopPriv := desktopKeys(t)
	canary := randBytes(t, 48)
	wrapped, canaryCT := wrapForPhone(t, p, canary)

	types := append(append([]string{}, relay.DefaultTypes...), RelayUnwrapTypes...)
	relaySrv := httptest.NewServer(relay.NewServer(relay.Options{Types: types}).Routes())
	t.Cleanup(relaySrv.Close)

	// The reference phone's honest answer, reused verbatim — here fed the request
	// fetched off the relay, its reply posted back.
	answer := p.honestRoundTrip(desktopPub)

	woken := make(chan string, 1)
	transport := &RelayTransport{
		RelayBase: relaySrv.URL,
		Poll:      5 * time.Millisecond,
		Wake: func(_ context.Context, _ string, session string) error {
			woken <- session
			return nil
		},
	}

	// The fake phone: woken with the session, it polls the relay for the request,
	// answers, and posts the reply — exactly the responder side of the exchange.
	go func() {
		session := <-woken
		responder := &relay.Client{Base: relaySrv.URL, Session: session, Role: "responder", PollInterval: 5 * time.Millisecond}
		req, err := responder.Fetch(context.Background(), RelayRequestType)
		if err != nil {
			return
		}
		resp, err := answer(context.Background(), req)
		if err != nil {
			return
		}
		_ = responder.Post(context.Background(), RelayResponseType, resp)
	}()

	u, err := New(transport, desktopPriv, p.sign.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	key, err := u.Unwrap(wrapped)
	if err != nil {
		t.Fatalf("Unwrap over the relay: %v", err)
	}
	got, err := encryption.DecryptChange(key, canaryCT)
	if err != nil {
		t.Fatalf("decrypt canary with the relay-recovered key: %v", err)
	}
	if !bytes.Equal(got, canary) {
		t.Fatal("the relay-recovered key did not decrypt the canary")
	}
}

// TestRelayTransportSurfacesWakeError: a wake failure aborts the round-trip rather
// than hanging on a phone that will never answer.
func TestRelayTransportSurfacesWakeError(t *testing.T) {
	types := append(append([]string{}, relay.DefaultTypes...), RelayUnwrapTypes...)
	relaySrv := httptest.NewServer(relay.NewServer(relay.Options{Types: types}).Routes())
	t.Cleanup(relaySrv.Close)

	p := newPhone(t)
	_, desktopPriv := desktopKeys(t)
	wrapped, _ := wrapForPhone(t, p, randBytes(t, 8))

	transport := &RelayTransport{
		RelayBase: relaySrv.URL,
		Wake:      func(context.Context, string, string) error { return context.DeadlineExceeded },
	}
	u, err := New(transport, desktopPriv, p.sign.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Unwrap(wrapped); err == nil {
		t.Fatal("a wake failure should abort the unwrap")
	}
}
