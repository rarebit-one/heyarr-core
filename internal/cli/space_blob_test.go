package cli

// space_blob_test.go drives the exported recovery blob end to end (ADR-0022
// addendum): `space export-recovery` on one control database, then
// `space recover --from-blob` against another that holds the spaces' content
// but none of their wrapped keys, the case the blob exists for. It pins the
// re-wrap check: a forged, stale or unverifiable key is refused before anything
// is written.

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/encryption"
	"github.com/rarebit-one/void-which-binds-go/recovery"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spacerecover"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	psstore "github.com/rarebit-one/heyarr-core/internal/personalstate/store"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

const (
	blobSpaceA = "01926a00-0000-7000-8000-00000000000a"
	blobSpaceB = "01926a00-0000-7000-8000-00000000000b"
)

var x25519Pattern = regexp.MustCompile(`x25519:[0-9a-f]{64}`)

// steppingClock advances a second per reading, so rows stored one after another
// have distinct, ordered created_at values.
type steppingClock struct{ t time.Time }

func (c *steppingClock) Now() time.Time { c.t = c.t.Add(time.Second); return c.t }

// blobNode is one controller's database, opened for a test to seed directly.
type blobNode struct {
	config string
	db     *sqlite.DB
	store  *psstore.Store
}

func newBlobNode(t *testing.T) blobNode {
	t.Helper()
	configPath, _ := recoverConfig(t)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// The shared migrated template, not a per-test Migrate: the database has to
	// sit at cfg.Database.Path because the CLI under test opens it there (#610).
	testdb.WriteMigrated(t, cfg.Database.Path)
	db, err := sqlite.Open(ctx, sqlite.Options{Path: cfg.Database.Path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clock := &steppingClock{t: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	st, err := psstore.New(psstore.Options{Writer: db.Writer(), Reader: db.Reader(), Events: log, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	return blobNode{config: configPath, db: db, store: st}
}

// space puts a space and one change encrypted under sk ("canary:<id>").
func (n blobNode) space(t *testing.T, id string, sk encryption.SpaceKey) {
	t.Helper()
	ctx := context.Background()
	if _, err := n.store.PutSpace(ctx, id, spaces.KindPersonal); err != nil {
		t.Fatal(err)
	}
	ct, err := encryption.EncryptChange(sk, []byte("canary:"+id))
	if err != nil {
		t.Fatal(err)
	}
	ch, err := protocol.NewChange(id, nil, ct)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.store.PutChange(ctx, ch); err != nil {
		t.Fatal(err)
	}
}

func (n blobNode) wrapFor(t *testing.T, id string, sk encryption.SpaceKey, recipient string) {
	t.Helper()
	pub, err := encryption.ParsePublicKey(recipient)
	if err != nil {
		t.Fatal(err)
	}
	w, err := encryption.Seal(sk, pub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.store.PutWrappedKey(context.Background(), id, recipient, w, 0); err != nil {
		t.Fatal(err)
	}
}

func (n blobNode) wrappedRows(t *testing.T, recipient string) map[string][]byte {
	t.Helper()
	rows, err := wrappedKeysForRecipient(context.Background(), n.db.Reader(), recipient)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

type blobFixture struct {
	secret     recovery.Secret
	secretFile string
	recipient  string
	keys       map[string]encryption.SpaceKey
	raw        map[string][]byte // each key's bytes, which a rotation seals (ADR-0103)
	source     blobNode          // holds the recovery-wrapped copies
	target     blobNode          // holds the content but no wrapped keys
	deviceDir  string
	dir        string
}

func newBlobFixture(t *testing.T) blobFixture {
	t.Helper()
	secret, err := recovery.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secretFile, []byte(secret.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recipient, err := spacerecover.RecipientID(secret)
	if err != nil {
		t.Fatal(err)
	}
	f := blobFixture{
		secret: secret, secretFile: secretFile, recipient: recipient,
		keys: map[string]encryption.SpaceKey{}, raw: map[string][]byte{},
		source: newBlobNode(t), target: newBlobNode(t),
		deviceDir: filepath.Join(dir, "device"), dir: dir,
	}
	for _, id := range []string{blobSpaceA, blobSpaceB} {
		raw, sk := rawSpaceKey(t)
		f.keys[id], f.raw[id] = sk, raw
		f.source.space(t, id, sk)
		f.source.wrapFor(t, id, sk, recipient)
		f.target.space(t, id, sk) // replicated content, no wrapped keys
	}
	if _, _, err := run(t, context.Background(), "device", "generate", "--device-dir", f.deviceDir, "--name", "recovered"); err != nil {
		t.Fatalf("device generate: %v", err)
	}
	return f
}

// testSealPrev and testUnroller are a TEST-ONLY key-history format: the previous
// key's raw bytes sealed with EncryptChange under the next key. The real
// sealed_prev format belongs to the client keyring change (PR 1.3, #698); the
// recovery path takes its unroller injected, so these tests pin the recovery
// logic without fixing a wire format.
func testSealPrev(t *testing.T, next encryption.SpaceKey, prevRaw []byte) []byte {
	t.Helper()
	sealed, err := encryption.EncryptChange(next, prevRaw)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func testUnroller(current encryption.SpaceKey, epoch int, history []psstore.KeyHistoryEntry) ([]encryption.SpaceKey, error) {
	keys := []encryption.SpaceKey{current}
	cur := current
	for i := len(history) - 1; i >= 0; i-- {
		raw, err := encryption.DecryptChange(cur, history[i].SealedPrev)
		if err != nil {
			if i == len(history)-1 {
				return nil, fmt.Errorf("%w: epoch %d", errKeyNotCurrent, epoch)
			}
			return nil, err
		}
		prev, err := encryption.SpaceKeyFromBytes(raw)
		if err != nil {
			return nil, err
		}
		keys = append(keys, prev)
		cur = prev
	}
	return keys, nil
}

// useTestUnroller swaps the recovery path's unroller for testUnroller for one
// (non-parallel) test.
func useTestUnroller(t *testing.T) {
	t.Helper()
	prev := unrollKeyChain
	unrollKeyChain = testUnroller
	t.Cleanup(func() { unrollKeyChain = prev })
}

// rawSpaceKey mints a space key from bytes the test keeps, because the test
// history format seals the previous key's raw bytes and a SpaceKey exposes none.
func rawSpaceKey(t *testing.T) ([]byte, encryption.SpaceKey) {
	t.Helper()
	raw := make([]byte, encryption.SpaceKeySize)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	sk, err := encryption.SpaceKeyFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw, sk
}

// rotate re-keys space id to a fresh key as a pure re-key (ADR-0103): the source
// stores the history row and the recovery key's new wrap, and the target gets the
// history row by replication. No content is re-encrypted, so the newest content
// on both is still under the old key. Returns the new key.
func (f *blobFixture) rotate(t *testing.T, id string) encryption.SpaceKey {
	t.Helper()
	ctx := context.Background()
	raw, next := rawSpaceKey(t)
	sealed := testSealPrev(t, next, f.raw[id])
	pub, err := encryption.ParsePublicKey(f.recipient)
	if err != nil {
		t.Fatal(err)
	}
	w, err := encryption.Seal(next, pub)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := f.source.store.KeyEpoch(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.source.store.RotateKey(ctx, id, epoch, sealed,
		[]psstore.RecipientWrap{{Recipient: f.recipient, Wrapped: w}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.target.store.PutKeyHistory(ctx, id, epoch+1, sealed); err != nil {
		t.Fatal(err)
	}
	f.keys[id], f.raw[id] = next, raw
	return next
}

func (f blobFixture) export(t *testing.T) string {
	t.Helper()
	out := filepath.Join(f.dir, "recovery.blob")
	if _, _, err := run(t, context.Background(), "--config", f.source.config, "space", "export-recovery",
		"--out", out, "--recipient", f.recipient); err != nil {
		t.Fatalf("export-recovery: %v", err)
	}
	return out
}

func (f blobFixture) recoverRewrap(t *testing.T, blob string) (string, error) {
	t.Helper()
	out, _, err := run(t, context.Background(), "--config", f.target.config, "space", "recover",
		"--device-dir", f.deviceDir, "--secret-file", f.secretFile, "--from-blob", blob, "--rewrap", "--json")
	return out, err
}

// TestRecoveryBlobRoundTrip: export on one node, recover on another that has the
// content but no wrapped keys, and the device then reads the content.
func TestRecoveryBlobRoundTrip(t *testing.T) {
	f := newBlobFixture(t)
	blob := f.export(t)

	info, err := os.Stat(blob)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("blob mode %v, want 0600", info.Mode().Perm())
	}

	if _, err := f.recoverRewrap(t, blob); err != nil {
		t.Fatalf("recover --from-blob --rewrap: %v", err)
	}

	devPriv, err := loadDeviceEncKey(f.deviceDir)
	if err != nil {
		t.Fatal(err)
	}
	rows := f.target.wrappedRows(t, encryption.FormatPublicKey(devPriv.PublicKey().Bytes()))
	if len(rows) != 2 {
		t.Fatalf("re-wrapped %d spaces for the device, want 2", len(rows))
	}
	for id, w := range rows {
		sk, err := encryption.Unwrap(w, devPriv)
		if err != nil {
			t.Fatal(err)
		}
		ct, _, err := newestSpaceCiphertext(context.Background(), f.target.db.Reader(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := encryption.DecryptChange(sk, ct); err != nil || string(got) != "canary:"+id {
			t.Fatalf("%s: the device cannot read the recovered space: %v", id, err)
		}
	}
}

// TestRecoveryBlobJSONShape pins the --json of export and of a blob recovery.
func TestRecoveryBlobJSONShape(t *testing.T) {
	f := newBlobFixture(t)
	blobClock = func() time.Time { return time.Date(2026, 9, 25, 13, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { blobClock = time.Now })

	blob := filepath.Join(f.dir, "recovery.blob")
	out, _, err := run(t, context.Background(), "--config", f.source.config, "space", "export-recovery",
		"--out", blob, "--recipient", f.recipient, "--json")
	if err != nil {
		t.Fatal(err)
	}
	norm := func(s string) string {
		s = strings.ReplaceAll(s, f.dir, "<dir>")
		return x25519Pattern.ReplaceAllString(normalise(s), "x25519:<hex>")
	}
	testutil.Golden(t, "testdata/space_export_recovery.json", []byte(norm(out)))

	out, err = f.recoverRewrap(t, blob)
	if err != nil {
		t.Fatal(err)
	}
	testutil.Golden(t, "testdata/space_recover_from_blob.json", []byte(norm(out)))
}

// TestRecoveryBlobWithoutADatabase: without --rewrap, a blob opens with the
// secret alone. No --config is read, which is the drill the blob allows.
func TestRecoveryBlobWithoutADatabase(t *testing.T) {
	f := newBlobFixture(t)
	blob := f.export(t)
	out, _, err := run(t, context.Background(), "--config", filepath.Join(f.dir, "missing.yaml"),
		"space", "recover", "--secret-file", f.secretFile, "--from-blob", blob)
	if err != nil {
		t.Fatalf("recover --from-blob with no database: %v", err)
	}
	if !strings.Contains(out, "Recovered 2 space key(s)") || !strings.Contains(out, "From a recovery blob exported") {
		t.Fatalf("output: %s", out)
	}
}

// TestRecoveryBlobRefusesTheWrongSecret: another secret opens nothing.
func TestRecoveryBlobRefusesTheWrongSecret(t *testing.T) {
	f := newBlobFixture(t)
	blob := f.export(t)
	other, err := recovery.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = run(t, context.Background(), "space", "recover", "--secret", other.String(), "--from-blob", blob)
	if err == nil || !strings.Contains(err.Error(), "does not open the blob") {
		t.Fatalf("wrong secret: err = %v", err)
	}
}

// sealBlob mints a blob for the fixture's recovery key carrying the given keys.
// Only the PUBLIC key is used: this is what a forger can do.
func (f blobFixture) sealBlob(t *testing.T, keys map[string]encryption.SpaceKey) string {
	t.Helper()
	pub, err := encryption.ParsePublicKey(f.recipient)
	if err != nil {
		t.Fatal(err)
	}
	var sp []spacerecover.BlobSpace
	for id, sk := range keys {
		w, err := encryption.Seal(sk, pub)
		if err != nil {
			t.Fatal(err)
		}
		sp = append(sp, spacerecover.BlobSpace{SpaceID: id, Kind: "personal", Wrapped: w})
	}
	data, err := spacerecover.SealBlob(spacerecover.Blob{RecoveryRecipient: f.recipient, GeneratedAt: time.Now(), Spaces: sp})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.dir, "minted.blob")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (f blobFixture) assertNothingRewrapped(t *testing.T) {
	t.Helper()
	devPriv, err := loadDeviceEncKey(f.deviceDir)
	if err != nil {
		t.Fatal(err)
	}
	if rows := f.target.wrappedRows(t, encryption.FormatPublicKey(devPriv.PublicKey().Bytes())); len(rows) != 0 {
		t.Fatalf("%d keys were re-wrapped for the device despite the refusal", len(rows))
	}
}

// TestRecoveryBlobRefusesAForgedKey: a blob minted with the public key alone,
// carrying an attacker's key for a real space, opens, but re-wrapping it is
// refused because the key does not decrypt the space's content. Nothing is
// written, including for the space whose key was genuine.
func TestRecoveryBlobRefusesAForgedKey(t *testing.T) {
	f := newBlobFixture(t)
	forged, err := encryption.NewSpaceKey()
	if err != nil {
		t.Fatal(err)
	}
	blob := f.sealBlob(t, map[string]encryption.SpaceKey{blobSpaceA: f.keys[blobSpaceA], blobSpaceB: forged})

	_, err = f.recoverRewrap(t, blob)
	if err == nil || !strings.Contains(err.Error(), "does not open this space's newest content") {
		t.Fatalf("forged key: err = %v", err)
	}
	f.assertNothingRewrapped(t)
}

// TestRecoveryBlobRefusesAStaleKey: after a rotation stores a snapshot under a
// new key, a blob holding the old key (genuine, but stale) is refused, even
// though older content still decrypts under it.
func TestRecoveryBlobRefusesAStaleKey(t *testing.T) {
	f := newBlobFixture(t)
	blob := f.export(t)

	rotated, err := encryption.NewSpaceKey()
	if err != nil {
		t.Fatal(err)
	}
	ct, err := encryption.EncryptChange(rotated, []byte("state after rotation"))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := protocol.NewSnapshot(blobSpaceA, nil, ct)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.target.store.PutSnapshot(context.Background(), snap); err != nil {
		t.Fatal(err)
	}

	_, err = f.recoverRewrap(t, blob)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale key: err = %v", err)
	}
	f.assertNothingRewrapped(t)
}

// TestRecoveryBlobRefusesAnUncheckableSpace: a space with no content on this
// node cannot be verified, so its key is not re-wrapped.
func TestRecoveryBlobRefusesAnUncheckableSpace(t *testing.T) {
	f := newBlobFixture(t)
	const empty = "01926a00-0000-7000-8000-00000000000c"
	if _, err := f.target.store.PutSpace(context.Background(), empty, spaces.KindPersonal); err != nil {
		t.Fatal(err)
	}
	sk, err := encryption.NewSpaceKey()
	if err != nil {
		t.Fatal(err)
	}
	blob := f.sealBlob(t, map[string]encryption.SpaceKey{empty: sk})

	_, err = f.recoverRewrap(t, blob)
	if err == nil || !strings.Contains(err.Error(), "holds no content") {
		t.Fatalf("uncheckable space: err = %v", err)
	}
	f.assertNothingRewrapped(t)
}

// TestExportRecoveryRefusesNothingToExport: a node with no recovery-wrapped keys
// writes no blob rather than an empty one that would look like a backup.
func TestExportRecoveryRefusesNothingToExport(t *testing.T) {
	n := newBlobNode(t)
	secret, err := recovery.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := spacerecover.RecipientID(secret)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "empty.blob")
	_, _, err = run(t, context.Background(), "--config", n.config, "space", "export-recovery", "--out", out, "--recipient", rec)
	if err == nil || !strings.Contains(err.Error(), "nothing to export") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("an empty blob was written")
	}
}

// deviceWrap returns this fixture device's wrapped copy of a space on node n and
// the epoch it is stored at.
func (f blobFixture) deviceWrap(t *testing.T, n blobNode, id string) (encryption.SpaceKey, int) {
	t.Helper()
	devPriv, err := loadDeviceEncKey(f.deviceDir)
	if err != nil {
		t.Fatal(err)
	}
	devID := encryption.FormatPublicKey(devPriv.PublicKey().Bytes())
	keys, err := n.store.WrappedKeysFor(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.Recipient == devID {
			sk, err := encryption.Unwrap(k.Wrapped, devPriv)
			if err != nil {
				t.Fatal(err)
			}
			return sk, k.Epoch
		}
	}
	t.Fatalf("space %s holds no copy for the device", id)
	return encryption.SpaceKey{}, 0
}

// TestRecoveryAfterARotation (ADR-0103, #698): a rotation is a pure re-key, so
// right after one the newest content is still under the OLD key. A blob exported
// after the rotation carries the NEW key: it opens the newest history row (it is
// current), unrolls to the old key that opens the content, and is re-wrapped for
// the device at the new epoch. Recovering from the control database itself works
// the same way.
func TestRecoveryAfterARotation(t *testing.T) {
	useTestUnroller(t)
	t.Run("from a blob exported after the rotation", func(t *testing.T) {
		f := newBlobFixture(t)
		next := f.rotate(t, blobSpaceA)
		blob := f.export(t)
		if _, err := f.recoverRewrap(t, blob); err != nil {
			t.Fatalf("recover --from-blob --rewrap after a rotation: %v", err)
		}
		sk, epoch := f.deviceWrap(t, f.target, blobSpaceA)
		if epoch != 1 {
			t.Fatalf("the device copy is at epoch %d, want 1", epoch)
		}
		probe, err := encryption.EncryptChange(next, []byte("probe"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := encryption.DecryptChange(sk, probe); err != nil {
			t.Fatal("the device was wrapped a key other than the current one")
		}
		if _, epochB := f.deviceWrap(t, f.target, blobSpaceB); epochB != 0 {
			t.Fatalf("the unrotated space's copy is at epoch %d, want 0", epochB)
		}
	})

	t.Run("from the control database", func(t *testing.T) {
		f := newBlobFixture(t)
		f.rotate(t, blobSpaceA)
		if _, _, err := run(t, context.Background(), "--config", f.source.config, "space", "recover",
			"--device-dir", f.deviceDir, "--secret-file", f.secretFile, "--rewrap", "--json"); err != nil {
			t.Fatalf("recover --rewrap after a rotation: %v", err)
		}
		if _, epoch := f.deviceWrap(t, f.source, blobSpaceA); epoch != 1 {
			t.Fatalf("the device copy is at epoch %d, want 1", epoch)
		}
	})

	t.Run("a blob exported before the rotation is stale", func(t *testing.T) {
		f := newBlobFixture(t)
		blob := f.export(t)
		f.rotate(t, blobSpaceA)
		// The old key still opens the newest content — only the history row
		// shows it is no longer current.
		_, err := f.recoverRewrap(t, blob)
		if err == nil || !strings.Contains(err.Error(), "stale") || !strings.Contains(err.Error(), "export-recovery") {
			t.Fatalf("stale blob after a rotation: err = %v", err)
		}
		f.assertNothingRewrapped(t)
	})
}

// TestRecoveryOfARotatedSpaceWaitsForTheUnroller: until the client keyring's
// unroller is wired (PR 1.3), a rotated space refuses --rewrap rather than
// guessing a history format — and writes nothing.
func TestRecoveryOfARotatedSpaceWaitsForTheUnroller(t *testing.T) {
	f := newBlobFixture(t)
	f.rotate(t, blobSpaceA)
	_, err := f.recoverRewrap(t, f.export(t))
	if err == nil || !strings.Contains(err.Error(), "cannot open its key history yet") {
		t.Fatalf("recover of a rotated space with the production unroller: err = %v", err)
	}
	f.assertNothingRewrapped(t)
}
