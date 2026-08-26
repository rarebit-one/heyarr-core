package pairflow

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/pairing"
)

// memRelay is an in-memory, write-once Relay: exactly the untrusted transport
// the two devices exchange through, with nothing behind it. It records every
// value ever stored so a test can prove the relay never saw a private key.
type memRelay struct {
	mu   sync.Mutex
	data map[string][]byte
	seen [][]byte
}

func newMemRelay() *memRelay { return &memRelay{data: map[string][]byte{}} }

func (m *memRelay) key(session, slot string) string { return session + "/" + slot }

func (m *memRelay) Put(_ context.Context, session, slot string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.key(session, slot)
	if _, ok := m.data[k]; ok {
		return nil // write-once; a repeat is a no-op, matching the HTTP relay
	}
	cp := append([]byte(nil), data...)
	m.data[k] = cp
	m.seen = append(m.seen, cp)
	return nil
}

func (m *memRelay) Get(_ context.Context, session, slot string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[m.key(session, slot)]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), v...), true, nil
}

// keypair returns a deterministic keypair for a seed byte.
func keypair(t *testing.T, seed byte) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{seed}, ed25519.SeedSize)))
	if err != nil {
		t.Fatalf("generating a keypair: %v", err)
	}
	return pub, priv
}

func alwaysYes(pairing.SAS) (bool, error) { return true, nil }

// The honest path: an old device and a new device, exchanging through the relay,
// derive the SAME SAS, the old device signs a cert for the new device's key, and
// the new device stores it. The cert is signed with the USER private key the
// flow never holds — it is handed in through Sign.
func TestHonestPairingEnrolsTheNewDevice(t *testing.T) {
	relay := newMemRelay()
	userPub, userPriv := keypair(t, 1)
	devPub, _ := keypair(t, 2)
	salt := bytes.Repeat([]byte{0x5A}, pairing.SaltLen)

	var stored string
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var initRes, respRes Result
	var initErr, respErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		initRes, initErr = Initiator{
			Relay: relay, Session: "s1", PollInterval: 5 * time.Millisecond,
			UserPub: userPub, Salt: salt,
			Confirm: alwaysYes,
			Sign: func(dk ed25519.PublicKey) (string, error) {
				// Stand in for useridentity.SignCert: sign the new device key.
				return "cert-for:" + string(dk), nil
			},
		}.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		respRes, respErr = Responder{
			Relay: relay, Session: "s1", PollInterval: 5 * time.Millisecond,
			DevicePub: devPub,
			Confirm:   alwaysYes,
			Accept:    func(cert string) error { stored = cert; return nil },
		}.Run(ctx)
	}()
	wg.Wait()

	if initErr != nil {
		t.Fatalf("initiator: %v", initErr)
	}
	if respErr != nil {
		t.Fatalf("responder: %v", respErr)
	}
	if initRes.SAS == "" || initRes.SAS != respRes.SAS {
		t.Fatalf("the two sides derived different SAS: initiator %q, responder %q", initRes.SAS, respRes.SAS)
	}
	if !initRes.Confirmed || !respRes.Confirmed {
		t.Fatal("a side did not record the confirmation")
	}
	want := "cert-for:" + string(devPub)
	if stored != want {
		t.Fatalf("the responder stored %q, want %q", stored, want)
	}
	if initRes.Cert != want {
		t.Fatalf("the initiator posted %q, want %q", initRes.Cert, want)
	}
	// The initiator authenticated the responder's device key, and vice versa.
	if !bytes.Equal(initRes.PeerKey, devPub) {
		t.Fatal("the initiator did not authenticate the responder's device key")
	}
	if !bytes.Equal(respRes.PeerKey, userPub) {
		t.Fatal("the responder did not authenticate the user key")
	}
	_ = userPriv
}

// The relay learns no key material (ADR-0038). After a full honest run, every
// value the relay ever stored is scanned, and NONE of them contains either
// party's private seed. This is the dumb-relay property asserted structurally
// rather than asserted in prose.
func TestRelayLearnsNoKeyMaterial(t *testing.T) {
	relay := newMemRelay()
	userPub, userPriv := keypair(t, 3)
	devPub, devPriv := keypair(t, 4)
	salt := bytes.Repeat([]byte{0x11}, pairing.SaltLen)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = Initiator{
			Relay: relay, Session: "s", PollInterval: 5 * time.Millisecond,
			UserPub: userPub, Salt: salt, Confirm: alwaysYes,
			Sign: func(dk ed25519.PublicKey) (string, error) { return "cert", nil },
		}.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		_, _ = Responder{
			Relay: relay, Session: "s", PollInterval: 5 * time.Millisecond,
			DevicePub: devPub, Confirm: alwaysYes, Accept: func(string) error { return nil },
		}.Run(ctx)
	}()
	wg.Wait()

	secrets := [][]byte{userPriv.Seed(), devPriv.Seed(), userPriv, devPriv}
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if len(relay.seen) == 0 {
		t.Fatal("the relay stored nothing, so this proves nothing")
	}
	for _, stored := range relay.seen {
		for _, secret := range secrets {
			if bytes.Contains(stored, secret) {
				t.Fatalf("the relay stored a value containing private key material: %x", stored)
			}
		}
	}
}

// The rushing attack, and the commit-before-reveal defence that closes it. A
// malicious responder COMMITS to one key and then REVEALS a different one —
// exactly the freedom a rushing attacker needs to choose its key after seeing the
// peer's. The initiator must refuse, because the revealed key does not open the
// commitment.
//
// This is the sabotage target: delete the Commitment.Open check in Initiator.Run
// and this test stops failing the attack — the initiator signs a cert for the
// substituted key K2 and posts it. The assertions below (ErrCommitmentMismatch,
// no cert) are what fire when that check is present and would not when it is gone.
func TestRushingSubstitutionIsRefused(t *testing.T) {
	relay := newMemRelay()
	userPub, _ := keypair(t, 5)
	committedKey, _ := keypair(t, 6) // K1 — what the attacker commits to
	revealedKey, _ := keypair(t, 7)  // K2 — what it rushes in at reveal time
	salt := bytes.Repeat([]byte{0x22}, pairing.SaltLen)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var signed bool
	var initRes Result
	var initErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		initRes, initErr = Initiator{
			Relay: relay, Session: "rush", PollInterval: 5 * time.Millisecond,
			UserPub: userPub, Salt: salt, Confirm: alwaysYes,
			Sign: func(ed25519.PublicKey) (string, error) { signed = true; return "cert", nil },
		}.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		// The malicious responder, by hand: commit to K1, reveal K2.
		if _, err := waitSlot(ctx, relay, "rush", SlotInitiatorCommit, 5*time.Millisecond); err != nil {
			return
		}
		commit, _ := pairing.Commit(committedKey)
		_ = relay.Put(ctx, "rush", SlotResponderCommit, commit)
		_ = relay.Put(ctx, "rush", SlotResponderReveal, revealBytes(revealedKey, nil))
	}()
	wg.Wait()

	if !errors.Is(initErr, ErrCommitmentMismatch) {
		t.Fatalf("the initiator did not refuse a rushed substitution: err=%v — "+
			"the commit-before-reveal check is broken and the rushing attack is open", initErr)
	}
	if signed {
		t.Fatal("the initiator SIGNED a cert for a key that did not open its commitment — the attack succeeded")
	}
	if initRes.Cert != "" {
		t.Fatal("a cert was produced for a rushed key")
	}
}

// A substituted key yields a DIFFERENT SAS, so the human comparison catches it.
// Measured at the flow level: the SAS the initiator derives against an honest
// responder key differs from the one it derives against a substituted key, so a
// MITM presenting its own key cannot make the two screens match.
func TestSubstitutedKeyChangesTheInitiatorSAS(t *testing.T) {
	userPub, _ := keypair(t, 8)
	honest, _ := keypair(t, 9)
	substitute, _ := keypair(t, 10)
	salt := bytes.Repeat([]byte{0x33}, pairing.SaltLen)

	sasFor := func(responderKey ed25519.PublicKey) pairing.SAS {
		relay := newMemRelay()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var got pairing.SAS
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			res, _ := Initiator{
				Relay: relay, Session: "sub", PollInterval: 5 * time.Millisecond,
				UserPub: userPub, Salt: salt,
				// Record the SAS and refuse, so no cert is signed.
				Confirm: func(s pairing.SAS) (bool, error) { got = s; return false, nil },
				Sign:    func(ed25519.PublicKey) (string, error) { return "cert", nil },
			}.Run(ctx)
			_ = res
		}()
		go func() {
			defer wg.Done()
			if _, err := waitSlot(ctx, relay, "sub", SlotInitiatorCommit, 5*time.Millisecond); err != nil {
				return
			}
			commit, _ := pairing.Commit(responderKey)
			_ = relay.Put(ctx, "sub", SlotResponderCommit, commit)
			_ = relay.Put(ctx, "sub", SlotResponderReveal, revealBytes(responderKey, nil))
		}()
		wg.Wait()
		return got
	}

	honestSAS := sasFor(honest)
	substituteSAS := sasFor(substitute)
	if honestSAS == "" || substituteSAS == "" {
		t.Fatal("a SAS was not derived")
	}
	if honestSAS == substituteSAS {
		t.Fatalf("a substituted key produced the SAME SAS (%q) — the binding is broken", honestSAS)
	}
}

// A human who says the codes do not match refuses the pairing, and no cert is
// signed — the ordinary, non-attack refusal.
func TestSASRefusalStopsBeforeSigning(t *testing.T) {
	relay := newMemRelay()
	userPub, _ := keypair(t, 11)
	devPub, _ := keypair(t, 12)
	salt := bytes.Repeat([]byte{0x44}, pairing.SaltLen)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var signed, accepted bool
	var initErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, initErr = Initiator{
			Relay: relay, Session: "no", PollInterval: 5 * time.Millisecond,
			UserPub: userPub, Salt: salt,
			Confirm: func(pairing.SAS) (bool, error) { return false, nil },
			Sign:    func(ed25519.PublicKey) (string, error) { signed = true; return "cert", nil },
		}.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		_, _ = Responder{
			Relay: relay, Session: "no", PollInterval: 5 * time.Millisecond,
			DevicePub: devPub, Confirm: alwaysYes,
			Accept: func(string) error { accepted = true; return nil },
		}.Run(ctx)
	}()
	wg.Wait()

	if !errors.Is(initErr, ErrSASRefused) {
		t.Fatalf("a refused SAS did not return ErrSASRefused: %v", initErr)
	}
	if signed {
		t.Fatal("a cert was signed after the human refused")
	}
	if accepted {
		t.Fatal("the responder accepted a cert after a refusal")
	}
}
