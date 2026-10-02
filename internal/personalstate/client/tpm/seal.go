package tpm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport"
)

// The sealing core (validated against a TPM 2.0 in a spike, then mirrored here):
// the device's X25519 SEED is sealed to the TPM under a policy that is
// PolicyPCR(selection) AND PolicyAuthValue(PIN). The object is policy-only
// (UserWithAuth clear), so unsealing requires satisfying the PCR state AND
// presenting the PIN. TPM 2.0 has no Curve25519, so it GATES the seed; the ECDH
// runs in RAM after the unseal (ADR-0098).
//
// Sealing itself (Seal) now lives only in the tpmsim test build: `heyarr device
// seal-tpm` is disabled until void-which-binds-go's custody/tpm lands
// (ADR-0021), so nothing in production writes this legacy blob any more. This
// package still opens an existing one, for the vault's tpm backend.

const (
	blobVersion  byte = 1
	blobMagic         = "heyarr-tpm-sealed-x25519-v1\x00"
	maxBlobField      = 8 << 10 // a sealed X25519 seed's TPM2B parts are small
)

// Blob is the on-disk sealed key: everything needed to reconstruct the policy and
// unseal, plus the X25519 PUBLIC point so RecipientID works without unsealing.
// The public point and the ciphertext are safe at rest; the seed is only ever in
// the TPM's sealed object or, briefly, in RAM after the gate.
type Blob struct {
	Public     []byte // X25519 public point (32 bytes) — the wrap target
	PCRs       tpm2.TPMLPCRSelection
	SealedPub  tpm2.TPM2BPublic
	SealedPriv tpm2.TPM2BPrivate
}

// PCRSelection builds a SHA-256 PCR selection over the given PCR indices.
func PCRSelection(pcrs ...uint) tpm2.TPMLPCRSelection {
	return tpm2.TPMLPCRSelection{PCRSelections: []tpm2.TPMSPCRSelection{{
		Hash:      tpm2.TPMAlgSHA256,
		PCRSelect: tpm2.PCClientCompatible.PCRs(pcrs...),
	}}}
}

// unsealSeed loads the sealed blob under the SRK, satisfies the policy (PCR + the
// PIN), and unseals — returning the raw X25519 seed into RAM.
func unsealSeed(tpm transport.TPM, b Blob, pin []byte) ([]byte, error) {
	parent, cleanup, err := primarySRK(tpm)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	ld, err := (tpm2.Load{ParentHandle: parent, InPrivate: b.SealedPriv, InPublic: b.SealedPub}).Execute(tpm)
	if err != nil {
		return nil, fmt.Errorf("tpm: loading the sealed key: %w", err)
	}
	defer func() { _, _ = (tpm2.FlushContext{FlushHandle: ld.ObjectHandle}).Execute(tpm) }()

	sess, closeSess, err := tpm2.PolicySession(tpm, tpm2.TPMAlgSHA256, 16, tpm2.Auth(pin))
	if err != nil {
		return nil, fmt.Errorf("tpm: policy session: %w", err)
	}
	defer func() { _ = closeSess() }()
	if _, err := (tpm2.PolicyPCR{PolicySession: sess.Handle(), Pcrs: b.PCRs}).Execute(tpm); err != nil {
		return nil, fmt.Errorf("tpm: PolicyPCR (has the boot state changed?): %w", err)
	}
	if _, err := (tpm2.PolicyAuthValue{PolicySession: sess.Handle()}).Execute(tpm); err != nil {
		return nil, fmt.Errorf("tpm: PolicyAuthValue: %w", err)
	}
	u, err := (tpm2.Unseal{ItemHandle: tpm2.AuthHandle{Handle: ld.ObjectHandle, Name: ld.Name, Auth: sess}}).Execute(tpm)
	if err != nil {
		return nil, fmt.Errorf("tpm: unseal (wrong PIN, or PCR policy not met): %w", err)
	}
	return u.OutData.Buffer, nil
}

// primarySRK deterministically derives the storage root key on the owner
// hierarchy (the standard ECC-P256 SRK template), the parent the sealed object
// loads under. It is stable across boots as long as the owner seed is unchanged.
func primarySRK(tpm transport.TPM) (tpm2.AuthHandle, func(), error) {
	p, err := (tpm2.CreatePrimary{
		PrimaryHandle: tpm2.TPMRHOwner,
		InPublic:      tpm2.New2B(tpm2.ECCSRKTemplate),
	}).Execute(tpm)
	if err != nil {
		return tpm2.AuthHandle{}, nil, fmt.Errorf("tpm: creating the SRK: %w", err)
	}
	h := tpm2.AuthHandle{Handle: p.ObjectHandle, Name: p.Name, Auth: tpm2.PasswordAuth(nil)}
	return h, func() { _, _ = (tpm2.FlushContext{FlushHandle: p.ObjectHandle}).Execute(tpm) }, nil
}

// Marshal serialises a Blob to the on-disk form.
func (b Blob) Marshal() []byte {
	var buf bytes.Buffer
	buf.WriteString(blobMagic)
	buf.WriteByte(blobVersion)
	putField(&buf, b.Public)
	putField(&buf, tpm2.Marshal(b.PCRs))
	putField(&buf, tpm2.Marshal(b.SealedPub))
	putField(&buf, tpm2.Marshal(b.SealedPriv))
	return buf.Bytes()
}

// UnmarshalBlob parses the on-disk form.
func UnmarshalBlob(raw []byte) (Blob, error) {
	r := bytes.NewReader(raw)
	magic := make([]byte, len(blobMagic))
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != blobMagic {
		return Blob{}, fmt.Errorf("tpm: not a sealed-key blob")
	}
	v, err := r.ReadByte()
	if err != nil || v != blobVersion {
		return Blob{}, fmt.Errorf("tpm: sealed-key blob version %d, want %d", v, blobVersion)
	}
	pub, err := getField(r)
	if err != nil {
		return Blob{}, err
	}
	pcrsRaw, err := getField(r)
	if err != nil {
		return Blob{}, err
	}
	pubRaw, err := getField(r)
	if err != nil {
		return Blob{}, err
	}
	privRaw, err := getField(r)
	if err != nil {
		return Blob{}, err
	}
	pcrs, err := tpm2.Unmarshal[tpm2.TPMLPCRSelection](pcrsRaw)
	if err != nil {
		return Blob{}, fmt.Errorf("tpm: PCR selection: %w", err)
	}
	sealedPub, err := tpm2.Unmarshal[tpm2.TPM2BPublic](pubRaw)
	if err != nil {
		return Blob{}, fmt.Errorf("tpm: sealed public: %w", err)
	}
	sealedPriv, err := tpm2.Unmarshal[tpm2.TPM2BPrivate](privRaw)
	if err != nil {
		return Blob{}, fmt.Errorf("tpm: sealed private: %w", err)
	}
	if len(pub) != 32 {
		return Blob{}, fmt.Errorf("tpm: public point is %d bytes, want 32", len(pub))
	}
	return Blob{Public: pub, PCRs: *pcrs, SealedPub: *sealedPub, SealedPriv: *sealedPriv}, nil
}

// WriteFile writes the sealed-key blob to path (owner-only, 0600), creating the
// parent directory if needed. The blob holds only the public point and TPM
// ciphertext — safe at rest — but there is no reason to make it world-readable.
func (b Blob) WriteFile(path string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("tpm: creating %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, b.Marshal(), 0o600); err != nil {
		return fmt.Errorf("tpm: writing the sealed key %s: %w", path, err)
	}
	return nil
}

// ReadBlobFile reads a sealed-key blob written by [Blob.WriteFile].
func ReadBlobFile(path string) (Blob, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Blob{}, fmt.Errorf("tpm: reading the sealed key %s: %w", path, err)
	}
	return UnmarshalBlob(raw)
}

func putField(b *bytes.Buffer, f []byte) {
	var l [4]byte
	// #nosec G115 -- blob fields are small TPM2B structures and a 32-byte point,
	// far below 2^32; getField refuses anything over maxBlobField on the way in.
	binary.BigEndian.PutUint32(l[:], uint32(len(f)))
	b.Write(l[:])
	b.Write(f)
}

func getField(r *bytes.Reader) ([]byte, error) {
	var l [4]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, fmt.Errorf("tpm: truncated blob field length")
	}
	n := binary.BigEndian.Uint32(l[:])
	if n > maxBlobField {
		return nil, fmt.Errorf("tpm: blob field of %d bytes exceeds %d", n, maxBlobField)
	}
	f := make([]byte, n)
	if _, err := io.ReadFull(r, f); err != nil {
		return nil, fmt.Errorf("tpm: truncated blob field")
	}
	return f, nil
}
