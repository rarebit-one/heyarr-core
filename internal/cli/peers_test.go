package cli

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

// The peer enrolment commands (§26, ADR-0012, M4-04).
//
// The keys here are derived from fixed seeds rather than generated, so the
// golden files carry the REAL rendered key rather than a placeholder. A key
// normalised away is a key nothing is asserting, and the whole contract of
// these commands is that the value an operator copies between two terminals
// survives the round trip byte for byte.

func fixedKeypair(t *testing.T, fill byte) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{fill}, ed25519.SeedSize))
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("the derived key is not an ed25519 public key")
	}
	return pub, priv
}

func TestPeersAddJSONShape(t *testing.T) {
	h := newAPIHarness(t).seed()
	pub, _ := fixedKeypair(t, 0x07)
	out := h.mustRun("peers", "add",
		"--name", "peer-b", "--site", "site-b",
		"--endpoint", "https://b.example:8385",
		"--public-key", identity.FormatPublicKey(pub), "--json")
	testutil.Golden(t, "testdata/peers_add.json", []byte(normalise(out)))
}

func TestPeersShowJSONShape(t *testing.T) {
	h := newAPIHarness(t).seed()
	pub, _ := fixedKeypair(t, 0x07)
	h.mustRun("peers", "add", "--name", "peer-b", "--site", "site-b",
		"--endpoint", "https://b.example:8385", "--public-key", identity.FormatPublicKey(pub))
	out := h.mustRun("peers", "show", "peer-b", "--json")
	testutil.Golden(t, "testdata/peers_show.json", []byte(normalise(out)))
}

func TestPeersRemoveJSONShape(t *testing.T) {
	h := newAPIHarness(t).seed()
	pub, _ := fixedKeypair(t, 0x07)
	h.mustRun("peers", "add", "--name", "peer-b", "--site", "site-b",
		"--endpoint", "https://b.example:8385", "--public-key", identity.FormatPublicKey(pub))
	out := h.mustRun("peers", "remove", "peer-b", "--json")
	testutil.Golden(t, "testdata/peers_remove.json", []byte(normalise(out)))
}

// TestPeersRemoveIsRefusedForSelf: the CLI must surface the refusal rather
// than swallowing it into a zero exit.
func TestPeersRemoveIsRefusedForSelf(t *testing.T) {
	h := newAPIHarness(t).seed()
	_, _, err := h.run("peers", "remove", "peer-a")
	if err == nil {
		t.Fatal("`peers remove` removed this node and exited 0")
	}
	if !strings.Contains(err.Error(), "cannot remove its own membership") {
		t.Errorf("the error does not say why: %v", err)
	}
}

// TestPeersJSONNeverCarriesPrivateKeyMaterial scans what the command actually
// prints.
//
// It scans rather than reviewing the struct, because the field that leaks a
// key is the one somebody adds after the review — and it plants a REAL private
// key on the node first, so there is something on this machine that could be
// leaked. A leak test run on a node with no private key anywhere proves
// nothing.
func TestPeersJSONNeverCarriesPrivateKeyMaterial(t *testing.T) {
	h := newAPIHarness(t).seed()

	// This node's own identity, on disk exactly as identity.Ensure writes it,
	// and recorded in the database exactly as M4-03 records it.
	selfPub, selfPriv := fixedKeypair(t, 0x11)
	keyFile := identity.KeyPath(h.dataDir)
	seedHex := hex.EncodeToString(selfPriv.Seed())
	if err := os.WriteFile(keyFile, []byte("ed25519-seed:"+seedHex+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE peers SET public_key = ?, key_algo = 'ed25519' WHERE is_self = 1`, []byte(selfPub))

	otherPub, _ := fixedKeypair(t, 0x07)
	outputs := []string{
		h.mustRun("peers", "add", "--name", "peer-b", "--endpoint", "https://b.example:8385",
			"--public-key", identity.FormatPublicKey(otherPub), "--json"),
		h.mustRun("peers", "list", "--json"),
		h.mustRun("peers", "show", "peer-a", "--json"),
		h.mustRun("peers", "show", "peer-b", "--json"),
		h.mustRun("peers", "list"), // the table, not just the JSON
		h.mustRun("peers", "remove", "peer-b", "--json"),
	}
	combined := strings.Join(outputs, "\n")

	// The positive control. Without it this test would pass on empty output,
	// on a command that errored quietly, and on a `peers` that printed nothing
	// at all.
	for _, want := range []string{
		identity.FormatPublicKey(selfPub),
		identity.FormatPublicKey(otherPub),
	} {
		if !strings.Contains(combined, want) {
			t.Fatalf("the public key %s is not in the output at all, so the absence of the "+
				"private key below proves nothing:\n%s", want, combined)
		}
	}

	// And the private half, in every encoding it could plausibly escape in.
	secrets := map[string]string{
		"the seed as hex":            seedHex,
		"the seed as base64":         base64.StdEncoding.EncodeToString(selfPriv.Seed()),
		"the full private key":       base64.StdEncoding.EncodeToString(selfPriv),
		"the full private key (hex)": hex.EncodeToString(selfPriv),
		"the key file's marker":      "ed25519-seed:",
	}
	for what, secret := range secrets {
		if strings.Contains(combined, secret) {
			t.Errorf("`heyarr peers` printed %s", what)
		}
	}

	// The key file itself is untouched and still 0600: a CLI that had read it
	// to print it would have had to open it.
	info, err := os.Stat(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the private key is %#o after running the peers commands, want 0600", perm)
	}
}
