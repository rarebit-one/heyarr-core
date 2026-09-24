package cruciform

// This file is the DESKTOP half of the one-time cruciform-offload pairing
// ceremony (ADR-0098 live-path addendum). It establishes desktop↔phone trust
// once, so every later unwrap is push/discover + biometric with no QR:
//
//   - the phone pins this desktop's persistent TRANSPORT signing key, which then
//     authenticates each unwrap request as coming from the paired terminal (the
//     confused-deputy gate a wake-with-no-QR would otherwise reopen);
//   - the desktop pins the phone's DEVICE signing key (it verifies each unwrap
//     response with it) and the phone's X25519 ENCRYPTION key (the wrap target it
//     looks the sealed space-key copy up by — RecipientID).
//
// It is NOT a voidbind membership enrolment. The transport key is a pairing
// identity, not a custody key: it holds no device encryption key and is never a
// member (ADR-0098 — the desktop holds nothing). So this ceremony deliberately
// does NOT use pairflow.Initiator (which signs an `add` op and seals a space key
// to enrol a new member); it reuses only the lower-level SAS primitives
// (pairing.Commit/Open/Derive) that pairflow itself is built on, over the same
// dumb relay, and pins keys instead of enrolling.
//
// Like the unwrap protocol (protocol.go), this pairing wire is heyarr-core-local:
// the voidbind-kmp/cruciform phone half mirrors THIS, not a voidbind-go pairflow
// variant. The generic pieces it rides — the relay, the SAS commit-reveal, the
// pairing primitives — are voidbind-go's; the offload-specific shape is here.
//
// # The handshake, and where each guarantee lives
//
//  1. Each side COMMITS to its keys and posts the commitment BEFORE revealing any
//     key (pairing.Commit) — the rushing-attack gate, exactly as pairflow.
//  2. Each side REVEALS its keys, fetches the peer's, and OPENS the peer's
//     commitment against them (pairing.Commitment.Open). A key that does not open
//     its commitment aborts the flow.
//  3. Each side DERIVES the SAS (pairing.Derive) over both key sets and the shared
//     salt. A relay that substituted either key produces a different string on the
//     two sides — the mismatch a human catches. [Initiator.Handshake] returns the
//     SAS; it pins NOTHING on its own.
//  4. Only after a human confirms the two strings match does each side CONFIRM: it
//     signs the transcript with its OWN signing key and posts it, and verifies the
//     peer's confirmation against the peer's revealed signing key before pinning.
//     The confirmation is a proof-of-possession (the peer controls the private key
//     behind the pubkey it revealed) AND the mutual "the human said yes" gate — a
//     side that never confirms (the strings differed) pins nobody.
//
// The relay only ever holds two commitments (hashes), two reveals (PUBLIC keys),
// and two confirmations (signatures over public values) — nothing secret, and any
// substitution it attempts is caught by the SAS or a commitment. That is the
// dumb-relay model (ADR-0038) again.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/rarebit-one/voidbind-go/encryption"
	"github.com/rarebit-one/voidbind-go/pairing"

	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// The relay slot names the pairing ceremony writes, one per handshake step. They
// live on their own relay session (the invite's session id), distinct from an
// unwrap exchange's slots.
const (
	pairMsgCommit  = "commit"
	pairMsgReveal  = "reveal"
	pairMsgConfirm = "confirm"
)

// InviteScheme and the offload invite's opaque + version. The scheme matches
// voidbind's pairing invite so a phone routes both through one QR scanner, but the
// opaque is distinct (`offload-pair`, not `pair`) so the phone dispatches this to
// the offload-pairing handler rather than device enrolment.
const (
	InviteScheme      = "voidbind"
	inviteOpaque      = "offload-pair"
	offloadInviteVer  = "1"
	pairConfirmDomain = "heyarr-cruciform-pair-confirm-v1\x00"
	pairConfigVersion = 1
	// PairConfigFileName is the offload pairing artifact's name in the device
	// directory, beside the software backend's key files.
	PairConfigFileName = "cruciform-pairing.json"
)

// Pairing sentinels.
var (
	ErrNotHandshaken       = errors.New("cruciform: Confirm before a successful Handshake")
	ErrBadPeerConfirm      = errors.New("cruciform: the phone's pairing confirmation does not verify against its revealed device key")
	ErrMalformedInvite     = errors.New("cruciform: malformed offload pairing invite")
	ErrMalformedPairConfig = errors.New("cruciform: malformed offload pairing config")
)

// PairTransport ferries opaque pairing messages between the two sides through the
// relay. Post writes THIS side's slot for a step; Fetch reads the PEER side's
// slot, blocking until it is present or ctx is done. voidbind's relay.Client
// implements it (the CLI wires one as role "initiator"); a test supplies an
// in-memory fake. Its shape matches voidbind's pairflow.Transport deliberately.
type PairTransport interface {
	Post(ctx context.Context, msgType string, payload []byte) error
	Fetch(ctx context.Context, msgType string) ([]byte, error)
}

// EncodeInvite renders the QR/short-code payload the phone scans to bootstrap the
// pairing rendezvous:
//
//	voidbind:offload-pair?v=1&relay=<origin>&session=<id>&salt=<hex>
//
// The salt is hex so it survives a QR/text round-trip. The relay, session and
// salt travel over the VISUAL channel (the phone scans the desktop's screen),
// which is what authenticates them — a network attacker cannot substitute them
// without also defeating the SAS. This is the wire contract the phone half scans.
func EncodeInvite(relayBase, session string, salt []byte) (string, error) {
	if relayBase == "" || session == "" {
		return "", fmt.Errorf("cruciform: invite needs a relay and a session")
	}
	if len(salt) < pairing.MinSaltLen {
		return "", fmt.Errorf("cruciform: invite salt is %d bytes, want at least %d", len(salt), pairing.MinSaltLen)
	}
	q := url.Values{}
	q.Set("v", offloadInviteVer)
	q.Set("relay", relayBase)
	q.Set("session", session)
	q.Set("salt", hex.EncodeToString(salt))
	return InviteScheme + ":" + inviteOpaque + "?" + q.Encode(), nil
}

// pairReveal is the wire form of a side's revealed public keys. Keys are rendered
// in the canonical algorithm-prefixed hex the rest of the system uses. The
// desktop reveals only its transport signing key (Enc empty, bound by its framed
// absence); the phone reveals both its device signing and encryption keys.
type pairReveal struct {
	Sign string `json:"sign"`
	Enc  string `json:"enc,omitempty"`
}

// pairConfirm is the wire form of a side's proof-of-possession-and-consent: an
// ed25519 signature by its OWN signing key over the shared transcript.
type pairConfirm struct {
	Sig string `json:"sig"`
}

// Initiator drives the DESKTOP through the pairing. It is used in two steps, and
// the split is the human gate:
//
//	sas, err := in.Handshake(ctx, t)  // derive the string; pin NOTHING
//	// … a human compares sas against the phone's screen …
//	cfg, err := in.Confirm(ctx, t)    // only now: exchange PoP and pin
//
// A caller that never calls Confirm (because the strings differed) pins nobody.
type Initiator struct {
	transportKey ed25519.PrivateKey
	transportPub ed25519.PublicKey
	relayBase    string
	session      string
	salt         []byte

	// Learned during Handshake, consumed by Confirm.
	phonePub  ed25519.PublicKey
	phoneEnc  []byte
	handshook bool
}

// NewInitiator builds a desktop-side pairing from this machine's transport
// signing key and the rendezvous the invite carries. Generate the transport key
// once (it persists in the pairing config) and reuse it across re-pairings so the
// phone's pin stays valid.
func NewInitiator(transportKey ed25519.PrivateKey, relayBase, session string, salt []byte) (*Initiator, error) {
	if len(transportKey) != ed25519.PrivateKeySize {
		return nil, ErrWrongTransportKeyLen
	}
	if relayBase == "" || session == "" {
		return nil, fmt.Errorf("cruciform: pairing needs a relay and a session")
	}
	if len(salt) < pairing.MinSaltLen {
		return nil, fmt.Errorf("cruciform: pairing salt is %d bytes, want at least %d", len(salt), pairing.MinSaltLen)
	}
	return &Initiator{
		transportKey: transportKey,
		transportPub: transportKey.Public().(ed25519.PublicKey),
		relayBase:    relayBase,
		session:      session,
		salt:         append([]byte(nil), salt...),
	}, nil
}

// TransportID is this desktop's transport signing key ("ed25519:<hex>") — the id
// the phone pins.
func (in *Initiator) TransportID() string { return identity.FormatPublicKey(in.transportPub) }

// Handshake runs commit → reveal → open → derive and returns the SAS the human
// must compare. It signs and pins nothing. The order is the guarantee: the
// commitment is posted and the peer's fetched BEFORE any key is revealed, and the
// peer commitment is OPENED against the revealed keys before they are trusted.
func (in *Initiator) Handshake(ctx context.Context, t PairTransport) (pairing.SAS, error) {
	myCommit, err := pairing.Commit(in.transportPub, nil)
	if err != nil {
		return "", fmt.Errorf("cruciform: committing: %w", err)
	}
	if err := t.Post(ctx, pairMsgCommit, myCommit); err != nil {
		return "", err
	}
	peerCommit, err := t.Fetch(ctx, pairMsgCommit)
	if err != nil {
		return "", err
	}

	mine, err := json.Marshal(pairReveal{Sign: in.TransportID()})
	if err != nil {
		return "", fmt.Errorf("cruciform: encoding reveal: %w", err)
	}
	if err := t.Post(ctx, pairMsgReveal, mine); err != nil {
		return "", err
	}
	peerRaw, err := t.Fetch(ctx, pairMsgReveal)
	if err != nil {
		return "", err
	}
	var peer pairReveal
	if err := json.Unmarshal(peerRaw, &peer); err != nil {
		return "", fmt.Errorf("cruciform: decoding phone reveal: %w", err)
	}
	phonePub, err := identity.ParsePublicKey(peer.Sign)
	if err != nil {
		return "", fmt.Errorf("cruciform: phone device key: %w", err)
	}
	if len(phonePub) != ed25519.PublicKeySize {
		return "", fmt.Errorf("cruciform: phone device key is %d bytes, want %d", len(phonePub), ed25519.PublicKeySize)
	}
	phoneEncKey, err := encryption.ParsePublicKey(peer.Enc)
	if err != nil {
		return "", fmt.Errorf("cruciform: phone encryption key: %w", err)
	}
	phoneEnc := phoneEncKey.Bytes()

	// OPEN the phone's commitment against its revealed keys — the rushing gate. A
	// key that does not open the commitment it was posted under is refused.
	if err := pairing.Commitment(peerCommit).Open(phonePub, phoneEnc); err != nil {
		return "", fmt.Errorf("cruciform: phone commitment: %w", err)
	}

	sas, err := pairing.Derive(
		pairing.Keys{Sign: in.transportPub},
		pairing.Keys{Sign: phonePub, Enc: phoneEnc},
		in.salt,
	)
	if err != nil {
		return "", fmt.Errorf("cruciform: deriving SAS: %w", err)
	}

	in.phonePub = phonePub
	in.phoneEnc = phoneEnc
	in.handshook = true
	return sas, nil
}

// Confirm exchanges the mutual proof-of-possession-and-consent and returns the
// pinned pairing config. Call it ONLY after a human has confirmed the SAS matched;
// until then nothing is pinned. It posts this desktop's signature over the shared
// transcript, then fetches and verifies the phone's — proving the phone controls
// the device key it revealed and that its human also said yes — before returning.
func (in *Initiator) Confirm(ctx context.Context, t PairTransport) (*PairConfig, error) {
	if !in.handshook {
		return nil, ErrNotHandshaken
	}
	transcript := pairTranscript(in.session, in.salt, in.transportPub, nil, in.phonePub, in.phoneEnc)

	mySig := ed25519.Sign(in.transportKey, transcript)
	payload, err := json.Marshal(pairConfirm{Sig: base64.RawURLEncoding.EncodeToString(mySig)})
	if err != nil {
		return nil, fmt.Errorf("cruciform: encoding confirm: %w", err)
	}
	if err := t.Post(ctx, pairMsgConfirm, payload); err != nil {
		return nil, err
	}

	peerRaw, err := t.Fetch(ctx, pairMsgConfirm)
	if err != nil {
		return nil, err
	}
	var peer pairConfirm
	if err := json.Unmarshal(peerRaw, &peer); err != nil {
		return nil, fmt.Errorf("cruciform: decoding phone confirm: %w", err)
	}
	peerSig, err := base64.RawURLEncoding.DecodeString(peer.Sig)
	if err != nil {
		return nil, fmt.Errorf("%w: signature is not base64", ErrBadPeerConfirm)
	}
	if len(peerSig) != ed25519.SignatureSize || !ed25519.Verify(in.phonePub, transcript, peerSig) {
		return nil, ErrBadPeerConfirm
	}

	return &PairConfig{
		TransportKey: append(ed25519.PrivateKey(nil), in.transportKey...),
		PhonePub:     append(ed25519.PublicKey(nil), in.phonePub...),
		PhoneEnc:     append([]byte(nil), in.phoneEnc...),
		RelayBase:    in.relayBase,
	}, nil
}

// pairTranscript is the canonical byte string both sides sign at confirm: the
// domain tag then every public value of the pairing, length-framed so no byte can
// migrate across a boundary and leave the digest unchanged (the same framing
// discipline pairing.Derive uses). Binding the exact keys and session means a
// relay cannot lift a confirmation from another pairing and replay it here.
func pairTranscript(session string, salt []byte, initSign ed25519.PublicKey, initEnc []byte, respSign ed25519.PublicKey, respEnc []byte) []byte {
	var b bytes.Buffer
	b.WriteString(pairConfirmDomain)
	writeFramed(&b, []byte(session))
	writeFramed(&b, salt)
	writeFramed(&b, initSign)
	writeFramed(&b, initEnc)
	writeFramed(&b, respSign)
	writeFramed(&b, respEnc)
	return b.Bytes()
}

// writeFramed writes b preceded by its length as an 8-byte big-endian count.
func writeFramed(b *bytes.Buffer, f []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(f)))
	b.Write(n[:])
	b.Write(f)
}

// PairConfig is what a completed pairing pins on the desktop: this machine's
// persistent transport signing key (secret — it authenticates unwrap requests),
// the phone's device signing key (verifies unwrap responses), the phone's X25519
// encryption key (the RecipientID the sealed space-key copy is looked up by), and
// the relay the two share. The cruciform custody backend is built from it.
type PairConfig struct {
	TransportKey ed25519.PrivateKey
	PhonePub     ed25519.PublicKey
	PhoneEnc     []byte
	RelayBase    string
}

// RecipientID is the phone's "x25519:<hex>" wrap target — the copy of a space key
// the offload backend opens. A cruciform custody is built as this backend's
// Unwrapper plus this id, so create wraps to and open unwraps the phone's copy.
func (c *PairConfig) RecipientID() string { return encryption.FormatPublicKey(c.PhoneEnc) }

// pairConfigFile is the on-disk JSON form. The private transport key is stored
// base64; the public keys use the system's algorithm-prefixed hex.
type pairConfigFile struct {
	Version      int    `json:"version"`
	TransportKey string `json:"transport_key"`
	PhonePub     string `json:"phone_pub"`
	PhoneEnc     string `json:"phone_enc"`
	RelayBase    string `json:"relay_base"`
}

// MarshalJSON renders the pairing config to its on-disk form.
func (c *PairConfig) MarshalJSON() ([]byte, error) {
	if len(c.TransportKey) != ed25519.PrivateKeySize {
		return nil, ErrWrongTransportKeyLen
	}
	if len(c.PhonePub) != ed25519.PublicKeySize {
		return nil, ErrWrongPhoneKeyLen
	}
	return json.Marshal(pairConfigFile{
		Version:      pairConfigVersion,
		TransportKey: base64.RawURLEncoding.EncodeToString(c.TransportKey),
		PhonePub:     identity.FormatPublicKey(c.PhonePub),
		PhoneEnc:     encryption.FormatPublicKey(c.PhoneEnc),
		RelayBase:    c.RelayBase,
	})
}

// UnmarshalJSON parses and validates the on-disk pairing config.
func (c *PairConfig) UnmarshalJSON(raw []byte) error {
	var f pairConfigFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformedPairConfig, err)
	}
	if f.Version != pairConfigVersion {
		return fmt.Errorf("%w: version %d, want %d", ErrMalformedPairConfig, f.Version, pairConfigVersion)
	}
	tk, err := base64.RawURLEncoding.DecodeString(f.TransportKey)
	if err != nil || len(tk) != ed25519.PrivateKeySize {
		return fmt.Errorf("%w: transport key", ErrMalformedPairConfig)
	}
	phonePub, err := identity.ParsePublicKey(f.PhonePub)
	if err != nil || len(phonePub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: phone device key", ErrMalformedPairConfig)
	}
	phoneEnc, err := encryption.ParsePublicKey(f.PhoneEnc)
	if err != nil {
		return fmt.Errorf("%w: phone encryption key", ErrMalformedPairConfig)
	}
	if f.RelayBase == "" {
		return fmt.Errorf("%w: missing relay base", ErrMalformedPairConfig)
	}
	c.TransportKey = ed25519.PrivateKey(tk)
	c.PhonePub = phonePub
	c.PhoneEnc = phoneEnc.Bytes()
	c.RelayBase = f.RelayBase
	return nil
}
