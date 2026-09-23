package custody

import (
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/voidbind-go/device"
	"github.com/rarebit-one/voidbind-go/encryption"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client/cruciform"
)

// The software backend is a client.Custody keyed on the device's own encryption
// key: Select returns one whose RecipientID is exactly the wrap target a space
// would be sealed to for this device.
func TestSelectSoftwareUsesTheDeviceKey(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "device")
	ds, err := device.NewStore(device.StoreOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ds.Generate("test-device", false); err != nil {
		t.Fatal(err)
	}
	priv, err := ds.LoadEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	want := encryption.FormatPublicKey(priv.PublicKey().Bytes())

	// Both the explicit name and the empty default resolve to software.
	for _, backend := range []string{"", Software} {
		cust, err := Select(Options{Backend: backend, DeviceDir: dir})
		if err != nil {
			t.Fatalf("Select(%q): %v", backend, err)
		}
		if got := cust.RecipientID(); got != want {
			t.Errorf("Select(%q).RecipientID() = %s, want %s", backend, got, want)
		}
		if _, ok := cust.(*client.KeyUnwrapper); !ok {
			t.Errorf("Select(%q) is %T, want *client.KeyUnwrapper", backend, cust)
		}
	}
}

func TestSelectSoftwareWithNoKeyErrors(t *testing.T) {
	t.Parallel()
	// A device dir with no generated key: loading it fails, surfaced by Select.
	if _, err := Select(Options{Backend: Software, DeviceDir: filepath.Join(t.TempDir(), "empty")}); err == nil {
		t.Fatal("Select on a keyless device dir: want error")
	}
}

func TestSelectYubiKeyNeedsAPIN(t *testing.T) {
	t.Parallel()
	// Without a PIN source the yubikey backend is refused before it touches a card
	// — a deterministic, hardware-free check that the branch validates its input.
	_, err := Select(Options{Backend: YubiKey})
	if err == nil || !strings.Contains(err.Error(), "PIN") {
		t.Fatalf("Select(yubikey) with no PIN: got %v, want a PIN error", err)
	}
}

func TestSelectRejectsUnwiredAndUnknown(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		backend, want string
	}{
		// Cruciform is wired now, but without a pairing config it is refused with a
		// pointer to the ceremony rather than a confusing "unknown backend".
		{Cruciform, "pairing config"},
		{"quantum", "unknown backend"},
	} {
		_, err := Select(Options{Backend: tc.backend})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Select(%q): got %v, want error mentioning %q", tc.backend, err, tc.want)
		}
	}
}

// Select(cruciform) with a pinned pairing config returns a custody keyed on the
// PHONE's encryption key — the wrap target a space is sealed to for offload — and
// a missing config is surfaced with a pointer to the ceremony.
func TestSelectCruciformFromPairing(t *testing.T) {
	t.Parallel()
	_, transportKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	phonePub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	phoneEnc, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &cruciform.PairConfig{
		TransportKey: transportKey,
		PhonePub:     phonePub,
		PhoneEnc:     phoneEnc.PublicKey().Bytes(),
		RelayBase:    "https://relay.example/pair",
	}
	path := filepath.Join(t.TempDir(), cruciform.PairConfigFileName)
	if err := cruciform.SavePairConfig(path, cfg); err != nil {
		t.Fatal(err)
	}

	cust, err := Select(Options{Backend: Cruciform, CruciformPairFile: path})
	if err != nil {
		t.Fatalf("Select(cruciform): %v", err)
	}
	if got, want := cust.RecipientID(), encryption.FormatPublicKey(phoneEnc.PublicKey().Bytes()); got != want {
		t.Errorf("RecipientID = %s, want the phone's key %s", got, want)
	}

	if _, err := Select(Options{Backend: Cruciform, CruciformPairFile: filepath.Join(t.TempDir(), "nope.json")}); err == nil {
		t.Error("Select(cruciform) with a missing pairing config: want an error")
	}
}

// The TPM backend validates its inputs before touching a TPM — deterministic,
// hardware-free. The live seal/unseal is proven against swtpm in the tpm package.
func TestSelectTPMValidatesInputs(t *testing.T) {
	t.Parallel()
	pin := func() (string, error) { return "123456", nil }

	if _, err := Select(Options{Backend: TPM, TPMSealedKeyFile: "/x"}); err == nil || !strings.Contains(err.Error(), "PIN") {
		t.Errorf("tpm with no PIN: got %v, want a PIN error", err)
	}
	if _, err := Select(Options{Backend: TPM, TPMPIN: pin}); err == nil || !strings.Contains(err.Error(), "sealed-key file") {
		t.Errorf("tpm with no sealed-key file: got %v, want a sealed-key error", err)
	}
	missing := filepath.Join(t.TempDir(), "nope.blob")
	if _, err := Select(Options{Backend: TPM, TPMPIN: pin, TPMSealedKeyFile: missing}); err == nil {
		t.Errorf("tpm with a missing sealed-key file: want an error")
	}
}
