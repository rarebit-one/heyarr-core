package cruciform

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/voidbind-go/encryption"
	"github.com/rarebit-one/voidbind-go/pairing"

	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// memRelay is an in-memory two-role relay for the pairing handshake: each
// (role, msgType) slot is written once and read once, so a buffered channel per
// slot models "Fetch blocks until the peer Posts" without a real relay.
type memRelay struct {
	mu    sync.Mutex
	slots map[string]chan []byte
}

func newMemRelay() *memRelay { return &memRelay{slots: map[string]chan []byte{}} }

func (m *memRelay) ch(key string) chan []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.slots[key] == nil {
		m.slots[key] = make(chan []byte, 1)
	}
	return m.slots[key]
}

// side is one participant's PairTransport: it Posts to its own role's slots and
// Fetches the peer's.
type side struct {
	relay *memRelay
	me    string
	peer  string
}

func (s *side) Post(_ context.Context, msgType string, payload []byte) error {
	s.relay.ch(s.me + ":" + msgType) <- append([]byte(nil), payload...)
	return nil
}

func (s *side) Fetch(ctx context.Context, msgType string) ([]byte, error) {
	select {
	case b := <-s.relay.ch(s.peer + ":" + msgType):
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// pairPhone is a reference responder that runs the mirror of the desktop
// handshake against the real primitives, so the desktop half is exercised end to
// end. It records the transport key it learned and the SAS it derived so a test
// can assert both sides agree.
type pairPhone struct {
	sign    ed25519.PrivateKey
	enc     *ecdh.PrivateKey
	session string
	salt    []byte

	// options for the adversarial tests
	tamperRevealEnc bool               // reveal an enc key different from the committed one
	confirmWith     ed25519.PrivateKey // sign the confirm with this key instead of the device key

	learnedTransport ed25519.PublicKey
	sas              pairing.SAS
}

func (p *pairPhone) run(ctx context.Context, t PairTransport) error {
	phonePub := p.sign.Public().(ed25519.PublicKey)
	phoneEnc := p.enc.PublicKey().Bytes()

	commit, err := pairing.Commit(phonePub, phoneEnc)
	if err != nil {
		return err
	}
	if err := t.Post(ctx, pairMsgCommit, commit); err != nil {
		return err
	}
	peerCommit, err := t.Fetch(ctx, pairMsgCommit)
	if err != nil {
		return err
	}

	revealEnc := encryption.FormatPublicKey(phoneEnc)
	if p.tamperRevealEnc {
		other, _ := encryption.GenerateKey()
		revealEnc = encryption.FormatPublicKey(other.PublicKey().Bytes())
	}
	mine, err := json.Marshal(pairReveal{Sign: identity.FormatPublicKey(phonePub), Enc: revealEnc})
	if err != nil {
		return err
	}
	if err := t.Post(ctx, pairMsgReveal, mine); err != nil {
		return err
	}
	peerRaw, err := t.Fetch(ctx, pairMsgReveal)
	if err != nil {
		return err
	}
	var peer pairReveal
	if err := json.Unmarshal(peerRaw, &peer); err != nil {
		return err
	}
	transportPub, err := identity.ParsePublicKey(peer.Sign)
	if err != nil {
		return err
	}
	if err := pairing.Commitment(peerCommit).Open(transportPub, nil); err != nil {
		return err
	}
	p.learnedTransport = transportPub
	p.sas, err = pairing.Derive(
		pairing.Keys{Sign: transportPub},
		pairing.Keys{Sign: phonePub, Enc: phoneEnc},
		p.salt,
	)
	if err != nil {
		return err
	}

	transcript := pairTranscript(p.session, p.salt, transportPub, nil, phonePub, phoneEnc)
	signer := p.sign
	if p.confirmWith != nil {
		signer = p.confirmWith
	}
	sig := ed25519.Sign(signer, transcript)
	payload, err := json.Marshal(pairConfirm{Sig: base64.RawURLEncoding.EncodeToString(sig)})
	if err != nil {
		return err
	}
	if err := t.Post(ctx, pairMsgConfirm, payload); err != nil {
		return err
	}
	// Drain the desktop's confirm so the round-trip completes symmetrically.
	if _, err := t.Fetch(ctx, pairMsgConfirm); err != nil {
		return err
	}
	return nil
}

func hexSalt(salt []byte) string { return hex.EncodeToString(salt) }

func writeFile(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }

func newPairPhone(t *testing.T) *pairPhone {
	t.Helper()
	_, sign, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("phone signing key: %v", err)
	}
	enc, err := encryption.GenerateKey()
	if err != nil {
		t.Fatalf("phone encryption key: %v", err)
	}
	return &pairPhone{sign: sign, enc: enc}
}

func newDesktop(t *testing.T, relayBase, session string, salt []byte) (*Initiator, ed25519.PrivateKey) {
	t.Helper()
	_, tk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("transport key: %v", err)
	}
	in, err := NewInitiator(tk, relayBase, session, salt)
	if err != nil {
		t.Fatalf("NewInitiator: %v", err)
	}
	return in, tk
}

func runPair(t *testing.T, phone *pairPhone) (*PairConfig, pairing.SAS, ed25519.PrivateKey, error) {
	t.Helper()
	salt, err := pairing.NewSalt()
	if err != nil {
		t.Fatalf("salt: %v", err)
	}
	const relayBase, session = "https://relay.example", "sess-123"
	phone.session, phone.salt = session, salt
	in, tk := newDesktop(t, relayBase, session, salt)

	relay := newMemRelay()
	desktopSide := &side{relay: relay, me: "initiator", peer: "responder"}
	phoneSide := &side{relay: relay, me: "responder", peer: "initiator"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		wg       sync.WaitGroup
		phoneErr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		phoneErr = phone.run(ctx, phoneSide)
	}()

	sas, err := in.Handshake(ctx, desktopSide)
	if err != nil {
		cancel()
		wg.Wait()
		return nil, "", tk, err
	}
	cfg, err := in.Confirm(ctx, desktopSide)
	wg.Wait()
	if phoneErr != nil && err == nil {
		t.Fatalf("fake phone failed: %v", phoneErr)
	}
	return cfg, sas, tk, err
}

func TestOfflinePairRoundTrip(t *testing.T) {
	phone := newPairPhone(t)
	cfg, sas, tk, err := runPair(t, phone)
	if err != nil {
		t.Fatalf("pairing: %v", err)
	}
	if sas != phone.sas {
		t.Fatalf("SAS mismatch: desktop %q, phone %q", sas, phone.sas)
	}
	if !phone.learnedTransport.Equal(tk.Public().(ed25519.PublicKey)) {
		t.Fatalf("phone pinned the wrong transport key")
	}
	if !cfg.PhonePub.Equal(phone.sign.Public().(ed25519.PublicKey)) {
		t.Fatalf("desktop pinned the wrong phone device key")
	}
	if want := phone.enc.PublicKey().Bytes(); string(cfg.PhoneEnc) != string(want) {
		t.Fatalf("desktop pinned the wrong phone encryption key")
	}
	if cfg.RelayBase != "https://relay.example" {
		t.Fatalf("relay base not pinned: %q", cfg.RelayBase)
	}
	if cfg.RecipientID() != encryption.FormatPublicKey(phone.enc.PublicKey().Bytes()) {
		t.Fatalf("RecipientID is not the phone's encryption key")
	}
}

func TestOfflinePairRejectsSubstitutedPhoneKey(t *testing.T) {
	phone := newPairPhone(t)
	phone.tamperRevealEnc = true // reveal an enc key it did not commit to
	_, _, _, err := runPair(t, phone)
	if err == nil {
		t.Fatal("expected the desktop to refuse a reveal that does not open the commitment")
	}
}

func TestOfflinePairRejectsForgedPhoneConfirm(t *testing.T) {
	phone := newPairPhone(t)
	_, forged, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("forged key: %v", err)
	}
	phone.confirmWith = forged // confirm signed by a key that is not the pinned device key
	_, _, _, err = runPair(t, phone)
	if !errors.Is(err, ErrBadPeerConfirm) {
		t.Fatalf("want ErrBadPeerConfirm, got %v", err)
	}
}

func TestOfflinePairConfirmBeforeHandshake(t *testing.T) {
	salt, _ := pairing.NewSalt()
	in, _ := newDesktop(t, "https://relay.example", "sess", salt)
	relay := newMemRelay()
	desktopSide := &side{relay: relay, me: "initiator", peer: "responder"}
	if _, err := in.Confirm(context.Background(), desktopSide); !errors.Is(err, ErrNotHandshaken) {
		t.Fatalf("want ErrNotHandshaken, got %v", err)
	}
}

func TestInviteRoundTrip(t *testing.T) {
	salt, _ := pairing.NewSalt()
	uri, err := EncodeInvite("https://relay.example", "sess-abc", salt)
	if err != nil {
		t.Fatalf("EncodeInvite: %v", err)
	}
	inv, err := DecodeInvite(uri)
	if err != nil {
		t.Fatalf("DecodeInvite: %v", err)
	}
	if inv.RelayBase != "https://relay.example" || inv.Session != "sess-abc" || string(inv.Salt) != string(salt) {
		t.Fatalf("invite did not round-trip: %+v", inv)
	}
}

func TestDecodeInviteRejectsMalformed(t *testing.T) {
	salt, _ := pairing.NewSalt()
	good, _ := EncodeInvite("https://relay.example", "s", salt)
	for _, tc := range []struct {
		name string
		uri  string
	}{
		{"wrong scheme", "https://relay.example/pair"},
		{"enrolment opaque", "voidbind:pair?v=1&relay=r&session=s&salt=00"},
		{"wrong version", "voidbind:offload-pair?v=99&relay=r&session=s&salt=" + hexSalt(salt)},
		{"short salt", "voidbind:offload-pair?v=1&relay=r&session=s&salt=00"},
		{"missing relay", "voidbind:offload-pair?v=1&session=s&salt=" + hexSalt(salt)},
	} {
		if _, err := DecodeInvite(tc.uri); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
	if _, err := DecodeInvite(good); err != nil {
		t.Fatalf("the good invite should decode: %v", err)
	}
}

func TestPairConfigRoundTrip(t *testing.T) {
	phone := newPairPhone(t)
	cfg, _, _, err := runPair(t, phone)
	if err != nil {
		t.Fatalf("pairing: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, PairConfigFileName)
	if err := SavePairConfig(path, cfg); err != nil {
		t.Fatalf("SavePairConfig: %v", err)
	}
	got, err := LoadPairConfig(path)
	if err != nil {
		t.Fatalf("LoadPairConfig: %v", err)
	}
	if !got.TransportKey.Equal(cfg.TransportKey) ||
		!got.PhonePub.Equal(cfg.PhonePub) ||
		string(got.PhoneEnc) != string(cfg.PhoneEnc) ||
		got.RelayBase != cfg.RelayBase {
		t.Fatalf("pair config did not round-trip through disk")
	}
}

func TestLoadPairConfigRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PairConfigFileName)
	if err := writeFile(path, []byte(`{"version":1,"transport_key":"nope"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPairConfig(path); !errors.Is(err, ErrMalformedPairConfig) {
		t.Fatalf("want ErrMalformedPairConfig, got %v", err)
	}
}
