package backupsync_test

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/backupsync"
	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// source is a peer producing signed backups of its own control plane.
type source struct {
	db   *sqlite.DB
	log  *events.Log
	dir  string
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	id   string
}

func newSource(t *testing.T, id string) *source {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(t.Context(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return &source{db: db, log: log, dir: dir, priv: priv, pub: pub, id: id}
}

// take advances the control plane by one event and produces a signed backup,
// returning its manifest and the path to its snapshot bytes.
func (s *source) take(t *testing.T) (backup.Manifest, string) {
	t.Helper()
	if _, err := s.log.Emit(t.Context(), "test.marker", "m", "x", nil); err != nil {
		t.Fatalf("emit: %v", err)
	}
	art, err := backup.Take(t.Context(), backup.TakeOptions{
		DB: s.db, Events: s.log, SourcePeerID: s.id, Signer: s.priv,
		Dir: filepath.Join(s.dir, "backups"),
	})
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	return art.Manifest, art.SnapshotPath()
}

func open(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // test path
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestReceiveStoresAndLists(t *testing.T) {
	t.Parallel()
	src := newSource(t, "peer-a")
	m, snap := src.take(t)
	store := backupsync.NewStore(t.TempDir(), 0)

	got, err := store.Receive(t.Context(), src.id, src.pub, m, open(t, snap))
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if got.Generation != m.Generation {
		t.Errorf("stored generation %d, want %d", got.Generation, m.Generation)
	}
	latest, ok, err := store.Latest(src.id)
	if err != nil || !ok {
		t.Fatalf("latest: ok=%v err=%v", ok, err)
	}
	if latest.Generation != m.Generation || latest.Digest != m.Digest {
		t.Errorf("latest = gen %d digest %s, want gen %d digest %s",
			latest.Generation, latest.Digest, m.Generation, m.Digest)
	}
}

// TestReceiveKeysOnTheCertNotTheManifestLabel proves the receiver identifies a
// backup's source by the AUTHENTICATED caller, not by the manifest's own
// SourcePeerID. Two independently enrolled peers assign each other unrelated
// ids, so a backup a peer signed and stamped with its OWN id arrives under the
// id the receiver derived for it from the certificate — and is stored and
// retrievable under THAT. The original code compared the two id spaces and so
// refused every real cross-peer push; the signature against the pinned key
// (TestReceiveRefusesWrongKey) is the identity guard, and keying on the cert is
// what lets recover-from-peer fetch back exactly what was pushed.
func TestReceiveKeysOnTheCertNotTheManifestLabel(t *testing.T) {
	t.Parallel()
	src := newSource(t, "peer-a") // the manifest is stamped "peer-a"
	m, snap := src.take(t)
	store := backupsync.NewStore(t.TempDir(), 0)

	// The receiver's id for this peer — what it derived from the certificate — is
	// not the id the peer stamped into its own manifest. The push must still be
	// accepted, because it is signed by the key the receiver pinned.
	const certID = "a-receivers-own-id-for-peer-a"
	got, err := store.Receive(t.Context(), certID, src.pub, m, open(t, snap))
	if err != nil {
		t.Fatalf("a validly signed cross-peer push was refused: %v", err)
	}
	// Held under the cert-derived id...
	latest, ok, err := store.Latest(certID)
	if err != nil || !ok {
		t.Fatalf("not held under the cert id: ok=%v err=%v", ok, err)
	}
	if latest.Generation != got.Generation {
		t.Errorf("held generation %d, want %d", latest.Generation, got.Generation)
	}
	// ...and nothing under the manifest's own self-label.
	if _, ok, _ := store.Latest("peer-a"); ok {
		t.Error("a backup was held under the manifest's self-label instead of the cert id")
	}
}

func TestReceiveRefusesUnsigned(t *testing.T) {
	t.Parallel()
	src := newSource(t, "peer-a")
	// Take WITHOUT a signer: a pushed backup must be signed.
	if _, err := src.log.Emit(t.Context(), "test.marker", "m", "x", nil); err != nil {
		t.Fatal(err)
	}
	art, err := backup.Take(t.Context(), backup.TakeOptions{
		DB: src.db, Events: src.log, SourcePeerID: src.id,
		Dir: filepath.Join(src.dir, "unsigned"),
	})
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	store := backupsync.NewStore(t.TempDir(), 0)
	_, err = store.Receive(t.Context(), src.id, src.pub, art.Manifest, open(t, art.SnapshotPath()))
	if err == nil {
		t.Fatal("an unsigned pushed backup was stored")
	}
	if !errors.Is(err, backup.ErrUnsigned) {
		t.Errorf("unsigned: got %v, want ErrUnsigned", err)
	}
}

func TestReceiveRefusesWrongKey(t *testing.T) {
	t.Parallel()
	src := newSource(t, "peer-a")
	m, snap := src.take(t)
	otherPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	store := backupsync.NewStore(t.TempDir(), 0)
	_, err = store.Receive(t.Context(), src.id, otherPub, m, open(t, snap))
	if !errors.Is(err, backup.ErrSignatureInvalid) {
		t.Errorf("wrong key: got %v, want ErrSignatureInvalid", err)
	}
}

func TestReceiveRefusesTamperedSnapshot(t *testing.T) {
	t.Parallel()
	src := newSource(t, "peer-a")
	m, snap := src.take(t)
	// Corrupt a copy of the snapshot bytes, keeping the (signed) manifest.
	corrupt := filepath.Join(t.TempDir(), "corrupt.db")
	b, err := os.ReadFile(snap) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 0xff
	if err := os.WriteFile(corrupt, b, 0o600); err != nil {
		t.Fatal(err)
	}
	store := backupsync.NewStore(t.TempDir(), 0)
	if _, err := store.Receive(t.Context(), src.id, src.pub, m, open(t, corrupt)); err == nil {
		t.Fatal("a tampered snapshot was stored")
	}
}

func TestRetentionKeepsNewest(t *testing.T) {
	t.Parallel()
	src := newSource(t, "peer-a")
	store := backupsync.NewStore(t.TempDir(), 2)

	var gens []int64
	for i := 0; i < 4; i++ {
		m, snap := src.take(t)
		if _, err := store.Receive(t.Context(), src.id, src.pub, m, open(t, snap)); err != nil {
			t.Fatalf("receive %d: %v", i, err)
		}
		gens = append(gens, m.Generation)
	}
	held, err := store.Held(src.id)
	if err != nil {
		t.Fatalf("held: %v", err)
	}
	if len(held) != 2 {
		t.Fatalf("retain=2 kept %d generations, want 2", len(held))
	}
	// The two newest survived; the two oldest were pruned.
	if held[0].Generation != gens[3] || held[1].Generation != gens[2] {
		t.Errorf("kept generations %d,%d, want the two newest %d,%d",
			held[0].Generation, held[1].Generation, gens[3], gens[2])
	}
}

// TestHeldBackupIsRefusedAsControlPlane reuses backup.Open on a stored backup:
// it opens read-only, so a write fails at the storage layer (invariant 5).
func TestHeldBackupIsRefusedAsControlPlane(t *testing.T) {
	t.Parallel()
	src := newSource(t, "peer-a")
	m, snap := src.take(t)
	store := backupsync.NewStore(t.TempDir(), 0)
	if _, err := store.Receive(t.Context(), src.id, src.pub, m, open(t, snap)); err != nil {
		t.Fatalf("receive: %v", err)
	}

	opened, err := backup.Open(t.Context(), store.PathFor(src.id, m.Generation), backup.OpenOptions{PublicKey: src.pub})
	if err != nil {
		t.Fatalf("open held backup: %v", err)
	}
	defer func() { _ = opened.Close() }()
	_, err = opened.DB().ExecContext(t.Context(), `INSERT INTO events (id, type, created_at) VALUES ('x','y','z')`)
	if err == nil {
		t.Fatal("a write to a held received backup succeeded — invariant 5 is not held")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "readonly") {
		t.Errorf("write refused, but not at the storage layer: %v", err)
	}
}
