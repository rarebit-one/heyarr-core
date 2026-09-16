package tpm

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/go-tpm/tpm2"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
)

// The backend implements the client.Custody seam (ADR-0098).
var _ client.Custody = (*Unwrapper)(nil)

// sampleBlob is a representative sealed-key blob with a real public point and
// well-formed (if inert) TPM2B parts — enough to exercise the codec and
// RecipientID without a TPM.
func sampleBlob(t *testing.T) Blob {
	t.Helper()
	priv, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return Blob{
		Public: priv.PublicKey().Bytes(),
		PCRs:   PCRSelection(7),
		SealedPub: tpm2.New2B(tpm2.TPMTPublic{
			Type:    tpm2.TPMAlgKeyedHash,
			NameAlg: tpm2.TPMAlgSHA256,
		}),
		SealedPriv: tpm2.TPM2BPrivate{Buffer: []byte{0xDE, 0xAD, 0xBE, 0xEF}},
	}
}

func constPIN(s string) PINFunc { return func() (string, error) { return s, nil } }

func TestNewValidates(t *testing.T) {
	t.Parallel()
	blob := sampleBlob(t)
	opener := OpenDevice("") // never called by New

	if _, err := New(blob, nil, opener); err == nil {
		t.Error("nil PINFunc: want error")
	}
	if _, err := New(blob, constPIN("x"), nil); err == nil {
		t.Error("nil opener: want error")
	}
	if _, err := New(Blob{Public: []byte{1, 2, 3}}, constPIN("x"), opener); err == nil {
		t.Error("blob without a 32-byte public point: want error")
	}
	if _, err := New(blob, constPIN("x"), opener); err != nil {
		t.Errorf("valid New: %v", err)
	}
}

func TestRecipientIDIsThePublicPoint(t *testing.T) {
	t.Parallel()
	blob := sampleBlob(t)
	u, err := New(blob, constPIN("x"), OpenDevice(""))
	if err != nil {
		t.Fatal(err)
	}
	// RecipientID must not touch the TPM (it is called before the gate to select
	// the wrapped copy) — here there is no TPM at all, and it still resolves.
	if got, want := u.RecipientID(), encryption.FormatPublicKey(blob.Public); got != want {
		t.Fatalf("RecipientID = %s, want %s", got, want)
	}
}

func TestBlobRoundTrips(t *testing.T) {
	t.Parallel()
	blob := sampleBlob(t)
	got, err := UnmarshalBlob(blob.Marshal())
	if err != nil {
		t.Fatalf("UnmarshalBlob: %v", err)
	}
	if !bytes.Equal(got.Public, blob.Public) {
		t.Error("public point changed across the round-trip")
	}
	if !bytes.Equal(tpm2.Marshal(got.PCRs), tpm2.Marshal(blob.PCRs)) {
		t.Error("PCR selection changed across the round-trip")
	}
	if !bytes.Equal(tpm2.Marshal(got.SealedPub), tpm2.Marshal(blob.SealedPub)) {
		t.Error("sealed public changed across the round-trip")
	}
	if !bytes.Equal(tpm2.Marshal(got.SealedPriv), tpm2.Marshal(blob.SealedPriv)) {
		t.Error("sealed private changed across the round-trip")
	}
}

func TestUnmarshalBlobRejectsGarbage(t *testing.T) {
	t.Parallel()
	good := sampleBlob(t).Marshal()
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"empty", nil},
		{"wrong magic", append([]byte("not-a-tpm-blob-000000000000\x00"), good[len(blobMagic):]...)},
		{"truncated", good[:len(blobMagic)+2]},
		{"trailing/short field", good[:len(good)-3]},
	} {
		if _, err := UnmarshalBlob(tc.raw); err == nil {
			t.Errorf("%s: want a parse error", tc.name)
		}
	}
	// An absurd field length is refused, not allocated.
	oversized := append([]byte(blobMagic), blobVersion, 0xFF, 0xFF, 0xFF, 0xFF)
	if _, err := UnmarshalBlob(oversized); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversized field: got %v, want an 'exceeds' error", err)
	}
}
