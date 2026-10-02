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
	"github.com/rarebit-one/void-which-binds-go/recovery"

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

	// Recovering the identity over a HEALTHY custody device keeps that device:
	// it is re-enrolled as it is, with no passphrase asked and no replacement.
	if _, _, err := run(t, ctx, "identity", "recover", "--identity-dir", f.identityDir, "--device-dir", custodyDir,
		"--secret-file", f.gen2File, "--force"); err != nil {
		t.Fatalf("identity recover over the custody device: %v", err)
	}
	if again := showDeviceJSON(t, ctx, custodyDir); again.ID != laptop.ID || again.KeyCustody != device.KeyCustodyExternal ||
		again.EnrolmentStatus != device.EnrolmentEnrolled {
		t.Fatalf("recover replaced or un-enrolled a healthy custody device: %+v", again)
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

// TestIdentityRecoverReplacesAnUnusableCustodyDevice: recovery signs with the
// identity alone, so a custody device whose sealed file is missing or
// unreadable does not stop it. That device — and only it — is replaced by a
// NEW sealed-file device (never seed files), enrolled under the recovered
// identity, and an unreadable sealed file is moved aside rather than deleted.
// A passphrase below the minimum is refused before anything is written.
// Each successful case seals once, at the real Argon2id floor.
func TestIdentityRecoverReplacesAnUnusableCustodyDevice(t *testing.T) {
	secret, err := recovery.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	// lostCustodyDevice makes a custody device record in a fresh root, with
	// sealed as its sealed file (nil: none).
	lostCustodyDevice := func(t *testing.T, sealed []byte) (root, idDir, devDir, secretFile string, old device.Device) {
		t.Helper()
		root = t.TempDir()
		secretFile = filepath.Join(root, "secret")
		writeFile(t, secretFile, []byte(secret.String()+"\n"))
		idDir = filepath.Join(root, "identity")
		devDir = filepath.Join(root, "device")
		_, sign, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		enc, err := encryption.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		lost, err := device.NewStore(device.StoreOptions{Dir: devDir})
		if err != nil {
			t.Fatal(err)
		}
		if old, err = lost.GenerateInto("lost-laptop", false, keysProvisioner{custody.SoftwareKeys(sign, enc)}); err != nil {
			t.Fatal(err)
		}
		if sealed != nil {
			writeFile(t, devicekeys.SealedPath(devDir), sealed)
		}
		return root, idDir, devDir, secretFile, old
	}

	for _, tc := range []struct {
		name   string
		sealed []byte
	}{
		{"the sealed file is missing", nil},
		{"the sealed file is unreadable", []byte("not a sealed file")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root, idDir, devDir, secretFile, old := lostCustodyDevice(t, tc.sealed)
			pass := filepath.Join(root, "passphrase")
			writeFile(t, pass, []byte("a new sealed-file passphrase\n"))

			out, _, err := run(t, ctx, "identity", "recover", "--identity-dir", idDir, "--device-dir", devDir,
				"--secret-file", secretFile, "--passphrase-file", pass, "--json")
			if err != nil {
				t.Fatalf("identity recover: %v", err)
			}
			var got struct {
				Identity struct {
					PublicKey string `json:"public_key"`
				} `json:"identity"`
				Device device.View `json:"device"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, out)
			}
			if got.Device.ID == old.ID || got.Device.PublicKey == old.PublicKeyString() {
				t.Fatal("the unusable device was kept")
			}
			if got.Device.KeyCustody != device.KeyCustodyExternal || got.Device.KeyPath != "" {
				t.Fatalf("the replacement is not custody-held: key_custody %q, key_path %q",
					got.Device.KeyCustody, got.Device.KeyPath)
			}
			if got.Device.EnrolmentStatus != device.EnrolmentEnrolled || got.Device.EnrolledUser != got.Identity.PublicKey {
				t.Fatalf("the replacement is %q under %q, want enrolled under %q",
					got.Device.EnrolmentStatus, got.Device.EnrolledUser, got.Identity.PublicKey)
			}
			assertNoSeedFiles(t, devDir)
			st, err := os.Stat(devicekeys.SealedPath(devDir))
			if err != nil || st.Mode().Perm() != 0o600 {
				t.Fatalf("the replacement's sealed file: %v, %v (want mode 0600)", st, err)
			}
			if err := devicekeys.Usable(devDir); err != nil {
				t.Fatalf("the replacement does not open with its sealed file: %v", err)
			}
			moved, _ := filepath.Glob(devicekeys.SealedPath(devDir) + ".unusable-*")
			if (tc.sealed != nil) != (len(moved) == 1) {
				t.Fatalf("moved-aside sealed files: %v", moved)
			}
			if len(moved) == 1 && string(readFile(t, moved[0])) != string(tc.sealed) {
				t.Fatal("the moved-aside sealed file changed")
			}
		})
	}

	t.Run("a short passphrase writes nothing", func(t *testing.T) {
		ctx := context.Background()
		root, idDir, devDir, secretFile, old := lostCustodyDevice(t, []byte("not a sealed file"))
		short := filepath.Join(root, "short")
		writeFile(t, short, []byte("too short\n"))
		if _, _, err := run(t, ctx, "identity", "recover", "--identity-dir", idDir, "--device-dir", devDir,
			"--secret-file", secretFile, "--passphrase-file", short); !errors.Is(err, devicekeys.ErrPassphraseTooShort) {
			t.Fatalf("recover with a short passphrase: %v, want ErrPassphraseTooShort", err)
		}
		if _, _, err := run(t, ctx, "identity", "show", "--identity-dir", idDir); err == nil {
			t.Fatal("a refused passphrase left an identity behind")
		}
		plain, err := device.NewStore(device.StoreOptions{Dir: devDir})
		if err != nil {
			t.Fatal(err)
		}
		if dev, err := plain.Get(""); err != nil || dev.ID != old.ID {
			t.Fatalf("a refused passphrase changed the device: %+v, %v", dev, err)
		}
		if got := string(readFile(t, devicekeys.SealedPath(devDir))); got != "not a sealed file" {
			t.Fatal("a refused passphrase moved the sealed file")
		}
	})
}

// TestIdentityRecoverChecksTheDeviceFirst: a device that is here but cannot be
// read (a software device with a damaged key file) is refused before the
// identity is written, so a failed recover leaves no half-recovered identity.
func TestIdentityRecoverChecksTheDeviceFirst(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	secret, err := recovery.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(root, "secret")
	writeFile(t, secretFile, []byte(secret.String()+"\n"))
	idDir := filepath.Join(root, "identity")
	devDir := filepath.Join(root, "device")
	generateDevice(t, devDir)
	writeFile(t, filepath.Join(devDir, device.KeyFileName), []byte("damaged\n"))

	if _, _, err := run(t, ctx, "identity", "recover", "--identity-dir", idDir, "--device-dir", devDir,
		"--secret-file", secretFile); err == nil {
		t.Fatal("recover over a damaged device succeeded")
	}
	if _, _, err := run(t, ctx, "identity", "show", "--identity-dir", idDir); err == nil {
		t.Fatal("a failed recover left an identity behind")
	}
}
