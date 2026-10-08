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

	"github.com/rarebit-one/void-which-binds-go/custody"
	"github.com/rarebit-one/void-which-binds-go/encryption"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
)

// ErrSpaceNotOpen is an encrypt/decrypt on a space whose key this device does not
// hold — it was never created here or opened from a wrapped copy.
var ErrSpaceNotOpen = errors.New("personalstate/client: space is not open on this device")

// The custody seam is void-which-binds-go's custody package (its ADR-0021):
// these are aliases of its types, so every caller here compiles unchanged and a
// library custody backend (a sealed-file device's holder, say) is a Custody
// with no adapter. Call sites move to the library names as they are touched.

// Unwrapper turns a wrapped space key into a space key using a device's X25519
// encryption private key. It is an interface, not a raw key, because a phone's
// keystore key is non-exportable and does the ECDH in-enclave (#330): what a
// caller supplies is "something that can unwrap", never the private key itself.
type Unwrapper = custody.Unwrapper

// Custody is an [Unwrapper] that also knows its own wrap-target id — the
// "x25519:<hex>" recipient a space key must be sealed to for this backend to open
// it. Opening a space needs both halves: the id, to pick out the copy the
// controller wrapped for this device, and the Unwrapper, to open it. Making a
// per-platform custody backend (ADR-0098) supply both lets the device gateway and
// the vault CLI SELECT one — software, sealed file, YubiKey-on-card, TPM-gated,
// cruciform — without either caller changing, and keeps the create side (which
// wraps to this same id) consistent with the open side. It is the library's
// custody.Holder.
type Custody = custody.Holder

// KeyUnwrapper is the exportable-key stand-in: the desktop CLI's software device
// key does the ECDH in-process (ADR-0032 — the CLI is the first device). It is
// the library's software backend, custody.KeyUnwrapper.
type KeyUnwrapper = custody.KeyUnwrapper

// NewKeyUnwrapper wraps an exportable X25519 private key as a [Custody]
// (custody.NewKeyUnwrapper).
func NewKeyUnwrapper(priv *ecdh.PrivateKey) *KeyUnwrapper { return custody.NewKeyUnwrapper(priv) }

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
//
// Each open space is a KEYRING (ADR-0103): the current key, its epoch, and every
// earlier key back to epoch 0. Encrypt always seals under the current key;
// Decrypt opens content sealed under any key on the ring, so content written
// before a rotation — or by a writer racing one — stays readable.
type Manager struct {
	mu    sync.RWMutex
	rings map[string]keyring
}

// keyring is one open space's keys: keys[0] is the current key, at epoch, and
// keys[i] is the key of epoch-i. It is never mutated in place — a rotation
// stores a new ring — so a reader holding an old slice is never raced.
type keyring struct {
	epoch int
	keys  []encryption.SpaceKey
}

// New returns an empty manager with no spaces open.
func New() *Manager { return &Manager{rings: make(map[string]keyring)} }

// Create mints a new space and seals its key for every recipient, holding the
// key open on this device. It returns the space to record and the wrapped copies
// to push to the peer. Recipients are this device plus the other authorised
// devices and the recovery key; at least one is required, or the space could be
// read by no one — and this device should be among them, or it just wrote a
// space it cannot itself read (a caller's responsibility, not enforced here).
// The new space is at key epoch 0.
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
	wrapped, err := sealFor(key, recipients)
	if err != nil {
		return spaces.EncryptedSpace{}, nil, err
	}
	m.mu.Lock()
	m.rings[sp.ID] = keyring{epoch: 0, keys: []encryption.SpaceKey{key}}
	m.mu.Unlock()
	return sp, wrapped, nil
}

// A Rotation is what [Manager.Rotate] hands back for the peer
// (POST /spaces/{id}/rotate, ADR-0103): the new key's wrapped copies, the
// previous current key sealed under the new one (the history row), and the
// epoch the space moves to. The caller names Epoch-1 as the rotation's expected
// epoch.
type Rotation struct {
	Epoch      int
	SealedPrev []byte
	Wrapped    []WrappedFor
}

// Rotate mints a FRESH key for an already-open space, seals it for the given
// recipients, and seals the previous current key under it — a pure re-key
// (§41, ADR-0049, ADR-0103). The recipients are the ones that REMAIN authorised:
// a revoked device is simply left out, so it is not sealed the new key and
// cannot read anything encrypted under it from here on. Nothing is
// re-encrypted: the new key becomes this device's current key (so Encrypt uses
// it), and the old one moves onto the ring, so this device — and any remaining
// recipient, through the history row — still reads every earlier change.
// Revocation is forward-looking, not retroactive (ADR-0022): the revoked device
// held the old keys already.
//
// The space must be open (only a device that can read a space may re-key it),
// and at least one recipient is required, or the space would be re-keyed for no
// one. The manager holds the new key from here on even if the peer then refuses
// the rotation (a 409 because another landed first); a caller that loses that
// race discards this manager and re-opens the space.
func (m *Manager) Rotate(spaceID string, recipients []Recipient) (Rotation, error) {
	ring, ok := m.ring(spaceID)
	if !ok {
		return Rotation{}, fmt.Errorf("%w: %s", ErrSpaceNotOpen, spaceID)
	}
	if len(recipients) == 0 {
		return Rotation{}, errors.New("personalstate/client: a rotation needs at least one recipient, or the space is re-keyed for no one")
	}
	key, err := encryption.NewSpaceKey()
	if err != nil {
		return Rotation{}, err
	}
	wrapped, err := sealFor(key, recipients)
	if err != nil {
		return Rotation{}, err
	}
	sealedPrev, err := SealPrevious(key, ring.keys[0])
	if err != nil {
		return Rotation{}, err
	}
	next := keyring{epoch: ring.epoch + 1, keys: append([]encryption.SpaceKey{key}, ring.keys...)}
	m.mu.Lock()
	m.rings[spaceID] = next
	m.mu.Unlock()
	return Rotation{Epoch: next.epoch, SealedPrev: sealedPrev, Wrapped: wrapped}, nil
}

// sealFor wraps key for every recipient.
func sealFor(key encryption.SpaceKey, recipients []Recipient) ([]WrappedFor, error) {
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
	return wrapped, nil
}

// Open recovers a space key from the wrapped copy this device holds, via the
// [Unwrapper], and remembers it so the device can read the space. It is for a
// space at key epoch 0 — one never rotated; a rotated space opens with
// [Manager.OpenWithHistory]. Idempotent.
func (m *Manager) Open(spaceID string, wrapped []byte, u Unwrapper) error {
	return m.OpenWithHistory(spaceID, wrapped, 0, nil, u)
}

// OpenWithHistory recovers the space key of epoch from this device's wrapped
// copy and unrolls the space's key history back to epoch 0 (ADR-0103), so the
// device reads content sealed under every earlier key. history is the space's
// rows as the peer serves them (any order); every epoch 1..epoch must be
// present, or the open fails rather than leaving content silently unreadable.
// Idempotent.
func (m *Manager) OpenWithHistory(spaceID string, wrapped []byte, epoch int, history []HistoryEntry, u Unwrapper) error {
	if u == nil {
		return errors.New("personalstate/client: an unwrapper is required")
	}
	key, err := u.Unwrap(wrapped)
	if err != nil {
		return fmt.Errorf("personalstate/client: opening space %s: %w", spaceID, err)
	}
	return m.Load(spaceID, key, epoch, history)
}

// Load opens a space from its CURRENT key held in the clear — a key recovered
// from the paper secret (internal/personalstate/spacerecover), not unwrapped by
// this device's custody — unrolling history exactly as
// [Manager.OpenWithHistory] does.
func (m *Manager) Load(spaceID string, current encryption.SpaceKey, epoch int, history []HistoryEntry) error {
	keys, err := Unroll(current, epoch, history)
	if err != nil {
		return fmt.Errorf("personalstate/client: opening space %s: %w", spaceID, err)
	}
	m.mu.Lock()
	m.rings[spaceID] = keyring{epoch: epoch, keys: keys}
	m.mu.Unlock()
	return nil
}

// Encrypt seals a change under an open space's CURRENT key, ready to ship to the
// peer as an opaque blob (§42). The space must be open.
func (m *Manager) Encrypt(spaceID string, plaintext []byte) ([]byte, error) {
	ring, ok := m.ring(spaceID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrSpaceNotOpen, spaceID)
	}
	return encryption.EncryptChange(ring.keys[0], plaintext)
}

// Decrypt opens a change fetched from the peer under whichever key on the
// space's ring sealed it, trying the current key first and then each earlier
// one, newest to oldest (the AEAD refuses a wrong key). When none opens it, the
// error is the current key's — the same opaque refusal a single-key space gave.
func (m *Manager) Decrypt(spaceID string, ciphertext []byte) ([]byte, error) {
	ring, ok := m.ring(spaceID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrSpaceNotOpen, spaceID)
	}
	var first error
	for _, k := range ring.keys {
		pt, err := encryption.DecryptChange(k, ciphertext)
		if err == nil {
			return pt, nil
		}
		if first == nil {
			first = err
		}
	}
	return nil, first
}

// IsOpen reports whether this device holds the given space's key.
func (m *Manager) IsOpen(spaceID string) bool {
	_, ok := m.ring(spaceID)
	return ok
}

// SpaceKey returns an open space's CURRENT key so the vault content path
// (internal/personalstate/vaultframe, vaultread) can seal fixed ciphertext frames
// directly under it, rather than through the change-oriented
// [Manager.Encrypt]/[Manager.Decrypt]. A reader wants [Manager.Keys], since a
// file sealed before a rotation is under an earlier key. ok is false when this
// device does not hold the key — it was never created here or opened from a
// wrapped copy. The key stays in memory on this device (§40): a caller must not
// persist it or hand it to a peer.
func (m *Manager) SpaceKey(spaceID string) (key encryption.SpaceKey, ok bool) {
	ring, ok := m.ring(spaceID)
	if !ok {
		return encryption.SpaceKey{}, false
	}
	return ring.keys[0], true
}

// Keys returns every key on an open space's ring, newest (the current key)
// first, for a vault reader that must try each against a manifest. The same
// in-memory rule as [Manager.SpaceKey] applies. The slice is the caller's.
func (m *Manager) Keys(spaceID string) ([]encryption.SpaceKey, bool) {
	ring, ok := m.ring(spaceID)
	if !ok {
		return nil, false
	}
	return append([]encryption.SpaceKey(nil), ring.keys...), true
}

// Epoch returns an open space's current key epoch (ADR-0103) — the epoch a
// rotation names as expected, and the one a new recipient's copy is wrapped at.
func (m *Manager) Epoch(spaceID string) (int, bool) {
	ring, ok := m.ring(spaceID)
	return ring.epoch, ok
}

// Close forgets a space's keys — on lock, or when this device is revoked from
// the space. The wrapped copies the peer holds are untouched; this only drops
// the in-memory ring.
func (m *Manager) Close(spaceID string) {
	m.mu.Lock()
	delete(m.rings, spaceID)
	m.mu.Unlock()
}

func (m *Manager) ring(spaceID string) (keyring, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.rings[spaceID]
	return r, ok
}
