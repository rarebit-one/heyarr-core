package cruciform

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/encryption"
	"github.com/rarebit-one/void-which-binds-go/relay"
)

// TestOffloadOverRealRelay drives the whole offload backend over a REAL void-which-binds
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

// TestRelayTransportWakeIsBestEffort: a failed wake does not abort the unwrap
// (#688). The phone, already awake on the relay, still answers, the unwrap
// succeeds, and WakeFailed hears about the failure once.
func TestRelayTransportWakeIsBestEffort(t *testing.T) {
	p := newPhone(t)
	desktopPub, desktopPriv := desktopKeys(t)
	canary := randBytes(t, 48)
	wrapped, canaryCT := wrapForPhone(t, p, canary)

	types := append(append([]string{}, relay.DefaultTypes...), RelayUnwrapTypes...)
	relaySrv := httptest.NewServer(relay.NewServer(relay.Options{Types: types}).Routes())
	t.Cleanup(relaySrv.Close)
	answer := p.honestRoundTrip(desktopPub)

	sessions := make(chan string, 1)
	var reported []error
	wakeErr := errors.New("cruciform wake: node refused (503): plane down")
	transport := &RelayTransport{
		RelayBase: relaySrv.URL,
		Poll:      5 * time.Millisecond,
		Wake: func(_ context.Context, _ string, session string) error {
			sessions <- session // the phone is reachable another way
			return wakeErr
		},
		WakeFailed: func(err error) { reported = append(reported, err) },
	}
	go func() {
		session := <-sessions
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
		t.Fatalf("a failed wake aborted the unwrap: %v", err)
	}
	if got, err := encryption.DecryptChange(key, canaryCT); err != nil || !bytes.Equal(got, canary) {
		t.Fatalf("the recovered key did not decrypt the canary: %v", err)
	}
	if len(reported) != 1 || !errors.Is(reported[0], wakeErr) {
		t.Fatalf("WakeFailed got %v, want the wake error once", reported)
	}
}

// TestRelayTransportStalledWakeDoesNotSpendTheDeadline: a wake that stalls
// (a node or plane that hangs rather than failing) runs beside the poll, so a
// phone answering meanwhile still completes the round-trip, and the stalled
// wake is cancelled, not reported as a failure (Codex on #739).
func TestRelayTransportStalledWakeDoesNotSpendTheDeadline(t *testing.T) {
	types := append(append([]string{}, relay.DefaultTypes...), RelayUnwrapTypes...)
	relaySrv := httptest.NewServer(relay.NewServer(relay.Options{Types: types}).Routes())
	t.Cleanup(relaySrv.Close)

	sessions := make(chan string, 1)
	stalled := make(chan struct{})
	var reported []error
	transport := &RelayTransport{
		RelayBase: relaySrv.URL,
		Poll:      5 * time.Millisecond,
		Wake: func(ctx context.Context, _ string, session string) error {
			sessions <- session
			<-ctx.Done() // hangs until the round-trip cancels it
			close(stalled)
			return ctx.Err()
		},
		WakeFailed: func(err error) { reported = append(reported, err) },
	}
	go func() {
		session := <-sessions
		responder := &relay.Client{Base: relaySrv.URL, Session: session, Role: "responder", PollInterval: 5 * time.Millisecond}
		if _, err := responder.Fetch(context.Background(), RelayRequestType); err != nil {
			return
		}
		_ = responder.Post(context.Background(), RelayResponseType, []byte("reply"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := transport.RoundTrip(ctx, []byte("request"))
	if err != nil {
		t.Fatalf("a stalled wake blocked the reply: %v", err)
	}
	if string(resp) != "reply" {
		t.Fatalf("got %q, want the phone's reply", resp)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the round-trip took %s: the stalled wake spent the deadline", d)
	}
	select {
	case <-stalled:
	default:
		t.Fatal("RoundTrip returned before cancelling and awaiting the wake")
	}
	if len(reported) != 0 {
		t.Fatalf("our own cancellation was reported as a failed wake: %v", reported)
	}
}

// TestRelayTransportFailedWakeNamedOnTimeout: with no phone answering, the
// round-trip fails only at its deadline, and the error names the failed wake.
func TestRelayTransportFailedWakeNamedOnTimeout(t *testing.T) {
	types := append(append([]string{}, relay.DefaultTypes...), RelayUnwrapTypes...)
	relaySrv := httptest.NewServer(relay.NewServer(relay.Options{Types: types}).Routes())
	t.Cleanup(relaySrv.Close)

	transport := &RelayTransport{
		RelayBase: relaySrv.URL,
		Poll:      5 * time.Millisecond,
		Wake: func(context.Context, string, string) error {
			return errors.New("cruciform wake: the node refused this device's credentials (401)")
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := transport.RoundTrip(ctx, []byte("request"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the deadline", err)
	}
	if !strings.Contains(err.Error(), "failed wake") || !strings.Contains(err.Error(), "(401)") {
		t.Fatalf("the timeout does not name the failed wake: %v", err)
	}
}
