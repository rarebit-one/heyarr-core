package cli

// device_custody_test.go drives a sealed-file custody device (void-which-binds-go
// ADR-0021, "Custody at C2") through the real command tree: C2 step 2's
// `heyarr device generate --custody sealedfile`, then everything the gen2
// laptop device does in the cutover — join by pairing, sign credentials and
// membership ops, take the rewrap's stage and proof, and unwrap a space key —
// with no seed file ever on disk.
//
// The sealed file's Argon2id floor is fixed outside the library and costs
// seconds under -race, so this is one test that derives three times: to seal,
// to unlock (once, for every command after it, through devicekeys' shared
// unlock), and for the wrong passphrase.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/custody"
	"github.com/rarebit-one/void-which-binds-go/custody/sealedfile"
	"github.com/rarebit-one/void-which-binds-go/device"
	"github.com/rarebit-one/void-which-binds-go/encryption"
	"github.com/rarebit-one/void-which-binds-go/enrolment"

	"github.com/rarebit-one/heyarr-core/internal/device/devicekeys"
)

// assertNoSeedFiles fails if dir holds a device seed file, or any file with a
// seed marker in it.
func assertNoSeedFiles(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{device.KeyFileName, device.EncryptionKeyFileName} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s exists in a custody device's directory (%v)", name, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(string(readFile(t, filepath.Join(dir, e.Name()))), "-seed:") {
			t.Fatalf("%s carries a seed marker", e.Name())
		}
	}
}

func TestSealedFileDeviceThroughTheCLI(t *testing.T) {
	f := newRewrapFixture(t) // the gen2 identity, and a software device enrolled under it
	relayAddr := relayServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	custodyDir := filepath.Join(f.dir, "custody")
	pass := filepath.Join(f.dir, "passphrase")
	writeFile(t, pass, []byte("a sealed-file passphrase\n"))
	t.Setenv(devicekeys.PassphraseFileEnvVar, pass)

	// C2 step 2: generate straight into a sealed file.
	out, _, err := run(t, ctx, "device", "generate", "--device-dir", custodyDir, "--name", "gen2-laptop",
		"--custody", "sealedfile", "--passphrase-file", pass, "--json")
	if err != nil {
		t.Fatalf("device generate --custody sealedfile: %v", err)
	}
	var gen device.View
	if err := json.Unmarshal([]byte(out), &gen); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if gen.KeyCustody != device.KeyCustodyExternal || gen.KeyPath != "" {
		t.Fatalf("generated view: key_custody %q, key_path %q", gen.KeyCustody, gen.KeyPath)
	}
	st, err := os.Stat(devicekeys.SealedPath(custodyDir))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("sealed file: %v, %v (want mode 0600)", st, err)
	}
	assertNoSeedFiles(t, custodyDir)
	if _, _, err := run(t, ctx, "device", "generate", "--device-dir", custodyDir, "--custody", "sealedfile",
		"--passphrase-file", pass); !errors.Is(err, device.ErrDeviceExists) {
		t.Fatalf("a second generate without --force: %v, want ErrDeviceExists", err)
	}
	if _, _, err := run(t, ctx, "device", "generate", "--device-dir", filepath.Join(f.dir, "x"),
		"--passphrase-file", pass); err == nil {
		t.Fatal("--passphrase-file with a software device was accepted")
	}
	shortPass := filepath.Join(f.dir, "short-passphrase")
	writeFile(t, shortPass, []byte("too short\n"))
	if _, _, err := run(t, ctx, "device", "generate", "--device-dir", filepath.Join(f.dir, "short"),
		"--custody", "sealedfile", "--passphrase-file", shortPass); !errors.Is(err, devicekeys.ErrPassphraseTooShort) {
		t.Fatalf("a short passphrase: %v, want ErrPassphraseTooShort", err)
	}
	if out, _, err := run(t, ctx, "device", "show", "--device-dir", custodyDir); err != nil ||
		!strings.Contains(out, "held in custody") {
		t.Fatalf("device show: %v\n%s", err, out)
	}

	// Join by pairing: the identity admits the custody device, which opens the
	// sealed admission through its holder.
	joined := runPair(t, ctx,
		[]string{"--identity-dir", f.identityDir, "--device-dir", f.deviceDir, "--relay", relayAddr, "--yes", "--poll", "10ms"},
		[]string{"--device-dir", custodyDir, "--yes", "--poll", "10ms"})
	assertPaired(t, joined)
	laptop := showDeviceJSON(t, ctx, custodyDir)
	idView := showIdentityJSON(t, ctx, f.identityDir)
	if laptop.EnrolmentStatus != device.EnrolmentEnrolled || laptop.EnrolledUser != idView.PublicKey {
		t.Fatalf("custody device is %q under %q, want enrolled under %q", laptop.EnrolmentStatus, laptop.EnrolledUser, idView.PublicKey)
	}

	// Sign a credential through the sealed file.
	out, _, err = run(t, ctx, "identity", "credential", "--device-dir", custodyDir)
	if err != nil {
		t.Fatalf("identity credential: %v", err)
	}
	cert, proof, ok := strings.Cut(strings.TrimSpace(out), enrolment.CredentialSeparator)
	if !ok {
		t.Fatalf("credential: %q", out)
	}
	ds, err := openDeviceStore(custodyDir)
	if err != nil {
		t.Fatal(err)
	}
	laptopDev, err := ds.Get("")
	if err != nil {
		t.Fatal(err)
	}
	if err := enrolment.VerifyPossession(proof, laptopDev.PublicKey, cert, time.Now()); err != nil {
		t.Fatalf("the custody device's possession proof: %v", err)
	}

	// Sign a membership op: the custody device, now a member, admits another.
	third := filepath.Join(f.dir, "third")
	member := runPair(t, ctx,
		[]string{"--identity-dir", filepath.Join(f.dir, "no-identity"), "--device-dir", custodyDir, "--relay", relayAddr, "--yes", "--poll", "10ms"},
		[]string{"--device-dir", third, "--yes", "--poll", "10ms"})
	assertPaired(t, member)
	if !strings.Contains(member.authOut.String(), "authorising as: member device "+laptop.PublicKey) {
		t.Fatalf("the custody device did not authorise as a member:\n%s", member.authOut)
	}
	thirdDev, err := openDeviceStore(third)
	if err != nil {
		t.Fatal(err)
	}
	td, err := thirdDev.Get("")
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := td.EnrolmentCert()
	if op, err := enrolment.VerifyOp(tok); err != nil || op.By != laptop.PublicKey {
		t.Fatalf("the third device's admission: %+v, %v (want by %s)", op, err, laptop.PublicKey)
	}

	// The rewrap's stage and proof (C2 steps 4 and 5), sealed to and opened
	// by the custody device.
	f.deviceDir = custodyDir
	if out := f.mustStage(t); !strings.Contains(out, "Staged 1 space(s)") {
		t.Fatalf("stage:\n%s", out)
	}
	m := f.manifest(t)
	if m.DeviceRecipient != laptop.EncryptionPublicKey {
		t.Fatalf("staged for %s, the custody device is %s", m.DeviceRecipient, laptop.EncryptionPublicKey)
	}
	// --prove is always the full proof (#693): the stage MAC, the gen2 secret
	// alone, and the custody device's holder.
	if out, err := f.prove(t); err != nil {
		t.Fatalf("prove: %v\n%s", err, out)
	}
	// And the space key itself, unwrapped through the selected custody.
	cust, err := selectCustody(&f.config, custodyDir)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := base64.StdEncoding.DecodeString(m.Spaces[0].Wraps[m.DeviceRecipient])
	if err != nil {
		t.Fatal(err)
	}
	sk, err := cust.Unwrap(wrapped)
	if err != nil {
		t.Fatalf("unwrap through the sealed file: %v", err)
	}
	if pt, err := encryption.DecryptChange(sk, mustHexDecode(t, f.v.ChangeCiphertext)); err != nil ||
		string(pt) != string(mustHexDecode(t, f.v.ChangePlaintext)) {
		t.Fatalf("the unwrapped key does not decrypt the vector's change: %v", err)
	}
	assertNoSeedFiles(t, custodyDir)

	// seal-tpm is disabled, for a custody device as for any other.
	if _, _, err := run(t, ctx, "--config", f.config, "device", "seal-tpm", "--device-dir", custodyDir,
		"--out", filepath.Join(f.dir, "tpm.blob")); !errors.Is(err, errSealTPMDisabled) {
		t.Fatalf("seal-tpm on a custody device: %v", err)
	}

	// A wrong passphrase fails cleanly. The copy is a fresh sealed file to this
	// process, so it asks again rather than reusing the unlock above.
	copyDir := filepath.Join(f.dir, "copy")
	if err := os.MkdirAll(copyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(custodyDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		writeFile(t, filepath.Join(copyDir, e.Name()), readFile(t, filepath.Join(custodyDir, e.Name())))
	}
	wrong := filepath.Join(f.dir, "wrong")
	writeFile(t, wrong, []byte("not the passphrase\n"))
	t.Setenv(devicekeys.PassphraseFileEnvVar, wrong)
	if _, _, err := run(t, ctx, "identity", "credential", "--device-dir", copyDir); !errors.Is(err, sealedfile.ErrOpen) {
		t.Fatalf("a wrong passphrase: %v, want sealedfile.ErrOpen", err)
	}

	// Removing the device removes its sealed file.
	if _, _, err := run(t, ctx, "device", "remove", laptop.ID, "--device-dir", copyDir); err != nil {
		t.Fatalf("device remove: %v", err)
	}
	if _, err := os.Stat(devicekeys.SealedPath(copyDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the sealed file survived device remove: %v", err)
	}
}

// keysProvisioner hands out keys it was given, so a custody device RECORD can
// be made with no sealed file and no key derivation.
type keysProvisioner struct{ keys custody.Keys }

func (p keysProvisioner) Provision(custody.Spec) (custody.Keys, error) { return p.keys, nil }

// TestPairAsIdentityWithTheSealedFileLost: the identity signs an admission
// itself and uses the local device only as an optional replica of known ops,
// so a custody device whose sealed file is lost does not stop the identity
// admitting a replacement — and the replica still records the add. Authorising
// AS that device still needs its keys, and says so.
func TestPairAsIdentityWithTheSealedFileLost(t *testing.T) {
	relayAddr := relayServer(t)
	idDir := identityDir(t)
	lostDir := deviceDir(t)
	newDir := deviceDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, _, err := run(t, ctx, "identity", "generate", "--identity-dir", idDir, "--name", "owner"); err != nil {
		t.Fatalf("identity generate: %v", err)
	}
	// A custody device enrolled under the identity, whose device.sealed is gone.
	_, sign, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	lost, err := device.NewStore(device.StoreOptions{Dir: lostDir})
	if err != nil {
		t.Fatal(err)
	}
	dev, err := lost.GenerateInto("lost-laptop", false, keysProvisioner{custody.SoftwareKeys(sign, enc)})
	if err != nil {
		t.Fatal(err)
	}
	idStore, err := openUserIdentityStore(idDir)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := idStore.SignCert(dev.PublicKey, dev.EncryptionKeyString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lost.Enrol(cert); err != nil {
		t.Fatal(err)
	}
	if _, err := openDeviceStore(lostDir); !errors.Is(err, devicekeys.ErrNoSealedFile) {
		t.Fatalf("the lost device opens with its keys: %v, want ErrNoSealedFile", err)
	}

	res := runPair(t, ctx,
		[]string{"--identity-dir", idDir, "--device-dir", lostDir, "--relay", relayAddr, "--yes", "--poll", "10ms"},
		[]string{"--device-dir", newDir, "--yes", "--poll", "10ms"})
	assertPaired(t, res)
	replacement := showDeviceJSON(t, ctx, newDir)
	if replacement.EnrolmentStatus != device.EnrolmentEnrolled {
		t.Fatalf("the replacement is %q", replacement.EnrolmentStatus)
	}
	view, err := lost.Membership(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !view.IsMember(replacement.PublicKey) {
		t.Fatal("the local replica did not record the identity's add")
	}

	if _, _, err := run(t, ctx, "pair", "authorise", "--as", "device", "--identity-dir", idDir,
		"--device-dir", lostDir, "--relay", relayAddr, "--yes"); !errors.Is(err, devicekeys.ErrNoSealedFile) {
		t.Fatalf("authorising as the lost device: %v, want ErrNoSealedFile", err)
	}
}
