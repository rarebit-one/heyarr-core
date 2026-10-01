package cli

// space_rewrap_test.go drives `heyarr space rewrap` (void-which-binds-go
// ADR-0022, C2 steps 4 to 7) from the library's gen1 vector: a gen1 secret, a
// gen1 heyarr-recovery-blob-v1 holding one space, that space's key and a change
// encrypted under it. Stage, prove and upload must carry that exact key to the
// gen2 device and recovery key, and every hard stop must leave nothing behind.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/encryption"
	"github.com/rarebit-one/void-which-binds-go/enrolment"
	"github.com/rarebit-one/void-which-binds-go/recovery"
	"github.com/rarebit-one/void-which-binds-go/recovery/slip39"
	"github.com/spf13/pflag"

	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spacerecover"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// gen1Vector is testdata/gen1/counting-entropy-space.json, copied from
// void-which-binds-go v0.19.0 migrate/gen1/testdata.
type gen1Vector struct {
	Secret           string `json:"secret"`
	Recipient        string `json:"recipient"`
	SpaceKey         string `json:"space_key"`
	ChangePlaintext  string `json:"change_plaintext"`
	ChangeCiphertext string `json:"change_ciphertext"`
	Blob             string `json:"blob"`
}

const (
	gen1VectorSpace = "0199a0b0-0000-7000-8000-000000000001"
	// gen1VectorEntropy is the counting-entropy secret's entropy, for the
	// shares test.
	gen1VectorEntropy = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	// otherGen1Secret is the library's zero-entropy gen1 vector: a valid gen1
	// secret that is not the blob's.
	otherGen1Secret = "heyarr1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqndy7c9"
	otherSpaceID    = "0199a0b0-0000-7000-8000-000000000002"
)

func mustHexDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type rewrapFixture struct {
	v           gen1Vector
	dir         string
	config      string
	gen1Blob    string
	gen1File    string
	gen2        recovery.Secret
	gen2File    string
	identityDir string
	deviceDir   string
	expectDB    string
	stage       string
}

func newRewrapFixture(t *testing.T) rewrapFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/gen1/counting-entropy-space.json")
	if err != nil {
		t.Fatal(err)
	}
	var v gen1Vector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	config, _ := recoverConfig(t)
	f := rewrapFixture{
		v: v, dir: dir, config: config,
		gen1Blob:    filepath.Join(dir, "gen1.blob"),
		gen1File:    filepath.Join(dir, "gen1-secret.txt"),
		gen2File:    filepath.Join(dir, "gen2-secret.txt"),
		identityDir: filepath.Join(dir, "identity"),
		deviceDir:   filepath.Join(dir, "device"),
		stage:       filepath.Join(dir, "stage"),
	}
	writeFile(t, f.gen1Blob, mustHexDecode(t, v.Blob))
	writeFile(t, f.gen1File, []byte(v.Secret+"\n"))
	if f.gen2, err = recovery.GenerateSecret(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.gen2File, []byte(f.gen2.String()+"\n"))
	// The gen2 ceremony's laptop: the identity from the gen2 secret, and a
	// software device (C2 steps 1 and 2).
	if _, _, err := run(t, context.Background(), "identity", "recover", "--identity-dir", f.identityDir,
		"--device-dir", f.deviceDir, "--secret-file", f.gen2File, "--name", "laptop"); err != nil {
		t.Fatalf("identity recover: %v", err)
	}
	f.expectDB = f.expectDBWith(t, "expect.db", gen1VectorSpace)
	return f
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// expectDBWith builds a migrated control database whose encrypted_spaces holds
// exactly ids, standing in for a copy of the C2 step 3 backup.
func (f rewrapFixture) expectDBWith(t *testing.T, name string, ids ...string) string {
	t.Helper()
	path := filepath.Join(f.dir, name)
	testdb.WriteMigrated(t, path)
	db, err := sqlite.Open(context.Background(), sqlite.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := db.Writer().Exec(`INSERT INTO encrypted_spaces (id, kind, created_at) VALUES (?, 'vault', ?)`,
			id, "2026-09-25T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func (f rewrapFixture) stageArgs(extra ...string) []string {
	args := []string{
		"--config", f.config, "space", "rewrap", "--device-dir", f.deviceDir,
		"--identity-dir", f.identityDir, "--from", f.gen1Blob, "--stage", f.stage,
		"--expect-db", f.expectDB, "--gen1-secret-file", f.gen1File, "--gen2-secret-file", f.gen2File,
	}
	return append(args, extra...)
}

func (f rewrapFixture) runStage(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	out, _, err := run(t, context.Background(), f.stageArgs(extra...)...)
	return out, err
}

func (f rewrapFixture) mustStage(t *testing.T, extra ...string) string {
	t.Helper()
	out, err := f.runStage(t, extra...)
	if err != nil {
		t.Fatalf("rewrap --from: %v", err)
	}
	return out
}

func (f rewrapFixture) prove(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	args := append([]string{"--config", f.config, "space", "rewrap", "--device-dir", f.deviceDir, "--prove", f.stage}, extra...)
	out, _, err := run(t, context.Background(), args...)
	return out, err
}

func (f rewrapFixture) manifest(t *testing.T) rewrapManifest {
	t.Helper()
	var m rewrapManifest
	if err := json.Unmarshal(readFile(t, filepath.Join(f.stage, rewrapManifestFile)), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rewriteManifest replaces the manifest and, when resum, SHA256SUMS with it.
func (f rewrapFixture) rewriteManifest(t *testing.T, m rewrapManifest, resum bool) {
	t.Helper()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	writeFile(t, filepath.Join(f.stage, rewrapManifestFile), data)
	if resum {
		blob := readFile(t, filepath.Join(f.stage, rewrapBlobFile))
		writeFile(t, filepath.Join(f.stage, rewrapSumsFile), fmt.Appendf(nil, "%x  %s\n%x  %s\n",
			sha256.Sum256(data), rewrapManifestFile, sha256.Sum256(blob), rewrapBlobFile))
	}
}

func normaliseRewrap(f rewrapFixture, s string) string {
	s = strings.ReplaceAll(s, f.dir, "<dir>")
	s = publicKeyPattern.ReplaceAllString(s, "ed25519:<hex>")
	return encryptionKeyPattern.ReplaceAllString(s, "x25519:<hex>")
}

// assertNoStage checks a refused stage left neither the directory nor a
// partial one beside it.
func (f rewrapFixture) assertNoStage(t *testing.T) {
	t.Helper()
	if _, err := os.Lstat(f.stage); err == nil {
		t.Fatal("the stage directory exists after a refusal")
	}
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".partial-") {
			t.Fatalf("a partial stage was left behind: %s", e.Name())
		}
	}
}

// TestSpaceRewrapEndToEnd: stage from the gen1 vector, prove with and without
// the gen2 secret, and both the gen2 recovery wrap and the device wrap open the
// vector's key, which decrypts the vector's change.
func TestSpaceRewrapEndToEnd(t *testing.T) {
	f := newRewrapFixture(t)
	out := f.mustStage(t)
	if !strings.Contains(out, "Staged 1 space(s)") || !strings.Contains(out, gen1VectorSpace) {
		t.Fatalf("stage output:\n%s", out)
	}

	info, err := os.Stat(f.stage)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("stage mode %v, want 0700", info.Mode().Perm())
	}
	for _, name := range []string{rewrapManifestFile, rewrapBlobFile, rewrapSumsFile} {
		fi, err := os.Stat(filepath.Join(f.stage, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v, want 0600", name, fi.Mode().Perm())
		}
	}

	m := f.manifest(t)
	id, err := deriveRecoveryIdentity(f.gen2)
	if err != nil {
		t.Fatal(err)
	}
	devPriv, err := loadDeviceEncKey(f.deviceDir)
	if err != nil {
		t.Fatal(err)
	}
	device := encryption.FormatPublicKey(devPriv.PublicKey().Bytes())
	if m.Gen1Recovery != f.v.Recipient || m.Gen2Recovery != id.RecoveryKey || m.Gen2User != id.UserID || m.DeviceRecipient != device {
		t.Fatalf("manifest = %+v", m)
	}
	if len(m.Spaces) != 1 || m.Spaces[0].SpaceID != gen1VectorSpace || m.Spaces[0].Kind != "vault" {
		t.Fatalf("spaces = %+v", m.Spaces)
	}

	ct := mustHexDecode(t, f.v.ChangeCiphertext)
	want := mustHexDecode(t, f.v.ChangePlaintext)
	wrap := func(r string) []byte {
		w, err := base64.StdEncoding.DecodeString(m.Spaces[0].Wraps[r])
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	recKeys, err := spacerecover.UnwrapAll(f.gen2, map[string][]byte{gen1VectorSpace: wrap(id.RecoveryKey)})
	if err != nil {
		t.Fatalf("the gen2 secret alone: %v", err)
	}
	devKey, err := encryption.Unwrap(wrap(device), devPriv)
	if err != nil {
		t.Fatalf("the device: %v", err)
	}
	for name, k := range map[string]encryption.SpaceKey{"recovery": recKeys[gen1VectorSpace], "device": devKey} {
		got, err := encryption.DecryptChange(k, ct)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("the %s unwrap does not decrypt the vector's change: %v", name, err)
		}
	}
	// The gen2 recovery blob opens with the gen2 secret alone.
	blob, err := spacerecover.OpenBlob(f.gen2, readFile(t, filepath.Join(f.stage, rewrapBlobFile)))
	if err != nil || blob.UserID != id.UserID || len(blob.Spaces) != 1 {
		t.Fatalf("recovery.blob: %+v, %v", blob, err)
	}

	if out, err := f.prove(t, "--gen2-secret-file", f.gen2File); err != nil || !strings.Contains(out, "(full)") {
		t.Fatalf("prove (full): %v\n%s", err, out)
	}
	if out, err := f.prove(t); err != nil || !strings.Contains(out, "(device-only)") {
		t.Fatalf("prove (device-only): %v\n%s", err, out)
	}
}

// TestSpaceRewrapJSONShapes pins the --json of stage and both proofs.
func TestSpaceRewrapJSONShapes(t *testing.T) {
	f := newRewrapFixture(t)
	testutil.Golden(t, "testdata/space_rewrap_stage.json", []byte(normaliseRewrap(f, f.mustStage(t, "--json"))))
	out, err := f.prove(t, "--gen2-secret-file", f.gen2File, "--json")
	if err != nil {
		t.Fatal(err)
	}
	testutil.Golden(t, "testdata/space_rewrap_prove.json", []byte(normaliseRewrap(f, out)))
	out, err = f.prove(t, "--json")
	if err != nil {
		t.Fatal(err)
	}
	testutil.Golden(t, "testdata/space_rewrap_prove_device_only.json", []byte(normaliseRewrap(f, out)))
}

// TestSpaceRewrapFromSharesAndStdin: the gen1 input may be SLIP-39 shares, and
// either secret (one at a time) may come from standard input; --expect checks
// the export's space list too.
func TestSpaceRewrapFromSharesAndStdin(t *testing.T) {
	f := newRewrapFixture(t)
	groups, err := slip39.Split(mustHexDecode(t, gen1VectorEntropy), 1, []slip39.GroupSpec{{Threshold: 2, Count: 3}}, nil, 0, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.gen1File, []byte(groups[0][0]+"\n"+groups[0][2]+"\n"))
	expect := filepath.Join(f.dir, "export.json")
	writeFile(t, expect, fmt.Appendf(nil, `{"out":"gen1.blob","recipient":%q,"generated_at":"2026-09-25T00:00:00Z","space_ids":[%q]}`,
		f.v.Recipient, gen1VectorSpace))

	args := f.stageArgs("--expect", expect)
	for i, a := range args {
		if a == f.gen2File {
			args[i] = "-"
		}
	}
	out, err := runWithStdin(t, f.gen2.String()+"\n", args...)
	if err != nil {
		t.Fatalf("stage from shares, gen2 on stdin: %v\n%s", err, out)
	}
	if _, err := runWithStdin(t, f.gen2.String(), "--config", f.config, "space", "rewrap", "--device-dir", f.deviceDir,
		"--prove", f.stage, "--gen2-secret-file", "-"); err != nil {
		t.Fatalf("prove with gen2 on stdin: %v", err)
	}
}

func runWithStdin(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	cmd := NewRootCommand(Options{Stdout: &out, Stderr: &errb, ShutdownGrace: 2 * time.Second})
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

// TestSpaceRewrapStageRefusals: every hard stop of --from names its cause and
// leaves no stage, partial or whole.
func TestSpaceRewrapStageRefusals(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, f *rewrapFixture) []string
		want  string
	}{
		{
			name: "wrong gen1 secret",
			setup: func(t *testing.T, f *rewrapFixture) []string {
				writeFile(t, f.gen1File, []byte(otherGen1Secret))
				return nil
			},
			want: "opening the gen1 recovery blob",
		},
		{
			name: "a gen2 secret given as gen1",
			setup: func(t *testing.T, f *rewrapFixture) []string {
				writeFile(t, f.gen1File, []byte(f.gen2.String()))
				return nil
			},
			want: "the gen1 secret was not accepted",
		},
		{
			name: "the gen2 secret is not the local identity's",
			setup: func(t *testing.T, f *rewrapFixture) []string {
				other, err := recovery.GenerateSecret()
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, f.gen2File, []byte(other.String()))
				return nil
			},
			want: "is not this machine's gen2 identity's",
		},
		{
			name: "expect-db has an extra space",
			setup: func(t *testing.T, f *rewrapFixture) []string {
				f.expectDB = f.expectDBWith(t, "extra.db", gen1VectorSpace, otherSpaceID)
				return nil
			},
			want: "in --expect-db's encrypted_spaces but not staged: " + otherSpaceID,
		},
		{
			name: "expect-db is missing the space",
			setup: func(t *testing.T, f *rewrapFixture) []string {
				f.expectDB = f.expectDBWith(t, "missing.db", otherSpaceID)
				return nil
			},
			want: "staged but not in --expect-db's encrypted_spaces: " + gen1VectorSpace,
		},
		{
			name: "expect lists another space",
			setup: func(t *testing.T, f *rewrapFixture) []string {
				p := filepath.Join(f.dir, "export.json")
				writeFile(t, p, fmt.Appendf(nil, `{"space_ids":[%q,%q]}`, gen1VectorSpace, otherSpaceID))
				return []string{"--expect", p}
			},
			want: "in --expect's space_ids but not staged: " + otherSpaceID,
		},
		{
			name: "both secrets on stdin",
			setup: func(t *testing.T, f *rewrapFixture) []string {
				f.gen1File, f.gen2File = "-", "-"
				return nil
			},
			want: "only one of --gen1-secret-file and --gen2-secret-file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRewrapFixture(t)
			extra := tt.setup(t, &f)
			_, err := f.runStage(t, extra...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
			f.assertNoStage(t)
		})
	}
}

// TestSpaceRewrapRefusesAnExistingStage: a stage is written once.
func TestSpaceRewrapRefusesAnExistingStage(t *testing.T) {
	f := newRewrapFixture(t)
	if err := os.Mkdir(f.stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runStage(t); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(f.stage); len(entries) != 0 {
		t.Fatalf("the existing directory was written to: %v", entries)
	}
}

// TestSpaceRewrapTakesNoSecretOnArgv: the only secret flags are files.
func TestSpaceRewrapTakesNoSecretOnArgv(t *testing.T) {
	cmd := newSpaceRewrapCommand(Options{}, new(string), new(string))
	cmd.Flags().VisitAll(func(fl *pflag.Flag) {
		if strings.Contains(fl.Name, "secret") && !strings.HasSuffix(fl.Name, "-secret-file") {
			t.Errorf("flag --%s takes a secret on argv", fl.Name)
		}
	})
	f := newRewrapFixture(t)
	for _, flag := range []string{"--secret", "--gen1-secret", "--gen2-secret"} {
		if _, err := f.runStage(t, flag, f.v.Secret); err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("%s: err = %v, want unknown flag", flag, err)
		}
	}
	f.assertNoStage(t)
}

// TestSpaceRewrapProveRefusals: a stage that was changed, or a secret that is
// not the stage's, does not prove.
func TestSpaceRewrapProveRefusals(t *testing.T) {
	tests := []struct {
		name       string
		tamper     func(t *testing.T, f rewrapFixture)
		deviceOnly bool
		want       string
	}{
		{
			name: "another gen2 secret",
			tamper: func(t *testing.T, f rewrapFixture) {
				other, err := recovery.GenerateSecret()
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, f.gen2File, []byte(other.String()))
			},
			want: "but the stage is for",
		},
		{
			name: "manifest edited, sums not",
			tamper: func(t *testing.T, f rewrapFixture) {
				m := f.manifest(t)
				m.BlobGeneratedAt = "2026-01-01T00:00:00Z"
				f.rewriteManifest(t, m, false)
			},
			deviceOnly: true,
			want:       "manifest.json does not match SHA256SUMS",
		},
		{
			name: "SHA256SUMS edited",
			tamper: func(t *testing.T, f rewrapFixture) {
				p := filepath.Join(f.stage, rewrapSumsFile)
				sums := readFile(t, p)
				if sums[0] == '0' {
					sums[0] = '1'
				} else {
					sums[0] = '0'
				}
				writeFile(t, p, sums)
			},
			deviceOnly: true,
			want:       "does not match SHA256SUMS",
		},
		{
			name: "a device wrap corrupted and re-summed",
			tamper: func(t *testing.T, f rewrapFixture) {
				m := f.manifest(t)
				w, _ := base64.StdEncoding.DecodeString(m.Spaces[0].Wraps[m.DeviceRecipient])
				w[len(w)-1] ^= 1
				m.Spaces[0].Wraps[m.DeviceRecipient] = base64.StdEncoding.EncodeToString(w)
				f.rewriteManifest(t, m, true)
			},
			deviceOnly: true,
			want:       "this device does not open its staged wrap",
		},
		{
			name: "a device wrap replaced by another key's, re-summed",
			tamper: func(t *testing.T, f rewrapFixture) {
				m := f.manifest(t)
				other, err := encryption.NewSpaceKey()
				if err != nil {
					t.Fatal(err)
				}
				w, err := spacerecover.RewrapForDevice(map[string]encryption.SpaceKey{"x": other}, m.DeviceRecipient)
				if err != nil {
					t.Fatal(err)
				}
				m.Spaces[0].Wraps[m.DeviceRecipient] = base64.StdEncoding.EncodeToString(w["x"])
				f.rewriteManifest(t, m, true)
			},
			want: "do not all hold the same key",
		},
		{
			name: "a space dropped from the manifest, re-summed",
			tamper: func(t *testing.T, f rewrapFixture) {
				m := f.manifest(t)
				m.Spaces = nil
				f.rewriteManifest(t, m, true)
			},
			deviceOnly: true,
			want:       "no spaces",
		},
		{
			name: "an extra file in the stage",
			tamper: func(t *testing.T, f rewrapFixture) {
				writeFile(t, filepath.Join(f.stage, "notes.txt"), []byte("x"))
			},
			deviceOnly: true,
			want:       "want exactly",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRewrapFixture(t)
			f.mustStage(t)
			tt.tamper(t, f)
			if _, err := f.prove(t, "--gen2-secret-file", f.gen2File); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("full prove: err = %v, want %q", err, tt.want)
			}
			if tt.deviceOnly {
				if _, err := f.prove(t); err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("device-only prove: err = %v, want %q", err, tt.want)
				}
			}
		})
	}
}

// rewrapController is the gen1 controller the upload runs against, as C2 step 6
// leaves it: the space with its content, the principal pinned, and the gen2
// laptop device enrolled.
type rewrapController struct {
	h         *apiHarness
	principal string
}

func newRewrapController(t *testing.T, f rewrapFixture, spaceIDs ...string) rewrapController {
	t.Helper()
	return newRewrapControllerWith(t, f, true, spaceIDs...)
}

// newRewrapControllerWith is newRewrapController, optionally with no content in
// any space.
func newRewrapControllerWith(t *testing.T, f rewrapFixture, withContent bool, spaceIDs ...string) rewrapController {
	t.Helper()
	ctx := context.Background()
	h := newAPIHarness(t, withWrapAuthorizer)
	for _, id := range spaceIDs {
		if _, err := h.spaces.PutSpace(ctx, id, spaces.KindPersonal); err != nil {
			t.Fatal(err)
		}
		if !withContent {
			continue
		}
		ct := mustHexDecode(t, f.v.ChangeCiphertext)
		if id != gen1VectorSpace {
			k, _ := encryption.NewSpaceKey()
			ct, _ = encryption.EncryptChange(k, []byte("another space"))
		}
		ch, err := protocol.NewChange(id, nil, ct)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.spaces.PutChange(ctx, ch); err != nil {
			t.Fatal(err)
		}
	}
	// The principal is pinned under a pre-cutover key and recovery key. The
	// test holds that key's private half only to enrol the laptop device; the
	// real cutover enrols it after the rekey.
	old, oldPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	u, err := h.identities.EnrolUser(ctx, old.UserID(), "owner", f.v.Recipient)
	if err != nil {
		t.Fatal(err)
	}
	ds, err := openDeviceStore(f.deviceDir)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := ds.Get("")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := enrolment.SignCert(oldPriv, dev.PublicKey, encryption.FormatPublicKey(dev.EncryptionKey), h.clock.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.identities.EnrolDevice(ctx, cert, "laptop"); err != nil {
		t.Fatal(err)
	}
	return rewrapController{h: h, principal: u.PrincipalID}
}

func (c rewrapController) rekey(t *testing.T, f rewrapFixture) {
	t.Helper()
	id, err := deriveRecoveryIdentity(f.gen2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.h.identities.RekeyUser(context.Background(), c.principal, id.UserID, id.RecoveryKey); err != nil {
		t.Fatal(err)
	}
}

func (c rewrapController) upload(f rewrapFixture, extra ...string) (string, string, error) {
	args := append([]string{"space", "rewrap", "--device-dir", f.deviceDir, "--upload", f.stage}, extra...)
	return c.h.run(args...)
}

// TestSpaceRewrapUpload: refused until the recovery key is rekeyed, then the
// wraps land, read back, and a second upload is a no-op. The gen1 wrap stays.
func TestSpaceRewrapUpload(t *testing.T) {
	f := newRewrapFixture(t)
	f.mustStage(t)
	c := newRewrapController(t, f, gen1VectorSpace)
	gen1Wrap := mustHexDecode(t, "00")
	if _, err := c.h.spaces.PutWrappedKey(context.Background(), gen1VectorSpace, f.v.Recipient, gen1Wrap); err != nil {
		t.Fatal(err)
	}

	_, _, err := c.upload(f)
	if err == nil || !strings.Contains(err.Error(), "not an enrolled device or recovery key") ||
		!strings.Contains(err.Error(), "heyarr admin user rekey") {
		t.Fatalf("upload before the rekey: err = %v", err)
	}

	c.rekey(t, f)
	out, _, err := c.upload(f, "--json")
	if err != nil {
		t.Fatalf("upload after the rekey: %v", err)
	}
	testutil.Golden(t, "testdata/space_rewrap_upload.json", []byte(normaliseRewrap(f, out)))

	m := f.manifest(t)
	keys, err := c.h.client(t).WrappedKeys(context.Background(), gen1VectorSpace)
	if err != nil {
		t.Fatal(err)
	}
	held := map[string]string{}
	for _, k := range keys {
		held[k.Recipient] = base64.StdEncoding.EncodeToString(k.Wrapped)
	}
	if held[m.DeviceRecipient] != m.Spaces[0].Wraps[m.DeviceRecipient] ||
		held[m.Gen2Recovery] != m.Spaces[0].Wraps[m.Gen2Recovery] ||
		held[f.v.Recipient] != base64.StdEncoding.EncodeToString(gen1Wrap) {
		t.Fatalf("held wraps = %v", held)
	}

	out, _, err = c.upload(f)
	if err != nil || !strings.Contains(out, "already-present") {
		t.Fatalf("second upload: %v\n%s", err, out)
	}
}

// TestSpaceRewrapUploadEmptySpace: a space with no content has nothing to
// check the staged key against; it is uploaded on the strength of the offline
// prove and reported as uploaded-empty.
func TestSpaceRewrapUploadEmptySpace(t *testing.T) {
	f := newRewrapFixture(t)
	f.mustStage(t)
	c := newRewrapControllerWith(t, f, false, gen1VectorSpace)
	c.rekey(t, f)
	out, _, err := c.upload(f)
	if err != nil || !strings.Contains(out, "uploaded-empty") {
		t.Fatalf("upload of an empty space: %v\n%s", err, out)
	}
	out, _, err = c.upload(f)
	if err != nil || !strings.Contains(out, "already-present") {
		t.Fatalf("second upload: %v\n%s", err, out)
	}
}

// TestSpaceRewrapUploadRefusals: a controller holding another set of spaces, or
// content the staged key does not open, gets nothing uploaded.
func TestSpaceRewrapUploadRefusals(t *testing.T) {
	t.Run("set mismatch", func(t *testing.T) {
		f := newRewrapFixture(t)
		f.mustStage(t)
		c := newRewrapController(t, f, gen1VectorSpace, otherSpaceID)
		c.rekey(t, f)
		_, _, err := c.upload(f)
		if err == nil || !strings.Contains(err.Error(), "in the controller's spaces but not staged: "+otherSpaceID) {
			t.Fatalf("err = %v", err)
		}
		c.assertNoGen2Wraps(t, f)
	})
	t.Run("stale key", func(t *testing.T) {
		f := newRewrapFixture(t)
		f.mustStage(t)
		c := newRewrapController(t, f, gen1VectorSpace)
		c.rekey(t, f)
		k, _ := encryption.NewSpaceKey()
		ct, _ := encryption.EncryptChange(k, []byte("after a rotation"))
		snap, err := protocol.NewSnapshot(gen1VectorSpace, nil, ct)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.h.spaces.PutSnapshot(context.Background(), snap); err != nil {
			t.Fatal(err)
		}
		_, _, err = c.upload(f)
		if err == nil || !strings.Contains(err.Error(), "does not open the space's newest content") {
			t.Fatalf("err = %v", err)
		}
		c.assertNoGen2Wraps(t, f)
	})
}

func (c rewrapController) assertNoGen2Wraps(t *testing.T, f rewrapFixture) {
	t.Helper()
	m := f.manifest(t)
	keys, err := c.h.client(t).WrappedKeys(context.Background(), gen1VectorSpace)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.Recipient == m.DeviceRecipient || k.Recipient == m.Gen2Recovery {
			t.Fatalf("a gen2 wrap was uploaded despite the refusal: %s", k.Recipient)
		}
	}
}
