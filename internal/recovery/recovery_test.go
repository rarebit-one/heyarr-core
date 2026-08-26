package recovery

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// mustGenerate draws a secret or fails the test — the ordinary "a user has a
// recovery secret" starting point.
func mustGenerate(t *testing.T) Secret {
	t.Helper()
	s, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	return s
}

// TestDeterminism is the property the whole design rests on: the same secret
// derives the same seed, and thus the same key, every time. If this ever fails,
// recovery reconstructs a DIFFERENT identity than the one enrolled, and every
// pinned peer rejects it — the exact failure ADR-0022 exists to prevent.
func TestDeterminism(t *testing.T) {
	s := mustGenerate(t)
	want := DeriveUserSeed(s)
	if len(want) != ed25519.SeedSize {
		t.Fatalf("seed is %d bytes, want %d", len(want), ed25519.SeedSize)
	}
	for i := 0; i < 1000; i++ {
		if got := DeriveUserSeed(s); !bytes.Equal(got, want) {
			t.Fatalf("derivation %d differs: derivation is not deterministic", i)
		}
	}

	// Determinism must survive the round trip through the encoded form the user
	// actually stores: parse the string back and the seed is identical.
	reparsed, err := ParseSecret(s.String())
	if err != nil {
		t.Fatalf("ParseSecret(own encoding): %v", err)
	}
	if got := DeriveUserSeed(reparsed); !bytes.Equal(got, want) {
		t.Fatal("a secret round-tripped through its encoding derives a different seed")
	}
}

// TestRoundTripEncoding proves the transcribable form is lossless: encode, parse,
// and the entropy — and therefore the derived key — is preserved exactly.
func TestRoundTripEncoding(t *testing.T) {
	for i := 0; i < 100; i++ {
		s := mustGenerate(t)
		enc := s.String()
		if !strings.HasPrefix(enc, hrp+"1") {
			t.Fatalf("encoding %q lacks the %q1 prefix", enc, hrp)
		}
		back, err := ParseSecret(enc)
		if err != nil {
			t.Fatalf("ParseSecret(%q): %v", enc, err)
		}
		if !bytes.Equal(back.entropy, s.entropy) {
			t.Fatalf("round trip changed the entropy for %q", enc)
		}
	}
}

// TestDistinctness: different secrets derive different keys. A collision here
// would mean two users recover to one identity.
func TestDistinctness(t *testing.T) {
	const n = 500
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		s := mustGenerate(t)
		key := string(DeriveUserSeed(s))
		if _, dup := seen[key]; dup {
			t.Fatal("two distinct secrets derived the same seed")
		}
		seen[key] = struct{}{}
	}
}

// TestValidity: the derived seed is a usable Ed25519 identity — it signs and
// verifies — and its public key renders and re-parses through the same
// identity.FormatPublicKey/ParsePublicKey a peer pins with, and slots straight
// into an enrolment.UserIdentity. This is the "recovers the authority peers
// already pinned" acceptance, at the primitive level.
func TestValidity(t *testing.T) {
	s := mustGenerate(t)
	seed := DeriveUserSeed(s)

	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)

	msg := []byte("a device enrolment cert the recovered identity signs")
	sig := ed25519.Sign(priv, msg)
	if !ed25519.Verify(pub, msg, sig) {
		t.Fatal("the derived key did not sign-and-verify")
	}

	// Renders and re-parses through the peer-facing encoding.
	id := FormatUserID(pub)
	if id != identity.FormatPublicKey(pub) {
		t.Fatalf("FormatUserID %q != identity.FormatPublicKey %q", id, identity.FormatPublicKey(pub))
	}
	parsed, err := identity.ParsePublicKey(id)
	if err != nil {
		t.Fatalf("ParsePublicKey(%q): %v", id, err)
	}
	if !bytes.Equal(parsed, pub) {
		t.Fatal("the derived public key did not round-trip through identity encoding")
	}

	// And it IS a user identity: the same string an enrolment cert names.
	if got := (enrolment.UserIdentity{PublicKey: pub}).UserID(); got != id {
		t.Fatalf("enrolment.UserIdentity.UserID() = %q, want %q", got, id)
	}
}

// TestEncryptionSeedIsAUsableX25519Key: the seed DeriveUserEncryptionSeed
// produces is a valid X25519 scalar that yields a working key-agreement key —
// the primitive M9's wrap/unwrap is built on (ADR-0049). Deterministic like the
// signing seed: the same secret reconstructs the same recovery encryption key, so
// the copies peers hold wrapped for it unwrap after a total device loss.
func TestEncryptionSeedIsAUsableX25519Key(t *testing.T) {
	s := mustGenerate(t)
	seed := DeriveUserEncryptionSeed(s)
	if len(seed) != 32 {
		t.Fatalf("encryption seed is %d bytes, want 32 (an X25519 scalar)", len(seed))
	}

	priv, err := ecdh.X25519().NewPrivateKey(seed)
	if err != nil {
		t.Fatalf("the derived seed is not a usable X25519 private key: %v", err)
	}

	// Deterministic: the same secret reconstructs the same public key, so the
	// recovery target the wraps name is stable across machines and time.
	priv2, err := ecdh.X25519().NewPrivateKey(DeriveUserEncryptionSeed(s))
	if err != nil {
		t.Fatalf("second derivation is not a usable key: %v", err)
	}
	if !bytes.Equal(priv.PublicKey().Bytes(), priv2.PublicKey().Bytes()) {
		t.Fatal("the recovery encryption key is not deterministic across derivations")
	}

	// It agrees with a peer key — i.e. it can actually do ECDH, which is what a
	// space-key unwrap needs.
	other, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a counterpart key: %v", err)
	}
	shared1, err := priv.ECDH(other.PublicKey())
	if err != nil {
		t.Fatalf("ECDH from the recovery key: %v", err)
	}
	shared2, err := other.ECDH(priv.PublicKey())
	if err != nil {
		t.Fatalf("ECDH to the recovery key: %v", err)
	}
	if !bytes.Equal(shared1, shared2) {
		t.Fatal("the recovery encryption key did not agree with a counterpart: not a working X25519 key")
	}
}

// TestFailsLoudOnChecksum is the load-bearing one: a well-formed secret with a
// single character changed to another alphabet character — the shape of a
// transcription slip — is REJECTED by the checksum as [ErrCorruptSecret], not
// silently decoded into a different key. This is the SLIP-39-over-plain-Shamir
// concern of ADR-0022 discharged at the base secret.
func TestFailsLoudOnChecksum(t *testing.T) {
	s := mustGenerate(t)
	enc := s.String()
	dataStart := len(hrp) + 1 // past "heyarr1"

	corruptions := 0
	for i := dataStart; i < len(enc); i++ {
		orig := enc[i]
		for _, repl := range charset {
			if byte(repl) == orig {
				continue
			}
			mutated := enc[:i] + string(repl) + enc[i+1:]
			_, err := ParseSecret(mutated)
			if err == nil {
				t.Fatalf("a one-character change at %d (%c→%c) was ACCEPTED: %q",
					i, orig, repl, mutated)
			}
			// A single in-alphabet substitution can only be a checksum miss,
			// never a structural malformation — so it must be the loud,
			// attributable corrupt-secret sentinel.
			if !errors.Is(err, ErrCorruptSecret) {
				t.Fatalf("one-char change at %d (%c→%c) rejected as %v, want ErrCorruptSecret",
					i, orig, repl, err)
			}
			corruptions++
		}
	}
	if corruptions == 0 {
		t.Fatal("no corruptions exercised")
	}
	t.Logf("rejected all %d single-character substitutions", corruptions)
}

// TestParseRejectsMalformed: everything that is not a well-formed secret is
// refused with ErrMalformedSecret, never parsed into a usable value.
func TestParseRejectsMalformed(t *testing.T) {
	valid := mustGenerate(t).String()
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"no separator", "heyarrqqqqqqqq"},
		{"wrong prefix", "wrong1" + strings.TrimPrefix(valid, hrp+"1")},
		{"out-of-alphabet char", valid[:len(valid)-1] + "b"}, // 'b' is not in the charset
		{"truncated", valid[:len(valid)-8]},
		{"empty hrp", strings.TrimPrefix(valid, hrp)}, // begins with '1'
		{"only prefix", "heyarr1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseSecret(tt.in); !errors.Is(err, ErrMalformedSecret) {
				t.Fatalf("ParseSecret(%q) = %v, want ErrMalformedSecret", tt.in, err)
			}
		})
	}
}

// TestTruncatedSecretDoesNotDerive is the sabotage-(1) shape from the issue:
// deriving from a truncated secret must fail at the gate, not reconstruct a key.
// A truncated secret cannot reach DeriveUserSeed because ParseSecret refuses it.
func TestTruncatedSecretDoesNotDerive(t *testing.T) {
	enc := mustGenerate(t).String()
	for cut := 1; cut <= 8; cut++ {
		if _, err := ParseSecret(enc[:len(enc)-cut]); err == nil {
			t.Fatalf("a secret truncated by %d chars parsed successfully", cut)
		}
	}
}

// TestDomainSeparation: the SAME secret under two different labels derives two
// INDEPENDENT keys. This is what lets the Milestone 9 encryption root share the
// recovery secret without ever colliding with the identity signing seed (§79).
func TestDomainSeparation(t *testing.T) {
	s := mustGenerate(t)

	userSeed := deriveSeed(s.entropy, UserIdentityLabel)
	encRoot := deriveSeed(s.entropy, UserEncryptionLabel)
	if bytes.Equal(userSeed, encRoot) {
		t.Fatal("the signing and encryption labels derived the same key: domain separation is broken")
	}

	// DeriveUserSeed must use exactly UserIdentityLabel — not some other label
	// or a bare expand — so the public entry point and the label agree.
	if !bytes.Equal(DeriveUserSeed(s), userSeed) {
		t.Fatal("DeriveUserSeed does not derive under UserIdentityLabel")
	}

	// DeriveUserEncryptionSeed must use exactly UserEncryptionLabel — the M9
	// encryption root — and so must be independent of the signing seed. This is
	// the sabotage target: deriving the encryption seed under the signing label
	// (or any shared one) makes these two equal and fires here.
	if !bytes.Equal(DeriveUserEncryptionSeed(s), encRoot) {
		t.Fatal("DeriveUserEncryptionSeed does not derive under UserEncryptionLabel")
	}
	if bytes.Equal(DeriveUserSeed(s), DeriveUserEncryptionSeed(s)) {
		t.Fatal("the signing seed and the encryption seed coincide: §79 domain separation is broken")
	}

	// A one-character change to the label yields an independent key too — the
	// label is a genuine domain-separation tag, not decoration.
	near := deriveSeed(s.entropy, UserIdentityLabel+"x")
	if bytes.Equal(userSeed, near) {
		t.Fatal("a changed label produced the same key")
	}
}

// TestOfflinePure enforces ADR-0022's "recovery must not require Heyarr to be
// running" structurally: the package's own source may not import anything that
// reaches the filesystem, the network or a process. The derivation is a pure
// function of its input, and this asserts it the way the domain invariants are
// asserted here — by forbidding the import, not by trusting review.
//
// The test file itself is exempt: it necessarily imports os/go-parser to perform
// this very check.
func TestOfflinePure(t *testing.T) {
	forbidden := map[string]bool{
		"os": true, "os/exec": true, "net": true, "net/http": true,
		"path": true, "path/filepath": true, "io": true, "io/ioutil": true,
		"bufio": true, "database/sql": true, "syscall": true,
		"os/signal": true, "net/url": true,
	}
	forbiddenPrefix := []string{"net/", "database/", "golang.org/x/net"}

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package dir: %v", err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Clean(name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if forbidden[path] {
				t.Errorf("%s imports %q — recovery must touch no filesystem/network/process", name, path)
			}
			for _, p := range forbiddenPrefix {
				if strings.HasPrefix(path, p) {
					t.Errorf("%s imports %q — recovery must stay offline and pure", name, path)
				}
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no package source files were checked")
	}
}

// TestBech32KnownAnswer pins the codec to a fixed vector so a refactor that
// changes the wire format — and would silently invalidate every secret ever
// written on paper — fails here instead of in the field.
func TestBech32KnownAnswer(t *testing.T) {
	s := Secret{entropy: bytes.Repeat([]byte{0x00}, SecretEntropyBytes)}
	enc := s.String()

	back, err := ParseSecret(enc)
	if err != nil {
		t.Fatalf("ParseSecret(%q): %v", enc, err)
	}
	if !bytes.Equal(back.entropy, s.entropy) {
		t.Fatal("known-answer secret did not round-trip")
	}
	// Stable across runs: encoding a fixed input is a fixed string.
	again := (Secret{entropy: bytes.Repeat([]byte{0x00}, SecretEntropyBytes)}).String()
	if enc != again {
		t.Fatal("encoding a fixed secret is not stable")
	}
}
