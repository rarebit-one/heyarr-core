package spacerecover

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/rarebit-one/voidbind-go/encryption"
	"github.com/rarebit-one/voidbind-go/identity"
	"github.com/rarebit-one/voidbind-go/recovery"
)

// BlobFormat names and versions the exported recovery blob (ADR-0022 addendum,
// 2026-09-25). It is both the file's leading magic and a field inside the sealed
// body, so a reader refuses a file of another format or version before and after
// opening it.
const BlobFormat = "heyarr-recovery-blob-v1"

// blobMagic prefixes every blob file: the format name and a NUL.
var blobMagic = []byte(BlobFormat + "\x00")

// ErrNotABlob is a file that is not a recovery blob of this version.
var ErrNotABlob = errors.New("spacerecover: not a " + BlobFormat + " file")

// ErrBlobSecret is a blob this recovery secret does not open: the wrong secret,
// or a damaged file. Like Unwrap, it deliberately does not say which.
var ErrBlobSecret = errors.New("spacerecover: this recovery secret does not open the blob")

// Blob is the exported recovery blob: every space's key copy that is wrapped for
// the user's recovery encryption key, gathered into one small file the user can
// keep anywhere (ADR-0022's "exported recovery blob"). Each Wrapped entry is the
// same encryption.Seal output the control database stores in wrapped_keys, so
// the blob adds no new key material. It lets recovery proceed without the control
// database that normally holds those copies.
//
// The whole blob is sealed to the recovery PUBLIC key ([SealBlob]), so any node
// or device can export it without the secret, and only the secret opens it
// ([OpenBlob]). That same property means anyone who knows the public key can MINT
// a blob. A blob is therefore a durability aid, not an authority: a key taken
// from one must be verified before it is trusted for writing (see the space
// recover command's re-wrap check).
type Blob struct {
	// Format is BlobFormat; set by SealBlob, checked by OpenBlob.
	Format string `json:"format"`
	// UserID is the user identity the recovery key belongs to ("ed25519:<hex>"),
	// when the exporter knew it. Informational; OpenBlob checks it against the
	// identity the secret derives when present.
	UserID string `json:"user_id,omitempty"`
	// RecoveryRecipient is the recovery encryption key ("x25519:<hex>") the blob
	// and every wrapped copy in it are sealed for.
	RecoveryRecipient string `json:"recovery_recipient"`
	// GeneratedAt is when the blob was exported. A space created or re-keyed
	// after it is missing from, or stale in, this blob.
	GeneratedAt time.Time `json:"generated_at"`
	// Spaces holds one wrapped copy per space, sorted by id.
	Spaces []BlobSpace `json:"spaces"`
}

// BlobSpace is one space's recovery-wrapped key copy.
type BlobSpace struct {
	SpaceID string `json:"space_id"`
	// Kind is the space's structural category (§39), so a recovered space can be
	// recreated with its kind.
	Kind string `json:"kind"`
	// Wrapped is the encryption.Seal output for the recovery key.
	Wrapped []byte `json:"wrapped"`
}

// SealBlob serialises b and seals it for b.RecoveryRecipient: a fresh one-time
// key is sealed to the recipient with encryption.Seal and encrypts the JSON body
// with encryption.EncryptChange. The file is
//
//	BlobFormat ‖ 0x00 ‖ uint32be(len(sealedKey)) ‖ sealedKey ‖ ciphertext
//
// No secret is needed, only the recipient's public key.
func SealBlob(b Blob) ([]byte, error) {
	b.Format = BlobFormat
	pub, err := encryption.ParsePublicKey(b.RecoveryRecipient)
	if err != nil {
		return nil, fmt.Errorf("spacerecover: the blob's recovery recipient: %w", err)
	}
	sort.Slice(b.Spaces, func(i, j int) bool { return b.Spaces[i].SpaceID < b.Spaces[j].SpaceID })
	body, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("spacerecover: encoding the blob: %w", err)
	}
	k, err := encryption.NewSpaceKey()
	if err != nil {
		return nil, err
	}
	sealedKey, err := encryption.Seal(k, pub)
	if err != nil {
		return nil, fmt.Errorf("spacerecover: sealing the blob key: %w", err)
	}
	ct, err := encryption.EncryptChange(k, body)
	if err != nil {
		return nil, fmt.Errorf("spacerecover: encrypting the blob: %w", err)
	}
	var out bytes.Buffer
	out.Write(blobMagic)
	_ = binary.Write(&out, binary.BigEndian, uint32(len(sealedKey))) // #nosec G115 -- a sealed 32-byte key is well under 4 GiB
	out.Write(sealedKey)
	out.Write(ct)
	return out.Bytes(), nil
}

// OpenBlob opens a blob with the recovery secret, offline. It refuses a file of
// another format ([ErrNotABlob]), one the secret does not open ([ErrBlobSecret]),
// and one whose recorded recipient or user is not the secret's. The keys inside
// are still wrapped: pass [Blob.Wrapped] to [UnwrapAll].
func OpenBlob(secret recovery.Secret, data []byte) (Blob, error) {
	rest, ok := bytes.CutPrefix(data, blobMagic)
	if !ok || len(rest) < 4 {
		return Blob{}, ErrNotABlob
	}
	n := binary.BigEndian.Uint32(rest[:4])
	rest = rest[4:]
	if uint64(n) > uint64(len(rest)) {
		return Blob{}, ErrNotABlob
	}
	sealedKey, ct := rest[:n], rest[n:]

	seed := recovery.DeriveUserEncryptionSeed(secret)
	defer clear(seed)
	priv, err := encryption.NewPrivateKey(seed)
	if err != nil {
		return Blob{}, fmt.Errorf("spacerecover: deriving the recovery encryption key: %w", err)
	}
	k, err := encryption.Unwrap(sealedKey, priv)
	if err != nil {
		return Blob{}, ErrBlobSecret
	}
	body, err := encryption.DecryptChange(k, ct)
	if err != nil {
		return Blob{}, ErrBlobSecret
	}
	var b Blob
	if err := json.Unmarshal(body, &b); err != nil || b.Format != BlobFormat {
		return Blob{}, ErrNotABlob
	}
	if want := encryption.FormatPublicKey(priv.PublicKey().Bytes()); b.RecoveryRecipient != want {
		return Blob{}, fmt.Errorf("spacerecover: the blob names recovery key %s, but this secret's is %s", b.RecoveryRecipient, want)
	}
	if b.UserID != "" {
		userSeed := recovery.DeriveUserSeed(secret)
		defer clear(userSeed)
		pub := ed25519.NewKeyFromSeed(userSeed).Public().(ed25519.PublicKey)
		if want := identity.FormatPublicKey(pub); b.UserID != want {
			return Blob{}, fmt.Errorf("spacerecover: the blob names user %s, but this secret's is %s", b.UserID, want)
		}
	}
	return b, nil
}

// Wrapped returns the blob's copies keyed by space id: the input [UnwrapAll] takes.
func (b Blob) Wrapped() map[string][]byte {
	out := make(map[string][]byte, len(b.Spaces))
	for _, s := range b.Spaces {
		out[s.SpaceID] = s.Wrapped
	}
	return out
}
