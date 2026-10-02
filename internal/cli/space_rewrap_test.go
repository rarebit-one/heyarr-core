package cli

// space_rewrap_test.go drives `heyarr space rewrap` (void-which-binds-go
// ADR-0022, C2 steps 4 to 7) from the library's gen1 vector: a gen1 secret, a
// gen1 heyarr-recovery-blob-v1 holding one space, that space's key and a change
// encrypted under it. Stage, prove and upload must carry that exact key to the
// gen2 device and recovery key, and every hard stop must leave nothing behind.

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/encryption"
	"github.com/rarebit-one/void-which-binds-go/enrolment"
	"github.com/rarebit-one/void-which-binds-go/hashing"
	"github.com/rarebit-one/void-which-binds-go/recovery"
	"github.com/rarebit-one/void-which-binds-go/recovery/slip39"
	"github.com/spf13/pflag"
	"golang.org/x/crypto/chacha20poly1305"

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
	Wrapped          string `json:"wrapped"`
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
	// stageID is the id the last mustStage printed, as the operator records it.
	stageID string
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
		if id != gen1VectorSpace {
			continue
		}
		// The frozen database's own gen1 recovery wrap, which staging checks
		// every blob key against.
		if _, err := db.Writer().Exec(`INSERT INTO wrapped_keys (id, space_id, recipient, wrapped, created_at) VALUES (?, ?, ?, ?, ?)`,
			"0199a0b0-0000-7000-8000-0000000000aa", id, f.v.Recipient, mustHexDecode(t, f.v.Wrapped), "2026-09-25T00:00:00Z"); err != nil {
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

// mustStage stages and records the stage id from the command's own output,
// as the operator does.
func (f *rewrapFixture) mustStage(t *testing.T, extra ...string) string {
	t.Helper()
	out, err := f.runStage(t, extra...)
	if err != nil {
		t.Fatalf("rewrap --from: %v", err)
	}
	m := stageIDPattern.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the stage output names no stage id:\n%s", out)
	}
	f.stageID = m[1]
	return out
}

// stageIDPattern finds the stage id in stage output, human or --json.
var stageIDPattern = regexp.MustCompile(`stage(?: id |_id": ")([0-9a-f]{32})`)

// prove runs --prove with the fixture's gen2 secret file.
func (f rewrapFixture) prove(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	args := append([]string{
		"--config", f.config, "space", "rewrap", "--device-dir", f.deviceDir, "--prove", f.stage,
		"--gen2-secret-file", f.gen2File, "--stage-id", f.stageID,
	}, extra...)
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

// How far a tampering test re-seals what it changed.
type reseal int

const (
	// resealNone leaves SHA256SUMS and STAGE-MAC as they were.
	resealNone reseal = iota
	// resealSums recomputes SHA256SUMS, as anyone with write access to the
	// stage can (#692); STAGE-MAC is left alone.
	resealSums
	// resealMAC recomputes SHA256SUMS and STAGE-MAC with the real gen2
	// secret, which no attacker holds. It reaches the checks behind the MAC.
	resealMAC
)

// rewriteManifest replaces the manifest, re-sealing it as r says.
func (f rewrapFixture) rewriteManifest(t *testing.T, m rewrapManifest, r reseal) {
	t.Helper()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	writeFile(t, filepath.Join(f.stage, rewrapManifestFile), data)
	f.reseal(t, r)
}

// reseal recomputes SHA256SUMS and, for resealMAC, STAGE-MAC over the stage
// as it now is.
func (f rewrapFixture) reseal(t *testing.T, r reseal) {
	t.Helper()
	if r == resealNone {
		return
	}
	manifest := readFile(t, filepath.Join(f.stage, rewrapManifestFile))
	blob := readFile(t, filepath.Join(f.stage, rewrapBlobFile))
	sums := fmt.Appendf(nil, "%x  %s\n%x  %s\n",
		sha256.Sum256(manifest), rewrapManifestFile, sha256.Sum256(blob), rewrapBlobFile)
	writeFile(t, filepath.Join(f.stage, rewrapSumsFile), sums)
	if r != resealMAC {
		return
	}
	tag, err := rewrapStageMAC(f.gen2, map[string][]byte{
		rewrapManifestFile: manifest, rewrapBlobFile: blob, rewrapSumsFile: sums,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.stage, rewrapMACFile), formatStageMAC(tag))
}

func normaliseRewrap(f rewrapFixture, s string) string {
	s = strings.ReplaceAll(s, f.dir, "<dir>")
	if f.stageID != "" {
		s = strings.ReplaceAll(s, f.stageID, "<stage-id>")
	}
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
	for _, name := range []string{rewrapManifestFile, rewrapBlobFile, rewrapSumsFile, rewrapMACFile} {
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
	// The stage id the operator records is the manifest's, and the gen1 blob's
	// sha256 is printed for checking against the export.
	gen1Sum, _, err := hashing.HashFile(f.gen1Blob)
	if err != nil {
		t.Fatal(err)
	}
	if m.StageID != f.stageID || m.Gen1BlobBLAKE3 != gen1Sum.Hex() ||
		!strings.Contains(out, "gen1 blob blake3 "+m.Gen1BlobBLAKE3) {
		t.Fatalf("stage id %q (printed %q), gen1 blob blake3 %q; output:\n%s", m.StageID, f.stageID, m.Gen1BlobBLAKE3, out)
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

	if out, err := f.prove(t); err != nil || !strings.Contains(out, "(full)") {
		t.Fatalf("prove: %v\n%s", err, out)
	}
}

// TestSpaceRewrapJSONShapes pins the --json of stage and prove.
func TestSpaceRewrapJSONShapes(t *testing.T) {
	f := newRewrapFixture(t)
	testutil.Golden(t, "testdata/space_rewrap_stage.json", []byte(normaliseRewrap(f, f.mustStage(t, "--json"))))
	out, err := f.prove(t, "--json")
	if err != nil {
		t.Fatal(err)
	}
	testutil.Golden(t, "testdata/space_rewrap_prove.json", []byte(normaliseRewrap(f, out)))
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
		"--prove", f.stage, "--gen2-secret-file", "-", "--stage-id", f.manifest(t).StageID); err != nil {
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
// not the stage's, does not prove. Anything changed without the gen2 secret is
// caught by STAGE-MAC before the stage is parsed, SHA256SUMS recomputed or not;
// the checks behind the MAC are reached by re-MACing with the real secret.
func TestSpaceRewrapProveRefusals(t *testing.T) {
	otherKeyWrap := func(t *testing.T, recipient string) string {
		t.Helper()
		other, err := encryption.NewSpaceKey()
		if err != nil {
			t.Fatal(err)
		}
		w, err := spacerecover.RewrapForDevice(map[string]encryption.SpaceKey{"x": other}, recipient)
		if err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(w["x"])
	}
	tests := []struct {
		name   string
		tamper func(t *testing.T, f *rewrapFixture)
		want   string
	}{
		{
			name: "another gen2 secret",
			tamper: func(t *testing.T, f *rewrapFixture) {
				other, err := recovery.GenerateSecret()
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, f.gen2File, []byte(other.String()))
			},
			want: "does not verify under this gen2 secret",
		},
		{
			name: "manifest edited, nothing re-sealed",
			tamper: func(t *testing.T, f *rewrapFixture) {
				m := f.manifest(t)
				m.BlobGeneratedAt = "2026-01-01T00:00:00Z"
				f.rewriteManifest(t, m, resealNone)
			},
			want: "does not verify under this gen2 secret",
		},
		{
			name: "manifest edited, SHA256SUMS recomputed",
			tamper: func(t *testing.T, f *rewrapFixture) {
				m := f.manifest(t)
				m.BlobGeneratedAt = "2026-01-01T00:00:00Z"
				f.rewriteManifest(t, m, resealSums)
			},
			want: "does not verify under this gen2 secret",
		},
		{
			name: "recovery.blob edited, SHA256SUMS recomputed",
			tamper: func(t *testing.T, f *rewrapFixture) {
				p := filepath.Join(f.stage, rewrapBlobFile)
				b := readFile(t, p)
				b[len(b)-1] ^= 1
				writeFile(t, p, b)
				f.reseal(t, resealSums)
			},
			want: "does not verify under this gen2 secret",
		},
		{
			name: "SHA256SUMS edited",
			tamper: func(t *testing.T, f *rewrapFixture) {
				p := filepath.Join(f.stage, rewrapSumsFile)
				sums := readFile(t, p)
				if sums[0] == '0' {
					sums[0] = '1'
				} else {
					sums[0] = '0'
				}
				writeFile(t, p, sums)
			},
			want: "does not verify under this gen2 secret",
		},
		{
			name: "STAGE-MAC edited",
			tamper: func(t *testing.T, f *rewrapFixture) {
				p := filepath.Join(f.stage, rewrapMACFile)
				tag := readFile(t, p)
				if tag[0] == '0' {
					tag[0] = '1'
				} else {
					tag[0] = '0'
				}
				writeFile(t, p, tag)
			},
			want: "does not verify under this gen2 secret",
		},
		{
			name: "STAGE-MAC malformed",
			tamper: func(t *testing.T, f *rewrapFixture) {
				writeFile(t, filepath.Join(f.stage, rewrapMACFile), []byte("not a tag\n"))
			},
			want: "STAGE-MAC: malformed",
		},
		{
			name: "STAGE-MAC missing",
			tamper: func(t *testing.T, f *rewrapFixture) {
				if err := os.Remove(filepath.Join(f.stage, rewrapMACFile)); err != nil {
					t.Fatal(err)
				}
			},
			want: "want exactly",
		},
		{
			name: "manifest edited and re-MACed, SHA256SUMS not",
			tamper: func(t *testing.T, f *rewrapFixture) {
				// Behind the MAC, SHA256SUMS still has to match.
				m := f.manifest(t)
				m.BlobGeneratedAt = "2026-01-01T00:00:00Z"
				f.rewriteManifest(t, m, resealMAC)
				sums := filepath.Join(f.stage, rewrapSumsFile)
				orig := readFile(t, sums)
				orig[0] ^= 1
				writeFile(t, sums, orig)
				manifest := readFile(t, filepath.Join(f.stage, rewrapManifestFile))
				blob := readFile(t, filepath.Join(f.stage, rewrapBlobFile))
				tag, err := rewrapStageMAC(f.gen2, map[string][]byte{
					rewrapManifestFile: manifest, rewrapBlobFile: blob, rewrapSumsFile: orig,
				})
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(f.stage, rewrapMACFile), formatStageMAC(tag))
			},
			want: "does not match SHA256SUMS",
		},
		{
			name: "a device wrap corrupted, re-MACed",
			tamper: func(t *testing.T, f *rewrapFixture) {
				m := f.manifest(t)
				w, _ := base64.StdEncoding.DecodeString(m.Spaces[0].Wraps[m.DeviceRecipient])
				w[len(w)-1] ^= 1
				m.Spaces[0].Wraps[m.DeviceRecipient] = base64.StdEncoding.EncodeToString(w)
				f.rewriteManifest(t, m, resealMAC)
			},
			want: "this device does not open its staged wrap",
		},
		{
			name: "a device wrap replaced by another key's, re-MACed",
			tamper: func(t *testing.T, f *rewrapFixture) {
				m := f.manifest(t)
				m.Spaces[0].Wraps[m.DeviceRecipient] = otherKeyWrap(t, m.DeviceRecipient)
				f.rewriteManifest(t, m, resealMAC)
			},
			want: "do not all hold the same key",
		},
		{
			name: "a space dropped from the manifest, re-MACed",
			tamper: func(t *testing.T, f *rewrapFixture) {
				m := f.manifest(t)
				m.Spaces = nil
				f.rewriteManifest(t, m, resealMAC)
			},
			want: "no spaces",
		},
		{
			name: "an extra file in the stage",
			tamper: func(t *testing.T, f *rewrapFixture) {
				writeFile(t, filepath.Join(f.stage, "notes.txt"), []byte("x"))
			},
			want: "want exactly",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRewrapFixture(t)
			f.mustStage(t)
			tt.tamper(t, &f)
			if _, err := f.prove(t); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("prove: err = %v, want %q", err, tt.want)
			}
		})
	}
}

// TestSpaceRewrapStageID: --prove and --upload need the --stage-id this run's
// --stage printed, refuse any other, and --from takes none.
func TestSpaceRewrapStageID(t *testing.T) {
	f := newRewrapFixture(t)
	f.mustStage(t)
	if len(f.stageID) != 32 {
		t.Fatalf("stage id %q", f.stageID)
	}
	c := newRewrapController(t, f, gen1VectorSpace)
	c.rekey(t, f)
	other, err := newRewrapStageID()
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ name, id, want string }{
		{"missing", "", "needs --stage-id"},
		{"another stage's", other, "is not the stage this run wrote"},
		{"malformed", "abc", "a stage id is 32 hex digits"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.prove(t, "--stage-id", tt.id); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("prove: err = %v, want %q", err, tt.want)
			}
			if _, _, err := c.upload(f, "--stage-id", tt.id); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("upload: err = %v, want %q", err, tt.want)
			}
			c.assertNoGen2Wraps(t, f)
		})
	}
	// The operator may type the id in either case.
	if _, err := f.prove(t, "--stage-id", strings.ToUpper(f.stageID)); err != nil {
		t.Fatalf("prove with an uppercase stage id: %v", err)
	}
	f2 := f
	f2.stage = filepath.Join(f.dir, "stage2")
	if _, err := f2.runStage(t, "--stage-id", f.stageID); err == nil || !strings.Contains(err.Error(), "draws a new stage id") {
		t.Fatalf("--from with --stage-id: err = %v", err)
	}
}

// TestSpaceRewrapRefusesAReplayedStage: an older stage, made and MACed with the
// same gen2 secret, put in place of this run's. Every check but the stage id
// passes it (it is genuine, just not this run's), and for an empty space the
// controller has nothing to tell its key from the current one, so only
// --stage-id stops it.
func TestSpaceRewrapRefusesAReplayedStage(t *testing.T) {
	f := newRewrapFixture(t)
	f.mustStage(t) // the older run's stage
	older := f.stage
	f.stage = filepath.Join(f.dir, "stage-now")
	f.mustStage(t) // this run's: f.stageID is now its id
	if f.manifest(t).StageID == stageIDOf(t, older) {
		t.Fatal("two stages drew the same id")
	}
	c := newRewrapControllerWith(t, f, false, gen1VectorSpace)
	c.rekey(t, f)

	replayed := f
	replayed.stage = older
	if _, err := replayed.prove(t); err == nil || !strings.Contains(err.Error(), "is not the stage this run wrote") {
		t.Fatalf("prove of the replayed stage: err = %v", err)
	}
	if _, _, err := c.upload(replayed); err == nil || !strings.Contains(err.Error(), "is not the stage this run wrote") {
		t.Fatalf("upload of the replayed stage: err = %v", err)
	}
	c.assertNoGen2Wraps(t, f)
	if out, _, err := c.upload(f); err != nil || !strings.Contains(out, "uploaded-empty") {
		t.Fatalf("upload of this run's stage: %v\n%s", err, out)
	}
}

func stageIDOf(t *testing.T, dir string) string {
	t.Helper()
	var m rewrapManifest
	if err := json.Unmarshal(readFile(t, filepath.Join(dir, rewrapManifestFile)), &m); err != nil {
		t.Fatal(err)
	}
	return m.StageID
}

// TestSpaceRewrapNeedsTheGen2Secret: no mode runs without --gen2-secret-file,
// because the stage MAC is keyed from it.
func TestSpaceRewrapNeedsTheGen2Secret(t *testing.T) {
	f := newRewrapFixture(t)
	f.mustStage(t)
	for _, mode := range [][]string{{"--prove", f.stage}, {"--upload", f.stage}} {
		args := append([]string{"--config", f.config, "space", "rewrap", "--device-dir", f.deviceDir}, mode...)
		if _, _, err := run(t, context.Background(), args...); err == nil || !strings.Contains(err.Error(), "needs --gen2-secret-file") {
			t.Errorf("%s without the gen2 secret: err = %v", mode[0], err)
		}
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
	args := append([]string{
		"space", "rewrap", "--device-dir", f.deviceDir, "--upload", f.stage,
		"--gen2-secret-file", f.gen2File, "--stage-id", f.stageID,
	}, extra...)
	return c.h.run(args...)
}

// TestSpaceRewrapUpload: stage, prove, then upload: refused until the recovery
// key is rekeyed, then the wraps land, read back, and a second upload is a
// no-op. The gen1 wrap stays.
func TestSpaceRewrapUpload(t *testing.T) {
	f := newRewrapFixture(t)
	f.mustStage(t)
	if _, err := f.prove(t); err != nil {
		t.Fatalf("prove: %v", err)
	}
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
// check the staged key against; it is uploaded on the strength of the stage
// MAC and the full prove, and reported as uploaded-empty.
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

// TestSpaceRewrapUploadRefusesASwappedEmptySpace is #692's attack. Someone who
// can write to the stage between --stage and --upload, but does not hold the
// gen2 secret, replaces an empty space's device and recovery wraps, and
// recovery.blob, with wraps of a key they know (both recipients are public
// keys), and recomputes SHA256SUMS. Every proof but the MAC passes, and the
// empty space has no content to catch the key, so only STAGE-MAC stops it.
func TestSpaceRewrapUploadRefusesASwappedEmptySpace(t *testing.T) {
	f := newRewrapFixture(t)
	f.mustStage(t)
	c := newRewrapControllerWith(t, f, false, gen1VectorSpace)
	c.rekey(t, f)

	forged, err := encryption.NewSpaceKey()
	if err != nil {
		t.Fatal(err)
	}
	m := f.manifest(t)
	keys := map[string]encryption.SpaceKey{gen1VectorSpace: forged}
	dev, err := spacerecover.RewrapForDevice(keys, m.DeviceRecipient)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := spacerecover.RewrapForDevice(keys, m.Gen2Recovery)
	if err != nil {
		t.Fatal(err)
	}
	m.Spaces[0].Wraps[m.DeviceRecipient] = base64.StdEncoding.EncodeToString(dev[gen1VectorSpace])
	m.Spaces[0].Wraps[m.Gen2Recovery] = base64.StdEncoding.EncodeToString(rec[gen1VectorSpace])
	blob, err := spacerecover.SealBlob(spacerecover.Blob{
		UserID: m.Gen2User, RecoveryRecipient: m.Gen2Recovery, GeneratedAt: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC),
		Spaces: []spacerecover.BlobSpace{{SpaceID: gen1VectorSpace, Kind: m.Spaces[0].Kind, Wrapped: rec[gen1VectorSpace]}},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.stage, rewrapBlobFile), blob)
	f.rewriteManifest(t, m, resealSums)

	_, _, err = c.upload(f)
	if err == nil || !strings.Contains(err.Error(), "does not verify under this gen2 secret") {
		t.Fatalf("upload of the swapped stage: err = %v", err)
	}
	c.assertNoGen2Wraps(t, f)
	if _, err := f.prove(t); err == nil || !strings.Contains(err.Error(), "does not verify under this gen2 secret") {
		t.Fatalf("prove of the swapped stage: err = %v", err)
	}

	// The control: had the forger held the gen2 secret to re-MAC it, every
	// other check passes and the forged key is uploaded. The MAC is the line.
	f.reseal(t, resealMAC)
	if out, _, err := c.upload(f); err != nil || !strings.Contains(out, "uploaded-empty") {
		t.Fatalf("the re-MACed forgery should pass every other check: %v\n%s", err, out)
	}
}

// TestSpaceRewrapUploadRefusesAnUnauthenticatedStage: upload checks the stage
// MAC before it reads anything from the controller, and uploads nothing on a
// tampered stage or under another gen2 secret.
func TestSpaceRewrapUploadRefusesAnUnauthenticatedStage(t *testing.T) {
	tests := []struct {
		name   string
		tamper func(t *testing.T, f *rewrapFixture)
	}{
		{"manifest tampered, re-summed", func(t *testing.T, f *rewrapFixture) {
			m := f.manifest(t)
			m.BlobGeneratedAt = "2026-01-01T00:00:00Z"
			f.rewriteManifest(t, m, resealSums)
		}},
		{"recovery.blob tampered, re-summed", func(t *testing.T, f *rewrapFixture) {
			p := filepath.Join(f.stage, rewrapBlobFile)
			b := readFile(t, p)
			b[len(b)-1] ^= 1
			writeFile(t, p, b)
			f.reseal(t, resealSums)
		}},
		{"another gen2 secret", func(t *testing.T, f *rewrapFixture) {
			other, err := recovery.GenerateSecret()
			if err != nil {
				t.Fatal(err)
			}
			f.gen2File = filepath.Join(f.dir, "other-gen2.txt")
			writeFile(t, f.gen2File, []byte(other.String()))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRewrapFixture(t)
			f.mustStage(t)
			c := newRewrapController(t, f, gen1VectorSpace)
			c.rekey(t, f)
			tt.tamper(t, &f)
			_, _, err := c.upload(f)
			if err == nil || !strings.Contains(err.Error(), "does not verify under this gen2 secret") {
				t.Fatalf("err = %v", err)
			}
			c.assertNoGen2Wraps(t, f)
		})
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
	t.Run("a change newer than the snapshot", func(t *testing.T) {
		// A rotation re-wrapped the keys but its snapshot never landed: the
		// latest snapshot is under the staged key, and a later change (past
		// its frontier) is under the new one. The stage is stale.
		f := newRewrapFixture(t)
		f.mustStage(t)
		c := newRewrapController(t, f, gen1VectorSpace)
		c.rekey(t, f)
		ctx := context.Background()
		cl := c.h.client(t)
		existing, err := cl.Changes(ctx, gen1VectorSpace)
		if err != nil || len(existing) != 1 {
			t.Fatalf("changes = %v, %v", existing, err)
		}
		snap, err := protocol.NewSnapshot(gen1VectorSpace, []string{existing[0].ChangeID}, mustHexDecode(t, f.v.ChangeCiphertext))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.h.spaces.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		k, _ := encryption.NewSpaceKey()
		ct, _ := encryption.EncryptChange(k, []byte("after a rotation"))
		ch, err := protocol.NewChange(gen1VectorSpace, []string{existing[0].ChangeID}, ct)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.h.spaces.PutChange(ctx, ch); err != nil {
			t.Fatal(err)
		}
		_, _, err = c.upload(f)
		if err == nil || !strings.Contains(err.Error(), "does not open the space's newest content") {
			t.Fatalf("err = %v", err)
		}
		c.assertNoGen2Wraps(t, f)
	})
	t.Run("a relabelled snapshot frontier", func(t *testing.T) {
		// An authenticated snapshot under the staged key whose public frontier
		// was relabelled to cover a newer change under another key: the
		// envelope does not match, so nothing is uploaded.
		f := newRewrapFixture(t)
		f.mustStage(t)
		c := newRewrapController(t, f, gen1VectorSpace)
		c.rekey(t, f)
		ctx := context.Background()
		existing, err := c.h.client(t).Changes(ctx, gen1VectorSpace)
		if err != nil || len(existing) != 1 {
			t.Fatalf("changes = %v, %v", existing, err)
		}
		k, _ := encryption.NewSpaceKey()
		ct, _ := encryption.EncryptChange(k, []byte("after a rotation"))
		newer, err := protocol.NewChange(gen1VectorSpace, []string{existing[0].ChangeID}, ct)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.h.spaces.PutChange(ctx, newer); err != nil {
			t.Fatal(err)
		}
		staged, err := encryption.SpaceKeyFromBytes(mustHexDecode(t, f.v.SpaceKey))
		if err != nil {
			t.Fatal(err)
		}
		pt := protocol.SealSnapshotPlaintext(gen1VectorSpace, []string{existing[0].ChangeID}, []byte("{}"))
		sct, err := encryption.EncryptChange(staged, pt)
		if err != nil {
			t.Fatal(err)
		}
		forged, err := protocol.NewSnapshot(gen1VectorSpace, []string{newer.ChangeID}, sct)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.h.spaces.PutSnapshot(ctx, forged); err != nil {
			t.Fatal(err)
		}
		_, _, err = c.upload(f)
		if err == nil || !strings.Contains(err.Error(), "does not match its own envelope") {
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

// TestSpaceRewrapBindsKeysToTheFrozenDB: the gen1 blob is sealed to a public
// key, so a forged blob could carry the right space ids with other keys. Every
// blob key must equal the key in the frozen database's own gen1 recovery wrap.
func TestSpaceRewrapBindsKeysToTheFrozenDB(t *testing.T) {
	setWrap := func(t *testing.T, f rewrapFixture, wrapped []byte) {
		t.Helper()
		db, err := sqlite.Open(context.Background(), sqlite.Options{Path: f.expectDB})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		q, args := `DELETE FROM wrapped_keys WHERE space_id = ?`, []any{gen1VectorSpace}
		if wrapped != nil {
			q, args = `UPDATE wrapped_keys SET wrapped = ? WHERE space_id = ?`, []any{wrapped, gen1VectorSpace}
		}
		if _, err := db.Writer().Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name    string
		wrapped func(t *testing.T, f rewrapFixture) []byte
		want    string
	}{
		{"no gen1 recovery wrap", func(*testing.T, rewrapFixture) []byte { return nil }, "holds no wrap for the gen1 recovery key"},
		{"unopenable wrap", func(*testing.T, rewrapFixture) []byte { return bytes.Repeat([]byte{1}, 104) }, "does not open --expect-db's recovery wrap"},
		{"a different key", func(t *testing.T, f rewrapFixture) []byte {
			return gen1SealForTest(t, bytes.Repeat([]byte{7}, 32), f.v.Recipient)
		}, "is not the key in --expect-db"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRewrapFixture(t)
			setWrap(t, f, c.wrapped(t, f))
			_, err := f.runStage(t)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			f.assertNoStage(t)
		})
	}
}

// gen1SealForTest seals key to an x25519 recipient under the retired gen1 wrap
// label, which no production code can do any more; it stands in for a forger.
func gen1SealForTest(t *testing.T, key []byte, recipient string) []byte {
	t.Helper()
	pubBytes := mustHexDecode(t, strings.TrimPrefix(recipient, "x25519:"))
	pub, err := ecdh.X25519().NewPublicKey(pubBytes)
	if err != nil {
		t.Fatal(err)
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := eph.ECDH(pub)
	if err != nil {
		t.Fatal(err)
	}
	ephPub := eph.PublicKey().Bytes()
	salt := append(append([]byte{}, ephPub...), pubBytes...)
	wk, err := hkdf.Key(sha256.New, shared, salt, "heyarr/space-key-wrap/v1", chacha20poly1305.KeySize)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := chacha20poly1305.NewX(wk)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	out := append(append([]byte{}, ephPub...), nonce...)
	return append(out, aead.Seal(nil, nonce, key, salt)...)
}
