// Package client is the DEVICE side of encrypted personal state (§40, §46, §73,
// ADR-0049). Where internal/personalstate/store is the peer's opaque storage,
// this is what an authorised device does that a peer cannot: mint a space key,
// seal it for the authorised devices and the recovery key, and — holding the key
// only in memory — encrypt and decrypt the space's changes.
//
// The space key never leaves this side. It is minted here, held in memory here,
// and handed to a peer only as WRAPPED copies (§41, §79); the peer stores those
// opaquely and cannot open them. Nothing in this package writes a space key to
// disk or hands it to a server.
//
// # The keystore constraint is built in, not retrofitted (#330)
//
// A real phone holds its device encryption key in a NON-EXPORTABLE keystore and
// does the ECDH inside the secure element — the private key never leaves it. So
// opening a space takes an [Unwrapper] INTERFACE, not a raw private key: the
// desktop CLI's exportable key ([KeyUnwrapper]) and a phone's enclave-backed
// unwrapper satisfy the same interface, and no code here changes when the phone
// arrives. This is ADR-0022's hardware-root revisit honoured while M9 is designed,
// not after.
package client

import (
	"crypto/ecdh"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
)

// ErrSpaceNotOpen is an encrypt/decrypt on a space whose key this device does not
// hold — it was never created here or opened from a wrapped copy.
var ErrSpaceNotOpen = errors.New("personalstate/client: space is not open on this device")

// Unwrapper turns a wrapped space key into a space key using a device's X25519
// encryption private key. It is an interface, not a raw key, because a phone's
// keystore key is non-exportable and does the ECDH in-enclave (#330): what a
// caller supplies is "something that can unwrap", never the private key itself.
type Unwrapper interface {
	Unwrap(wrapped []byte) (encryption.SpaceKey, error)
}

// KeyUnwrapper is the exportable-key stand-in: the desktop CLI's device key does
// the ECDH in-process (ADR-0032 — the CLI is the first device). A phone drops in
// an enclave-backed Unwrapper with the same interface and nothing else changes.
type KeyUnwrapper struct{ priv *ecdh.PrivateKey }

// NewKeyUnwrapper wraps an exportable X25519 private key as an [Unwrapper].
func NewKeyUnwrapper(priv *ecdh.PrivateKey) *KeyUnwrapper { return &KeyUnwrapper{priv: priv} }

// Unwrap implements [Unwrapper] by an in-process ECDH.
func (k *KeyUnwrapper) Unwrap(wrapped []byte) (encryption.SpaceKey, error) {
	return encryption.Unwrap(wrapped, k.priv)
}

// A Recipient is an authorised wrap target — a device or the recovery encryption
// key — by its rendered "x25519:<hex>" id and parsed public key.
type Recipient struct {
	ID  string
	Key *ecdh.PublicKey
}

// ParseRecipient parses a rendered recipient id into a [Recipient].
func ParseRecipient(id string) (Recipient, error) {
	key, err := encryption.ParsePublicKey(id)
	if err != nil {
		return Recipient{}, err
	}
	return Recipient{ID: id, Key: key}, nil
}

// A WrappedFor is one recipient's sealed copy of a space key, ready to hand to
// the peer store (which holds it opaquely and cannot open it).
type WrappedFor struct {
	Recipient string
	Wrapped   []byte
}

// Manager holds the space keys this device has open, in memory only. A zero
// Manager is unusable; construct with [New]. It is safe for concurrent use.
type Manager struct {
	mu   sync.RWMutex
	keys map[string]encryption.SpaceKey
}

// New returns an empty manager with no spaces open.
func New() *Manager { return &Manager{keys: make(map[string]encryption.SpaceKey)} }

// Create mints a new space and seals its key for every recipient, holding the
// key open on this device. It returns the space to record and the wrapped copies
// to push to the peer. Recipients are this device plus the other authorised
// devices and the recovery key; at least one is required, or the space could be
// read by no one — and this device should be among them, or it just wrote a
// space it cannot itself read (a caller's responsibility, not enforced here).
func (m *Manager) Create(kind spaces.Kind, now time.Time, recipients []Recipient) (spaces.EncryptedSpace, []WrappedFor, error) {
	if len(recipients) == 0 {
		return spaces.EncryptedSpace{}, nil, errors.New("personalstate/client: a space needs at least one recipient, or nobody could read it")
	}
	sp, err := spaces.NewSpace(kind, now)
	if err != nil {
		return spaces.EncryptedSpace{}, nil, err
	}
	key, err := encryption.NewSpaceKey()
	if err != nil {
		return spaces.EncryptedSpace{}, nil, err
	}
	wrapped := make([]WrappedFor, 0, len(recipients))
	for _, r := range recipients {
		if r.Key == nil {
			return spaces.EncryptedSpace{}, nil, fmt.Errorf("personalstate/client: recipient %q has no key", r.ID)
		}
		w, err := encryption.Seal(key, r.Key)
		if err != nil {
			return spaces.EncryptedSpace{}, nil, fmt.Errorf("personalstate/client: sealing for %s: %w", r.ID, err)
		}
		wrapped = append(wrapped, WrappedFor{Recipient: r.ID, Wrapped: w})
	}
	m.mu.Lock()
	m.keys[sp.ID] = key
	m.mu.Unlock()
	return sp, wrapped, nil
}

// Rotate mints a FRESH key for an already-open space and seals it for the given
// recipients — the forward-looking half of device revocation (§41, ADR-0022,
// ADR-0049). The recipients are the ones that REMAIN authorised: the revoked
// device is simply left out, so it is not sealed the new key and cannot read
// anything encrypted under it from here on. The new key replaces the old in
// memory, so this device's subsequent Encrypt uses it; past changes under the old
// key stay readable to whoever already held it (revocation is forward-looking,
// not retroactive — that honesty is ADR-0022's, kept here).
//
// The space must be open (only a device that can read a space may re-key it), and
// at least one recipient is required, or the space would be re-keyed for no one.
// It returns the new wrapped copies to push to the peer, replacing the remaining
// recipients' copies; the caller deletes the revoked device's stored copy
// separately (store.DeleteWrappedKey).
func (m *Manager) Rotate(spaceID string, recipients []Recipient) ([]WrappedFor, error) {
	if !m.IsOpen(spaceID) {
		return nil, fmt.Errorf("%w: %s", ErrSpaceNotOpen, spaceID)
	}
	if len(recipients) == 0 {
		return nil, errors.New("personalstate/client: a rotation needs at least one recipient, or the space is re-keyed for no one")
	}
	key, err := encryption.NewSpaceKey()
	if err != nil {
		return nil, err
	}
	wrapped := make([]WrappedFor, 0, len(recipients))
	for _, r := range recipients {
		if r.Key == nil {
			return nil, fmt.Errorf("personalstate/client: recipient %q has no key", r.ID)
		}
		w, err := encryption.Seal(key, r.Key)
		if err != nil {
			return nil, fmt.Errorf("personalstate/client: sealing for %s: %w", r.ID, err)
		}
		wrapped = append(wrapped, WrappedFor{Recipient: r.ID, Wrapped: w})
	}
	m.mu.Lock()
	m.keys[spaceID] = key
	m.mu.Unlock()
	return wrapped, nil
}

// Open recovers a space key from the wrapped copy this device holds, via the
// [Unwrapper], and remembers it so the device can read the space. Idempotent.
func (m *Manager) Open(spaceID string, wrapped []byte, u Unwrapper) error {
	if u == nil {
		return errors.New("personalstate/client: an unwrapper is required")
	}
	key, err := u.Unwrap(wrapped)
	if err != nil {
		return fmt.Errorf("personalstate/client: opening space %s: %w", spaceID, err)
	}
	m.mu.Lock()
	m.keys[spaceID] = key
	m.mu.Unlock()
	return nil
}

// Encrypt seals a change under an open space's key, ready to ship to the peer as
// an opaque blob (§42). The space must be open.
func (m *Manager) Encrypt(spaceID string, plaintext []byte) ([]byte, error) {
	key, ok := m.key(spaceID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrSpaceNotOpen, spaceID)
	}
	return encryption.EncryptChange(key, plaintext)
}

// Decrypt opens a change fetched from the peer, under an open space's key.
func (m *Manager) Decrypt(spaceID string, ciphertext []byte) ([]byte, error) {
	key, ok := m.key(spaceID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrSpaceNotOpen, spaceID)
	}
	return encryption.DecryptChange(key, ciphertext)
}

// IsOpen reports whether this device holds the given space's key.
func (m *Manager) IsOpen(spaceID string) bool {
	_, ok := m.key(spaceID)
	return ok
}

// Close forgets a space's key — on lock, or when this device is revoked from the
// space. The wrapped copies the peer holds are untouched; this only drops the
// in-memory key.
func (m *Manager) Close(spaceID string) {
	m.mu.Lock()
	delete(m.keys, spaceID)
	m.mu.Unlock()
}

func (m *Manager) key(spaceID string) (encryption.SpaceKey, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.keys[spaceID]
	return k, ok
}
