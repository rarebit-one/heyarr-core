package yubikey

import (
	"fmt"

	"github.com/rarebit-one/voidbind-go/encryption"
)

// PINFunc yields the OpenPGP User PIN (PW1) that gates PSO:DECIPHER. Production
// wires it to a secure prompt (pinentry) or a session cache; a test supplies a
// constant. The PIN gates the card; it is never persisted by this package.
type PINFunc func() (string, error)

// Unwrapper is the ADR-0098 YubiKey-on-card device-key custody backend. It
// implements personalstate/client.Unwrapper by driving the card's cv25519
// PSO:DECIPHER for the one ECDH an unwrap needs, so the X25519 private key never
// leaves the token. The public half is read once at construction and passed to
// UnwrapWithAgreement as the recipient key that binds the HKDF salt and AEAD AAD.
type Unwrapper struct {
	socket string  // gpg-agent Assuan socket ("" = discover via gpgconf)
	pub    []byte  // the card's 32-byte X25519 public point
	pin    PINFunc // gates the on-card decipher
}

// New opens the gpg-agent, reads the card's cv25519 public point (OPENPGP.2, the
// decryption key), and returns a ready Unwrapper. It fails if no OpenPGP card is
// present or its encryption slot is not a cv25519 key.
func New(socket string, pin PINFunc) (*Unwrapper, error) {
	if pin == nil {
		return nil, fmt.Errorf("yubikey: a PINFunc is required")
	}
	s, err := dialSCD(socket)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.close() }()
	sexp, err := s.cmd("SCD READKEY OPENPGP.2")
	if err != nil {
		return nil, fmt.Errorf("yubikey: reading the card public key (is a cv25519 OpenPGP card present?): %w", err)
	}
	pub, err := parsePubkeyPoint(sexp)
	if err != nil {
		return nil, err
	}
	return &Unwrapper{socket: socket, pub: pub, pin: pin}, nil
}

// PublicKey returns a copy of the card's X25519 public point — the device
// encryption key a controller wraps space keys to (register it via
// encryption.FormatPublicKey).
func (u *Unwrapper) PublicKey() []byte {
	out := make([]byte, len(u.pub))
	copy(out, u.pub)
	return out
}

// RecipientID reports the card's "x25519:<hex>" wrap-target id, so Unwrapper is a
// personalstate/client.Custody — selectable at the gateway and vault CLI.
func (u *Unwrapper) RecipientID() string {
	return encryption.FormatPublicKey(u.pub)
}

// Unwrap recovers a space key wrapped to this card, running the ECDH on-card.
func (u *Unwrapper) Unwrap(wrapped []byte) (encryption.SpaceKey, error) {
	s, err := dialSCD(u.socket)
	if err != nil {
		return encryption.SpaceKey{}, err
	}
	defer func() { _ = s.close() }()
	if err := u.verify(s); err != nil {
		return encryption.SpaceKey{}, err
	}
	return encryption.UnwrapWithAgreement(wrapped, u.pub, func(ephPub []byte) ([]byte, error) {
		return decipher(s, ephPub)
	})
}

// verify presents the User PIN (PW1 mode 82, for PSO:DECIPHER) to the card.
func (u *Unwrapper) verify(s *scd) error {
	pin, err := u.pin()
	if err != nil {
		return fmt.Errorf("yubikey: obtaining the card PIN: %w", err)
	}
	b := []byte(pin)
	resp, err := s.apdu(fmt.Sprintf("00200082%02X%X", len(b), b))
	if err != nil {
		return err
	}
	if _, err := checkSW(resp, "VERIFY PW1"); err != nil {
		return err
	}
	return nil
}

// decipher runs PSO:DECIPHER for one X25519 agreement and returns the 32-byte
// shared secret ECDH(card_priv, ephPub).
func decipher(s *scd, ephPub []byte) ([]byte, error) {
	do, err := decipherDO(ephPub)
	if err != nil {
		return nil, err
	}
	resp, err := s.apdu(fmt.Sprintf("002A8086%02X%s00", len(do)/2, do))
	if err != nil {
		return nil, err
	}
	shared, err := checkSW(resp, "PSO:DECIPHER")
	if err != nil {
		return nil, err
	}
	if len(shared) != 32 {
		return nil, fmt.Errorf("yubikey: PSO:DECIPHER returned %d bytes, want a 32-byte shared secret", len(shared))
	}
	return shared, nil
}
