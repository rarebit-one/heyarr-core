package recover_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/backupsync"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	recover "github.com/rarebit-one/heyarr-core/internal/peer/recover"
	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// signedBackup builds a signed backup of a control plane on disk, attributed to
// sourceID, and returns its directory, the manifest, and the seed that signed it.
func signedBackup(t *testing.T, sourceID string) (string, backup.Manifest, []byte) {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Emit(t.Context(), "test.seed", "s", "before-loss", nil); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = pub
	art, err := backup.Take(t.Context(), backup.TakeOptions{
		DB: db, Events: log, SourcePeerID: sourceID, Signer: priv,
		Dir: filepath.Join(dir, "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return art.Dir, art.Manifest, priv.Seed()
}

// fetcher copies a prepared bundle into the work directory, standing in for the
// network fetch so Fetch/Apply are tested without a peer.
type fetcher struct {
	bundleDir string
	manifest  backup.Manifest
	err       error
}

func (f fetcher) FetchBundle(_ context.Context, _ backupsync.Target, _ int64, destDir string) (backup.Manifest, error) {
	if f.err != nil {
		return backup.Manifest{}, f.err
	}
	for _, name := range []string{backup.ManifestFile, backup.SnapshotFile} {
		b, err := os.ReadFile(filepath.Join(f.bundleDir, name)) //nolint:gosec // test path
		if err != nil {
			return backup.Manifest{}, err
		}
		if err := os.WriteFile(filepath.Join(destDir, name), b, 0o600); err != nil {
			return backup.Manifest{}, err
		}
	}
	return f.manifest, nil
}

func TestFetchVerifiesAndPlans(t *testing.T) {
	t.Parallel()
	dir, manifest, seed := signedBackup(t, "peer-a")

	plan, err := recover.Fetch(t.Context(), recover.FetchOptions{
		Fetcher:      fetcher{bundleDir: dir, manifest: manifest},
		IdentitySeed: seed,
		WorkDir:      t.TempDir(),
		Now:          func() time.Time { return manifest.TakenAt.Add(90 * time.Second) },
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if plan.SourcePeerID != "peer-a" || plan.Generation != manifest.Generation {
		t.Errorf("plan = %s gen %d, want peer-a gen %d", plan.SourcePeerID, plan.Generation, manifest.Generation)
	}
	if plan.Age != "1m30s" {
		t.Errorf("age = %q, want 1m30s", plan.Age)
	}
	// The five §51 inputs are reported: two restored here, three refetched.
	restored, refetched := 0, 0
	for _, in := range plan.Inputs {
		switch in.Status {
		case recover.InputRestored:
			restored++
		case recover.InputRefetched:
			refetched++
		}
	}
	if restored < 2 || refetched < 3 {
		t.Errorf("inputs: %d restored, %d refetched; want >=2 and >=3", restored, refetched)
	}
}

func TestFetchRefusesAWrongIdentity(t *testing.T) {
	t.Parallel()
	dir, manifest, _ := signedBackup(t, "peer-a")
	// A different seed than the one that signed the backup: the signature will
	// not verify, so the backup cannot be tied to this node's identity.
	_, otherPriv, _ := ed25519.GenerateKey(nil)

	_, err := recover.Fetch(t.Context(), recover.FetchOptions{
		Fetcher:      fetcher{bundleDir: dir, manifest: manifest},
		IdentitySeed: otherPriv.Seed(),
		WorkDir:      t.TempDir(),
	})
	if err == nil {
		t.Fatal("a backup that does not verify against this node's identity was accepted")
	}
}

func TestApplyRestoresControlPlaneAndIdentity(t *testing.T) {
	t.Parallel()
	dir, manifest, seed := signedBackup(t, "peer-a")
	plan, err := recover.Fetch(t.Context(), recover.FetchOptions{
		Fetcher: fetcher{bundleDir: dir, manifest: manifest}, IdentitySeed: seed, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	dataDir := t.TempDir()
	store, err := cas.OpenFS(filepath.Join(dataDir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	if err := recover.Apply(t.Context(), plan, seed, recover.ApplyOptions{DataDir: dataDir, Store: store}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// The identity key is installed at 0600.
	info, err := os.Stat(identity.KeyPath(dataDir))
	if err != nil {
		t.Fatalf("identity key not installed: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("identity key mode = %#o, want 0600", info.Mode().Perm())
	}
	// The restored control database carries the pre-loss state.
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: sqlite.DataDirFor(dataDir)})
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT count(*) FROM events WHERE subject_id = 'before-loss'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the pre-loss state did not survive the restore (found %d)", n)
	}
	// The CAS marker is bound to the restored identity, so a later identity.Ensure
	// finds all three artefacts agreeing.
	markerID, err := store.MarkerPeerID()
	if err != nil {
		t.Fatal(err)
	}
	if markerID != "peer-a" {
		t.Errorf("CAS marker = %q, want peer-a", markerID)
	}
}

// TestApplyRefusesToOverwriteAnIdentity proves Apply will not clobber an
// existing key — the last guard against two machines claiming one identity.
func TestApplyRefusesToOverwriteAnIdentity(t *testing.T) {
	t.Parallel()
	dir, manifest, seed := signedBackup(t, "peer-a")
	plan, err := recover.Fetch(t.Context(), recover.FetchOptions{
		Fetcher: fetcher{bundleDir: dir, manifest: manifest}, IdentitySeed: seed, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	// An identity already exists here.
	if err := identity.Install(dataDir, seed); err != nil {
		t.Fatal(err)
	}
	store, _ := cas.OpenFS(filepath.Join(dataDir, "cas"))
	err = recover.Apply(t.Context(), plan, seed, recover.ApplyOptions{DataDir: dataDir, Store: store})
	if !errors.Is(err, identity.ErrKeyExists) {
		t.Errorf("apply over an existing identity: got %v, want ErrKeyExists", err)
	}
}

// TestFetchRefusesASchemaTooNew proves a backup at a newer schema than this
// binary knows is refused in the dry run, before anything is installed.
func TestFetchRefusesASchemaTooNew(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	known, err := sqlite.KnownSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	// Plant a goose row a version ahead of this binary — a backup taken by a
	// FUTURE build. The digest covers it, so the backup is genuinely at N+1.
	if _, err := db.Writer().ExecContext(t.Context(),
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, known+1); err != nil {
		t.Fatal(err)
	}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Emit(t.Context(), "test.seed", "s", "x", nil); err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(nil)
	art, err := backup.Take(t.Context(), backup.TakeOptions{
		DB: db, Events: log, SourcePeerID: "peer-a", Signer: priv, Dir: filepath.Join(dir, "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if art.Manifest.SchemaVersion <= known {
		t.Fatalf("backup schema %d is not ahead of known %d", art.Manifest.SchemaVersion, known)
	}
	_, err = recover.Fetch(t.Context(), recover.FetchOptions{
		Fetcher: fetcher{bundleDir: art.Dir, manifest: art.Manifest}, IdentitySeed: priv.Seed(), WorkDir: t.TempDir(),
	})
	if !errors.Is(err, recover.ErrSchemaTooNew) {
		t.Errorf("fetch of a too-new backup: got %v, want ErrSchemaTooNew", err)
	}
}
