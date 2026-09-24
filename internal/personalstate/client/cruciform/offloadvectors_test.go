package cruciform

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/rarebit-one/voidbind-go/encryption"
	"github.com/rarebit-one/voidbind-go/pairing"
)

// This test is the golden-vector generator + self-consistency check for the
// cruciform-offload wire (ADR-0098). The wire is heyarr-core-local, so the
// voidbind-kmp/cruciform phone half mirrors THIS: the deterministic vectors it
// emits (invite, pairing-confirm transcript + SAS + both signatures, and the
// unwrap request/response signing-inputs + marshalled bytes) are replayed
// byte-for-byte on the Kotlin side, and a Go-sealed interop blob proves the
// phone can open what the desktop wraps.
//
// It always runs as an ordinary test (asserting the wire is internally
// consistent — signatures verify, records round-trip, the interop blob opens). It
// ALSO writes the vectors JSON when HEYARR_OFFLOAD_VECTORS_OUT names a path, which
// is how the committed copy under voidbind-kmp's test resources is refreshed. The
// interop blob uses a fresh ephemeral (encryption.Seal is randomised), so the
// vectors are regenerated rather than byte-pinned in this repo; the byte-for-byte
// parity lives in the Kotlin replay.

// rep returns an n-byte slice filled with b — fixed, legible test inputs.
func rep(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func mustX25519(t *testing.T, seed []byte) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().NewPrivateKey(seed)
	if err != nil {
		t.Fatalf("x25519 key from seed: %v", err)
	}
	return k
}

type offloadVectors struct {
	Note    string        `json:"note"`
	Invite  inviteVector  `json:"invite"`
	Pair    pairVector    `json:"pairing_confirm"`
	Req     reqVector     `json:"unwrap_request"`
	Resp    respVector    `json:"unwrap_response"`
	Interop interopVector `json:"seal_interop"`
}

type inviteVector struct {
	RelayBase string `json:"relay_base"`
	Session   string `json:"session"`
	SaltHex   string `json:"salt_hex"`
	URI       string `json:"uri"`
}

type pairVector struct {
	Session       string `json:"session"`
	SaltHex       string `json:"salt_hex"`
	TransportPub  string `json:"transport_pub_hex"`
	PhonePub      string `json:"phone_pub_hex"`
	PhoneEnc      string `json:"phone_enc_hex"`
	TranscriptHex string `json:"transcript_hex"`
	TransportSig  string `json:"transport_sig_hex"` // desktop signs; phone verifies
	PhoneSig      string `json:"phone_sig_hex"`     // phone signs; desktop verifies
	SAS           string `json:"sas"`
}

type reqVector struct {
	WrappedHex      string `json:"wrapped_hex"`
	EphPubHex       string `json:"eph_pub_hex"`
	NonceHex        string `json:"nonce_hex"`
	TransportPubHex string `json:"transport_pub_hex"`
	SigningInputHex string `json:"signing_input_hex"`
	RequestHex      string `json:"request_hex"` // marshalled, signed by the transport key
}

type respVector struct {
	NonceHex        string `json:"nonce_hex"`
	SealedHex       string `json:"sealed_hex"`
	PhonePubHex     string `json:"phone_pub_hex"`
	SigningInputHex string `json:"signing_input_hex"`
	ResponseHex     string `json:"response_hex"` // marshalled, signed by the phone device key
}

type interopVector struct {
	RecipientSeedHex string `json:"recipient_seed_hex"` // the phone's x25519 seed
	WrappedHex       string `json:"wrapped_hex"`        // encryption.Seal(spaceKey, phonePub)
	CanaryCtHex      string `json:"canary_ct_hex"`      // encryption.EncryptChange(spaceKey, canary)
	CanaryPlainHex   string `json:"canary_plain_hex"`   // Kotlin unwrap→decryptChange must recover this
}

func TestOffloadWireVectors(t *testing.T) {
	transportPriv := ed25519.NewKeyFromSeed(rep(0x11, 32))
	transportPub := transportPriv.Public().(ed25519.PublicKey)
	phonePriv := ed25519.NewKeyFromSeed(rep(0x22, 32))
	phonePub := phonePriv.Public().(ed25519.PublicKey)
	phoneEncPriv := mustX25519(t, rep(0x33, 32))
	phoneEnc := phoneEncPriv.PublicKey().Bytes()
	ephPub := mustX25519(t, rep(0x44, 32)).PublicKey().Bytes()

	salt := rep(0x55, 16)
	const session = "sess-golden-1"
	const relayBase = "https://relay.example/pair"
	nonce := rep(0x66, 16)
	wrapped := rep(0x77, 104)
	sealed := rep(0x88, 120)

	// --- invite ---
	uri, err := EncodeInvite(relayBase, session, salt)
	if err != nil {
		t.Fatalf("EncodeInvite: %v", err)
	}
	if inv, err := DecodeInvite(uri); err != nil || inv.RelayBase != relayBase || inv.Session != session || string(inv.Salt) != string(salt) {
		t.Fatalf("invite did not round-trip: %v / %+v", err, inv)
	}

	// --- pairing confirm transcript + both signatures + SAS ---
	transcript := pairTranscript(session, salt, transportPub, nil, phonePub, phoneEnc)
	transportSig := ed25519.Sign(transportPriv, transcript)
	phoneSig := ed25519.Sign(phonePriv, transcript)
	if !ed25519.Verify(transportPub, transcript, transportSig) || !ed25519.Verify(phonePub, transcript, phoneSig) {
		t.Fatal("confirm signatures do not verify against their signers")
	}
	sas, err := pairing.Derive(
		pairing.Keys{Sign: transportPub},
		pairing.Keys{Sign: phonePub, Enc: phoneEnc},
		salt,
	)
	if err != nil {
		t.Fatalf("pairing.Derive: %v", err)
	}

	// --- unwrap request: signing input + marshalled signed record ---
	reqSigningInput := requestSigningInput(wrapped, ephPub, nonce)
	reqBytes, err := signRequest(transportPriv, wrapped, ephPub, nonce)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	if _, err := verifyRequest(transportPub, reqBytes); err != nil {
		t.Fatalf("request does not verify/round-trip: %v", err)
	}

	// --- unwrap response: signing input + marshalled signed record ---
	respSigningInput := responseSigningInput(nonce, sealed)
	respBytes, err := signResponse(phonePriv, nonce, sealed)
	if err != nil {
		t.Fatalf("signResponse: %v", err)
	}
	if _, err := verifyResponse(phonePub, nonce, respBytes); err != nil {
		t.Fatalf("response does not verify/round-trip: %v", err)
	}

	// --- seal interop: the phone (Kotlin) must open what the desktop (Go) wraps ---
	sk, err := encryption.NewSpaceKey()
	if err != nil {
		t.Fatalf("NewSpaceKey: %v", err)
	}
	wrappedInterop, err := encryption.Seal(sk, phoneEncPriv.PublicKey())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if got, err := encryption.Unwrap(wrappedInterop, phoneEncPriv); err != nil || got.IsZero() {
		t.Fatalf("Go could not open its own sealed blob: %v", err)
	}
	canary := []byte("cruciform-offload-canary")
	canaryCt, err := encryption.EncryptChange(sk, canary)
	if err != nil {
		t.Fatalf("EncryptChange: %v", err)
	}

	vectors := offloadVectors{
		Note: "cruciform-offload wire golden vectors (ADR-0098); generated by heyarr-core " +
			"internal/personalstate/client/cruciform. The Kotlin phone half replays these byte-for-byte.",
		Invite: inviteVector{RelayBase: relayBase, Session: session, SaltHex: hex.EncodeToString(salt), URI: uri},
		Pair: pairVector{
			Session: session, SaltHex: hex.EncodeToString(salt),
			TransportPub: hex.EncodeToString(transportPub), PhonePub: hex.EncodeToString(phonePub), PhoneEnc: hex.EncodeToString(phoneEnc),
			TranscriptHex: hex.EncodeToString(transcript), TransportSig: hex.EncodeToString(transportSig), PhoneSig: hex.EncodeToString(phoneSig), SAS: string(sas),
		},
		Req: reqVector{
			WrappedHex: hex.EncodeToString(wrapped), EphPubHex: hex.EncodeToString(ephPub), NonceHex: hex.EncodeToString(nonce),
			TransportPubHex: hex.EncodeToString(transportPub), SigningInputHex: hex.EncodeToString(reqSigningInput), RequestHex: hex.EncodeToString(reqBytes),
		},
		Resp: respVector{
			NonceHex: hex.EncodeToString(nonce), SealedHex: hex.EncodeToString(sealed), PhonePubHex: hex.EncodeToString(phonePub),
			SigningInputHex: hex.EncodeToString(respSigningInput), ResponseHex: hex.EncodeToString(respBytes),
		},
		Interop: interopVector{
			RecipientSeedHex: hex.EncodeToString(rep(0x33, 32)), WrappedHex: hex.EncodeToString(wrappedInterop),
			CanaryCtHex: hex.EncodeToString(canaryCt), CanaryPlainHex: hex.EncodeToString(canary),
		},
	}

	if out := os.Getenv("HEYARR_OFFLOAD_VECTORS_OUT"); out != "" {
		data, err := json.MarshalIndent(vectors, "", "  ")
		if err != nil {
			t.Fatalf("marshal vectors: %v", err)
		}
		data = append(data, '\n')
		if err := os.WriteFile(out, data, 0o644); err != nil { //nolint:gosec // a golden fixture, world-readable by design
			t.Fatalf("writing vectors to %s: %v", out, err)
		}
		t.Logf("wrote offload vectors to %s", out)
	}
}
