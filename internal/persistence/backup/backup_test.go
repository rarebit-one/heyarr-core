package backup_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// fixedClock is a deterministic clock for provenance assertions.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// node is a live control plane for a test: a database, its event log, and the
// data directory they live in.
type node struct {
	db  *sqlite.DB
	log *events.Log
	dir string
}

func newNode(t *testing.T) *node {
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
	return &node{db: db, log: log, dir: dir}
}

// emit records one event, standing in for a control-plane state transition. Its
// subject id is the caller's marker so a restore can be asked whether that exact
// transition survived.
func (n *node) emit(t *testing.T, marker string) {
	t.Helper()
	if _, err := n.log.Emit(t.Context(), "test.marker", "marker", marker, nil); err != nil {
		t.Fatalf("emit %q: %v", marker, err)
	}
}

func (n *node) take(t *testing.T, opts backup.TakeOptions) backup.Artifact {
	t.Helper()
	if opts.DB == nil {
		opts.DB = n.db
	}
	if opts.Events == nil {
		opts.Events = n.log
	}
	if opts.SourcePeerID == "" {
		opts.SourcePeerID = "peer-a"
	}
	if opts.Dir == "" {
		opts.Dir = filepath.Join(n.dir, "backups")
	}
	if opts.Clock == nil {
		opts.Clock = fixedClock{t: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)}
	}
	art, err := backup.Take(t.Context(), opts)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	return art
}

// markerCount opens a restored database and counts events with a subject id.
func markerCount(t *testing.T, dbPath, marker string) int {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT count(*) FROM events WHERE subject_id = ?`, marker).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", marker, err)
	}
	return n
}

// TestRestoresToTheMomentItWasTaken is the central acceptance: a value written
// before the backup is present in the restore, a value written after it is not.
func TestRestoresToTheMomentItWasTaken(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.emit(t, "before")
	art := n.take(t, backup.TakeOptions{})
	n.emit(t, "after")

	dest := filepath.Join(t.TempDir(), "restored.db")
	if _, err := backup.Restore(t.Context(), art.Dir, dest, backup.RestoreOptions{}); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if got := markerCount(t, dest, "before"); got != 1 {
		t.Errorf("value written BEFORE the backup: present=%d, want 1 — the backup did not capture committed state", got)
	}
	if got := markerCount(t, dest, "after"); got != 0 {
		t.Errorf("value written AFTER the backup: present=%d, want 0 — the backup captured writes that came later", got)
	}
}

// TestConsistentUnderConcurrentWrites takes a backup while writes are in flight
// and proves it opens, its foreign keys check out, and integrity_check passes —
// which Open asserts internally, so a successful Open here IS the assertion.
func TestConsistentUnderConcurrentWrites(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.emit(t, "seed") // generation must be > 0 before a backup is allowed

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			// Writes on the single writer pool serialise with VACUUM INTO; the
			// point is that the snapshot is consistent regardless of where in a
			// write stream it lands.
			_, _ = n.log.Emit(ctx, "test.churn", "churn", "x", nil)
		}
	}()

	art := n.take(t, backup.TakeOptions{})
	cancel()
	wg.Wait()

	opened, err := backup.Open(t.Context(), art.Dir, backup.OpenOptions{})
	if err != nil {
		t.Fatalf("a backup taken under concurrent writes did not open consistently: %v", err)
	}
	_ = opened.Close()
}

// TestRPOMeasured writes transactions, loses more after the backup, restores,
// and states the loss as a number — the RPO ADR-0044 set, measured.
func TestRPOMeasured(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	const kept, lost = 20, 7
	for i := 0; i < kept; i++ {
		n.emit(t, "kept")
	}
	art := n.take(t, backup.TakeOptions{})
	for i := 0; i < lost; i++ {
		n.emit(t, "lost")
	}

	dest := filepath.Join(t.TempDir(), "restored.db")
	m, err := backup.Restore(t.Context(), art.Dir, dest, backup.RestoreOptions{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	keptSurvived := markerCount(t, dest, "kept")
	lostSurvived := markerCount(t, dest, "lost")
	if keptSurvived != kept {
		t.Errorf("committed-before-backup transactions: %d survived, want %d", keptSurvived, kept)
	}
	if lostSurvived != 0 {
		t.Errorf("RPO breach: %d of %d post-backup transactions survived the restore (should be 0)", lostSurvived, lost)
	}
	// The measured RPO: work committed after the backup's generation is lost.
	t.Logf("RPO measured: backup generation %d; %d transactions committed after it were lost on restore",
		m.Generation, lost)
}

// TestArtifactRefusedAsControlPlane proves a held backup cannot be written —
// the write fails at the storage layer (invariant 5), with a read as a positive
// control that the handle is otherwise usable.
func TestArtifactRefusedAsControlPlane(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.emit(t, "seed")
	art := n.take(t, backup.TakeOptions{})

	opened, err := backup.Open(t.Context(), art.Dir, backup.OpenOptions{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = opened.Close() }()

	// Positive control: a read works.
	var count int
	if err := opened.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM events`).Scan(&count); err != nil {
		t.Fatalf("read from a held backup should work: %v", err)
	}

	// The refusal: a write fails at the storage layer.
	_, err = opened.DB().ExecContext(t.Context(), `INSERT INTO events (id, type, created_at) VALUES ('x','y','z')`)
	if err == nil {
		t.Fatal("a write to a held backup succeeded — invariant 5 is not held")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "readonly") {
		t.Errorf("write refused, but not at the storage layer: %v — want a SQLITE_READONLY", err)
	}
}

// TestSchemaMismatchRefused proves a backup at version N is refused by a caller
// expecting a different version, rather than silently opened against the wrong
// schema.
func TestSchemaMismatchRefused(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.emit(t, "seed")
	art := n.take(t, backup.TakeOptions{})
	realVersion := art.Manifest.SchemaVersion
	if realVersion <= 1 {
		t.Fatalf("expected a schema version > 1, got %d", realVersion)
	}

	_, err := backup.Open(t.Context(), art.Dir, backup.OpenOptions{ExpectSchema: realVersion - 1})
	if !errors.Is(err, backup.ErrSchemaMismatch) {
		t.Errorf("Open with wrong expected schema: got %v, want ErrSchemaMismatch", err)
	}
	dest := filepath.Join(t.TempDir(), "restored.db")
	_, err = backup.Restore(t.Context(), art.Dir, dest, backup.RestoreOptions{ExpectSchema: realVersion - 1})
	if !errors.Is(err, backup.ErrSchemaMismatch) {
		t.Errorf("Restore with wrong expected schema: got %v, want ErrSchemaMismatch", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("a refused restore still wrote the destination database")
	}
}

// TestTamperedBackupRefused proves both tamper points fail: the snapshot bytes
// (caught by the digest) and the manifest (caught by the signature).
func TestTamperedBackupRefused(t *testing.T) {
	t.Parallel()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}

	t.Run("snapshot bytes", func(t *testing.T) {
		t.Parallel()
		n := newNode(t)
		n.emit(t, "seed")
		art := n.take(t, backup.TakeOptions{Signer: priv})
		flipByte(t, art.SnapshotPath())
		if _, err := backup.Open(t.Context(), art.Dir, backup.OpenOptions{PublicKey: pub}); err == nil {
			t.Fatal("a backup with tampered snapshot bytes was accepted")
		}
	})

	t.Run("manifest signature", func(t *testing.T) {
		t.Parallel()
		n := newNode(t)
		n.emit(t, "seed")
		art := n.take(t, backup.TakeOptions{Signer: priv})
		tamperManifestGeneration(t, art.Dir)
		_, err := backup.Open(t.Context(), art.Dir, backup.OpenOptions{PublicKey: pub})
		if !errors.Is(err, backup.ErrSignatureInvalid) {
			t.Errorf("a tampered manifest: got %v, want ErrSignatureInvalid", err)
		}
	})
}

// TestGenerationAdvances proves two backups with a state change between them
// report different, advancing generations — a mechanism that ran twice, not one
// that reported the same number twice.
func TestGenerationAdvances(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.emit(t, "one")
	first := n.take(t, backup.TakeOptions{})
	n.emit(t, "two")
	second := n.take(t, backup.TakeOptions{})

	if second.Manifest.Generation <= first.Manifest.Generation {
		t.Errorf("generation did not advance: first=%d second=%d",
			first.Manifest.Generation, second.Manifest.Generation)
	}
}

// TestIdempotentPerGeneration proves that with no state change, a second Take
// yields the same artefact rather than a second copy (invariant 9).
func TestIdempotentPerGeneration(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.emit(t, "one")
	first := n.take(t, backup.TakeOptions{})
	second := n.take(t, backup.TakeOptions{})
	if first.Dir != second.Dir {
		t.Errorf("a re-take with no change made a second artefact: %s vs %s", first.Dir, second.Dir)
	}
	if first.Manifest.Generation != second.Manifest.Generation {
		t.Errorf("generation moved with no state change: %d vs %d",
			first.Manifest.Generation, second.Manifest.Generation)
	}
}

// TestGenerationRegressionRefusedOnRestore proves a restore refuses to install a
// backup older than what it would overwrite (ADR-0044 question 3).
func TestGenerationRegressionRefusedOnRestore(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.emit(t, "one")
	old := n.take(t, backup.TakeOptions{})

	dest := filepath.Join(t.TempDir(), "restored.db")
	_, err := backup.Restore(t.Context(), old.Dir, dest, backup.RestoreOptions{
		AgainstGeneration: old.Manifest.Generation + 5,
	})
	if !errors.Is(err, backup.ErrGenerationRegressed) {
		t.Errorf("restoring a stale backup over a fresher database: got %v, want ErrGenerationRegressed", err)
	}
}

// TestRefusedAtGenerationZero proves a control plane with no events refuses a
// backup and records the refusal (invariant 7).
func TestRefusedAtGenerationZero(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	_, err := backup.Take(t.Context(), backup.TakeOptions{
		DB: n.db, Events: n.log, SourcePeerID: "peer-a",
		Dir: filepath.Join(n.dir, "backups"), Clock: fixedClock{t: time.Now()},
	})
	if err == nil {
		t.Fatal("a backup of a control plane with no events succeeded")
	}
	latest, _ := n.log.Latest(t.Context())
	if latest == 0 {
		t.Error("a refused backup emitted no event — the refusal is a state transition (invariant 7)")
	}
}

// TestBackupTakenEventEmitted proves the taken transition is recorded.
func TestBackupTakenEventEmitted(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.emit(t, "seed")
	before, _ := n.log.Latest(t.Context())
	n.take(t, backup.TakeOptions{})
	evs, err := n.log.Since(t.Context(), before, []string{backup.EventTaken}, 10)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	if len(evs) != 1 {
		t.Errorf("expected one %s event, got %d", backup.EventTaken, len(evs))
	}
}

// TestOpenRefusesAnInconsistentSnapshot proves assertConsistent is not vacuous:
// a snapshot whose digest matches its manifest but which contains a foreign-key
// violation is refused. Without this, the consistency check could be removed and
// every other test would still pass — Open succeeds on a valid snapshot whether
// or not the check runs. This is the negative control for that check.
func TestOpenRefusesAnInconsistentSnapshot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	snapPath := filepath.Join(dir, backup.SnapshotFile)
	schema := buildInconsistentDB(t, snapPath)

	digest, size, err := hashing.HashFile(snapPath)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	m := backup.Manifest{Core: backup.Core{
		SourcePeerID:  "peer-a",
		Generation:    1,
		SchemaVersion: schema,
		TakenAt:       time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
		Digest:        digest.String(),
		SizeBytes:     size,
		Omissions:     []string{backup.OmitProviderCredentials},
	}}
	writeManifestJSON(t, filepath.Join(dir, backup.ManifestFile), m)

	_, err = backup.Open(t.Context(), dir, backup.OpenOptions{})
	if !errors.Is(err, backup.ErrIntegrity) {
		t.Errorf("Open on a foreign-key-violating snapshot: got %v, want ErrIntegrity", err)
	}
}

// TestOpenRefusesAManifestLyingAboutSchema proves assertSchema is not vacuous:
// an unsigned manifest that claims a schema version the snapshot is not actually
// at — with a correct digest — is refused. The snapshot's real schema is what
// Open trusts, never the manifest's word for it, because a restore run against
// the wrong schema corrupts rather than fails.
func TestOpenRefusesAManifestLyingAboutSchema(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.emit(t, "seed")
	art := n.take(t, backup.TakeOptions{}) // unsigned, so its manifest is just a file

	m, err := backup.ReadManifest(art.Dir)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	m.SchemaVersion += 100 // a version the snapshot is not at; the digest stays correct
	writeManifestJSON(t, filepath.Join(art.Dir, backup.ManifestFile), m)

	_, err = backup.Open(t.Context(), art.Dir, backup.OpenOptions{})
	if !errors.Is(err, backup.ErrSchemaMismatch) {
		t.Errorf("Open on a manifest lying about schema: got %v, want ErrSchemaMismatch", err)
	}
}

// buildInconsistentDB writes a migrated database that passes integrity_check but
// contains a foreign-key violation, and returns its schema version. The
// violation is planted with foreign_keys OFF so the insert itself succeeds; it
// is foreign_key_check, not the write, that must catch it.
func buildInconsistentDB(t *testing.T, path string) int64 {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: path})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := sqlite.Migrate(t.Context(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if _, err := log.Emit(t.Context(), "test.seed", "seed", "s", nil); err != nil {
		t.Fatalf("emit: %v", err)
	}
	schema, err := sqlite.AppliedSchemaVersion(t.Context(), db)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	for _, stmt := range []string{
		`PRAGMA foreign_keys=OFF`,
		`CREATE TABLE _fk_probe (id INTEGER PRIMARY KEY, parent INTEGER REFERENCES events(seq))`,
		`INSERT INTO _fk_probe (id, parent) VALUES (1, 999999)`,
	} {
		if _, err := db.Writer().ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("planting the violation (%q): %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil { // checkpoints TRUNCATE, leaving a self-contained file
		t.Fatalf("close: %v", err)
	}
	return schema
}

func writeManifestJSON(t *testing.T, path string, m backup.Manifest) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// TestRestoreRecordsItsOwnEvent proves the restored database records its own
// restoration (invariant 7) — the transition is not just performed, it leaves a
// durable trace in the plane it created.
func TestRestoreRecordsItsOwnEvent(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.emit(t, "seed")
	art := n.take(t, backup.TakeOptions{})

	dest := filepath.Join(t.TempDir(), "restored.db")
	if _, err := backup.Restore(t.Context(), art.Dir, dest, backup.RestoreOptions{}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := markerCountByType(t, dest, backup.EventRestored); got != 1 {
		t.Errorf("restored database recorded %d restore events, want 1", got)
	}
}

// markerCountByType counts events of a type in a restored database.
func markerCountByType(t *testing.T, dbPath, eventType string) int {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT count(*) FROM events WHERE type = ?`, eventType).Scan(&n); err != nil {
		t.Fatalf("count type %q: %v", eventType, err)
	}
	return n
}

func flipByte(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(b) == 0 {
		t.Fatalf("%s is empty", path)
	}
	// Flip a byte in the page body, past the SQLite header, so the file is still
	// a database that opens — the digest is what must catch it, not a parse error.
	i := len(b) - 1
	b[i] ^= 0xff
	if err := os.WriteFile(path, b, 0o640); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func tamperManifestGeneration(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, backup.ManifestFile)
	b, err := os.ReadFile(path) //nolint:gosec // test path
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	// Change the recorded generation without re-signing: the signature covers
	// Core, so this must fail verification.
	s := bumpFirstNumberAfter(string(b), `"generation": `)
	if s == string(b) {
		t.Fatal("manifest tamper did not change anything")
	}
	if err := os.WriteFile(path, []byte(s), 0o640); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// bumpFirstNumberAfter increments the first integer following key.
func bumpFirstNumberAfter(s, key string) string {
	idx := strings.Index(s, key)
	if idx < 0 {
		return s
	}
	i := idx + len(key)
	j := i
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j == i {
		return s
	}
	// Append a digit rather than parse: turns 12 into 120, a different value.
	return s[:j] + "0" + s[j:]
}
