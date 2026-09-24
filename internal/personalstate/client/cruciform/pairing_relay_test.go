package cruciform

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rarebit-one/voidbind-go/pairing"
	"github.com/rarebit-one/voidbind-go/relay"
)

// TestOfflinePairOverRealRelay drives the whole pairing ceremony over a REAL
// voidbind relay (Server + Client) against the reference fake phone — the live
// pairing path minus the actual device and its QR scan. It proves the desktop
// half posts commit/reveal/confirm and reads the phone's over the real relay
// wire, and that both sides derive the same SAS and pin each other's keys. The
// relay is stood up with the offload pairing types (RelayPairTypes) alongside the
// pairing defaults — the same allow-list the node's relay mount must carry.
func TestOfflinePairOverRealRelay(t *testing.T) {
	types := append(append([]string{}, relay.DefaultTypes...), RelayPairTypes...)
	relaySrv := httptest.NewServer(relay.NewServer(relay.Options{Types: types}).Routes())
	t.Cleanup(relaySrv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	session, err := relay.CreateSession(ctx, nil, relaySrv.URL)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	salt, err := pairing.NewSalt()
	if err != nil {
		t.Fatalf("salt: %v", err)
	}
	_, tk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("transport key: %v", err)
	}

	in, err := NewInitiator(tk, relaySrv.URL, session, salt)
	if err != nil {
		t.Fatalf("NewInitiator: %v", err)
	}

	phone := newPairPhone(t)
	phone.session, phone.salt = session, salt

	go func() {
		responder := &relay.Client{Base: relaySrv.URL, Session: session, Role: "responder", PollInterval: 5 * time.Millisecond}
		_ = phone.run(ctx, responder)
	}()

	desktop := &relay.Client{Base: relaySrv.URL, Session: session, Role: "initiator", PollInterval: 5 * time.Millisecond}
	sas, err := in.Handshake(ctx, desktop)
	if err != nil {
		t.Fatalf("Handshake over the relay: %v", err)
	}
	cfg, err := in.Confirm(ctx, desktop)
	if err != nil {
		t.Fatalf("Confirm over the relay: %v", err)
	}
	if sas != phone.sas {
		t.Fatalf("SAS mismatch over the relay: desktop %q, phone %q", sas, phone.sas)
	}
	if !cfg.PhonePub.Equal(phone.sign.Public().(ed25519.PublicKey)) {
		t.Fatal("desktop pinned the wrong phone device key over the relay")
	}
	if !phone.learnedTransport.Equal(tk.Public().(ed25519.PublicKey)) {
		t.Fatal("phone pinned the wrong transport key over the relay")
	}
}
