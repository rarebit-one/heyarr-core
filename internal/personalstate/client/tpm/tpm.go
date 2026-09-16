// Package tpm is the ADR-0098 TPM-gated device-key custody backend. The device's
// X25519 encryption seed is SEALED to a TPM 2.0 under a policy that is
// PolicyPCR(selection) AND PolicyAuthValue(PIN); it is released only after that
// gate, and the ECDH then runs in RAM. TPM 2.0 has no Curve25519, so the TPM
// GATES the seed — it does not compute X25519 (mirrors voidbind ADR-0001, and
// unlike the yubikey backend where the key never leaves the card).
//
// RecipientID reads the recorded public point from the sealed-key blob, with NO
// TPM interaction, so selecting the wrapped copy of a space key never triggers
// the gate; only Unwrap does. The fingerprint reader (fprintd/PAM) is an
// authorization gate in front of this, never a key holder.
package tpm

import (
	"fmt"

	"github.com/google/go-tpm/tpm2/transport"
	"github.com/google/go-tpm/tpm2/transport/linuxtpm"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
)

// PINFunc yields the TPM policy PIN — the sealed object's authValue. Production
// wires it to a prompt, a pin file, or an env var; a test supplies a constant. It
// gates the unseal and is never persisted by this package.
type PINFunc func() (string, error)

// Opener opens a connection to a TPM 2.0. Production uses [OpenDevice]; a test
// wires a swtpm (or simulator) transport. Unwrap opens per call and closes after,
// so the backend never holds the device open.
type Opener func() (transport.TPMCloser, error)

// Unwrapper implements personalstate/client.Custody by unsealing the device seed
// from the TPM after the PCR+PIN gate and running the ECDH in-process.
type Unwrapper struct {
	blob   Blob
	pin    PINFunc
	opener Opener
}

// New builds the backend from a sealed-key [Blob] (provisioned by [Seal]) and a
// PIN source.
func New(blob Blob, pin PINFunc, opener Opener) (*Unwrapper, error) {
	if pin == nil {
		return nil, fmt.Errorf("tpm: a PINFunc is required")
	}
	if opener == nil {
		return nil, fmt.Errorf("tpm: a TPM opener is required")
	}
	if len(blob.Public) != 32 {
		return nil, fmt.Errorf("tpm: the sealed-key blob has no X25519 public point")
	}
	return &Unwrapper{blob: blob, pin: pin, opener: opener}, nil
}

// RecipientID reports the sealed key's "x25519:<hex>" id, read from the blob with
// no TPM interaction — so it is a client.Custody and selecting the wrapped copy
// never triggers the gate.
func (u *Unwrapper) RecipientID() string {
	return encryption.FormatPublicKey(u.blob.Public)
}

// Unwrap recovers a space key wrapped to this device by unsealing the seed after
// the TPM gate and running the ECDH in RAM.
func (u *Unwrapper) Unwrap(wrapped []byte) (encryption.SpaceKey, error) {
	pin, err := u.pin()
	if err != nil {
		return encryption.SpaceKey{}, fmt.Errorf("tpm: obtaining the PIN: %w", err)
	}
	tpm, err := u.opener()
	if err != nil {
		return encryption.SpaceKey{}, err
	}
	defer func() { _ = tpm.Close() }()

	seed, err := unsealSeed(tpm, u.blob, []byte(pin))
	if err != nil {
		return encryption.SpaceKey{}, err
	}
	priv, err := encryption.NewPrivateKey(seed)
	if err != nil {
		return encryption.SpaceKey{}, fmt.Errorf("tpm: the unsealed seed is not a valid X25519 key: %w", err)
	}
	return encryption.Unwrap(wrapped, priv)
}

// OpenDevice is the production [Opener]: it opens the kernel TPM resource-manager
// device (default /dev/tpmrm0), which serialises access so this coexists with
// other TPM users.
func OpenDevice(path string) Opener {
	if path == "" {
		path = "/dev/tpmrm0"
	}
	return func() (transport.TPMCloser, error) {
		t, err := linuxtpm.Open(path)
		if err != nil {
			return nil, fmt.Errorf("tpm: opening %s (is a TPM present?): %w", path, err)
		}
		return t, nil
	}
}

var _ client.Custody = (*Unwrapper)(nil)
