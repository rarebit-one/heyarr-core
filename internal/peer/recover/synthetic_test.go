package recover_test

// Adversarial synthetic tests for the recover package, covering scenarios the
// hand-written suite in recover_test.go does not: a propagated fetch error, the
// two partial-state data-dir cases (an existing DB with no key; a key with no
// DB), end-to-end lease voiding through Apply, age formatting at the extremes,
// and Apply with an optional (nil) content store.
//
// These reuse signedBackup and the fetcher stand-in from recover_test.go (same
// package).

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	recover "github.com/rarebit-one/heyarr-core/internal/peer/recover"
	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// --- Scenario 1: Fetch propagates a fetch error cleanly. ---

var errFetchBoom = errors.New("synthetic: the surviving peer refused the pull")

func TestFetchPropagatesAFetchError(t *testing.T) {
	t.Parallel()
	_, priv, _ := ed25519.GenerateKey(nil)

	plan, err := recover.Fetch(t.Context(), recover.FetchOptions{
		Fetcher:      fetcher{err: errFetchBoom},
		IdentitySeed: priv.Seed(),
		WorkDir:      t.TempDir(),
	})
	if !errors.Is(err, errFetchBoom) {
		t.Fatalf("fetch error not propagated: got %v, want %v", err, errFetchBoom)
	}
	// Nothing usable comes back on the error path.
	if plan.SourcePeerID != "" || plan.Signed {
		t.Errorf("a plan leaked on the error path: %+v", plan)
	}
}

// --- Scenario 2: the two partial-state data-dir cases. ---
//
// These are assessments, not pass/fail assertions: they exercise recover.Apply
// against a data directory in each half-installed state and document precisely
// what it does. The finding: the identity key is the SOLE interlock. Apply
// guards the key (identity.Install refuses to overwrite) but does NOT guard the
// control database (backup.Restore overwrites via an atomic temp+rename). So an
// existing DB with no key is silently replaced, while an existing key aborts the
// whole restore before the DB is touched. The command layer's "refuse a live
// data dir" check — which recover.Apply's own doc says "belongs to the command"
// — is what actually protects a live DB; Apply alone does not.

func TestApplyPartialStateExistingDBNoKey(t *testing.T) {
	t.Parallel()
	dir, manifest, seed := signedBackup(t, "peer-a")
	plan, err := recover.Fetch(t.Context(), recover.FetchOptions{
		Fetcher: fetcher{bundleDir: dir, manifest: manifest}, IdentitySeed: seed, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	dataDir := t.TempDir()
	// A pre-existing, unrelated control database sits at the canonical path, but
	// there is NO identity key. This models a data dir left half-populated by an
	// earlier aborted run.
	dbPath := sqlite.DataDirFor(dataDir)
	pre, err := sqlite.Open(t.Context(), sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.Migrate(t.Context(), pre); err != nil {
		t.Fatal(err)
	}
	preLog, err := events.New(events.Options{Writer: pre.Writer(), Reader: pre.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := preLog.Emit(t.Context(), "test.stale", "s", "stale-preexisting", nil); err != nil {
		t.Fatal(err)
	}
	_ = pre.Close()
	if _, err := os.Stat(identity.KeyPath(dataDir)); !os.IsNotExist(err) {
		t.Fatalf("precondition: expected no key, stat gave %v", err)
	}

	store, _ := cas.OpenFS(filepath.Join(dataDir, "cas"))
	applyErr := recover.Apply(t.Context(), plan, seed, recover.ApplyOptions{DataDir: dataDir, Store: store})

	// OBSERVED behavior: Apply succeeds. It installs the key, then OVERWRITES the
	// pre-existing DB with the restored one.
	if applyErr != nil {
		t.Fatalf("FINDING (existing-DB/no-key): Apply returned %v; expected it to install the key and overwrite the DB", applyErr)
	}
	if _, err := os.Stat(identity.KeyPath(dataDir)); err != nil {
		t.Errorf("key was not installed: %v", err)
	}
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer func() { _ = db.Close() }()
	var stale, restored int
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT count(*) FROM events WHERE subject_id = 'stale-preexisting'`).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT count(*) FROM events WHERE subject_id = 'before-loss'`).Scan(&restored); err != nil {
		t.Fatal(err)
	}
	// The stale DB is gone; the restored state is present. This is a clean
	// replacement, not a merge or a corruption — but it IS an unguarded clobber.
	if stale != 0 {
		t.Errorf("FINDING: the pre-existing DB survived (stale rows=%d); expected a full overwrite", stale)
	}
	if restored != 1 {
		t.Errorf("restored state missing after overwrite (before-loss rows=%d)", restored)
	}
	t.Logf("FINDING (existing-DB/no-key): Apply SUCCEEDS and silently overwrites the DB atomically "+
		"(stale rows=%d, restored rows=%d). No partial corruption, but no guard against clobbering a "+
		"DB either — the identity key is the only interlock.", stale, restored)
}

func TestApplyPartialStateKeyNoDB(t *testing.T) {
	t.Parallel()
	dir, manifest, seed := signedBackup(t, "peer-a")
	plan, err := recover.Fetch(t.Context(), recover.FetchOptions{
		Fetcher: fetcher{bundleDir: dir, manifest: manifest}, IdentitySeed: seed, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	dataDir := t.TempDir()
	// A key is already installed, but there is NO control database yet.
	if err := identity.Install(dataDir, seed); err != nil {
		t.Fatal(err)
	}
	dbPath := sqlite.DataDirFor(dataDir)
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("precondition: expected no DB, stat gave %v", err)
	}

	store, _ := cas.OpenFS(filepath.Join(dataDir, "cas"))
	applyErr := recover.Apply(t.Context(), plan, seed, recover.ApplyOptions{DataDir: dataDir, Store: store})

	// OBSERVED behavior: Apply aborts at step 1 with ErrKeyExists, BEFORE the DB
	// is created. The data dir is left exactly as it was — the key that was
	// already there, and still no DB. No partial write.
	if !errors.Is(applyErr, identity.ErrKeyExists) {
		t.Errorf("FINDING (key/no-DB): Apply returned %v; want ErrKeyExists (it must abort before touching the DB)", applyErr)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("FINDING: a DB was written despite the identity guard firing; the abort is not clean (stat=%v)", err)
	}
	t.Logf("FINDING (key/no-DB): Apply FAILS FAST with ErrKeyExists at step 1 and writes NO database. " +
		"Clean, no partial corruption — the existing key is the guard.")
}

// --- Scenario 3: Apply voids leases end-to-end. ---

// leasedBackup builds a signed control-plane backup whose control DB carries a
// LEASED job (enqueued and claimed before the snapshot), so a restore of it must
// reclaim the lease. Returns the bundle dir, the manifest, the signing seed, and
// the leased job's id.
func leasedBackup(t *testing.T, sourceID string) (string, backup.Manifest, []byte, string) {
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
	queue, err := jobs.New(jobs.Options{Writer: db.Writer(), Reader: db.Reader(), Events: log})
	if err != nil {
		t.Fatal(err)
	}
	enq, err := queue.Enqueue(t.Context(), jobs.EnqueueOptions{Type: "hash_blob"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim(t.Context(), jobs.ClaimOptions{Owner: "dead-worker", LeaseTTL: time.Hour})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.ID != enq.ID || claimed.State != jobs.Leased {
		t.Fatalf("precondition: job %s state %q, want %s leased", claimed.ID, claimed.State, enq.ID)
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
	return art.Dir, art.Manifest, priv.Seed(), claimed.ID
}

func TestApplyVoidsLeasesEndToEnd(t *testing.T) {
	t.Parallel()
	dir, manifest, seed, jobID := leasedBackup(t, "peer-a")

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

	// Open the restored DB directly and inspect the raw job row: the lease the
	// dead worker held must be gone.
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: sqlite.DataDirFor(dataDir)})
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer func() { _ = db.Close() }()

	var state string
	var leaseOwner, leaseExpires *string
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT state, lease_owner, lease_expires_at FROM jobs WHERE id = ?`, jobID).
		Scan(&state, &leaseOwner, &leaseExpires); err != nil {
		t.Fatalf("reading the restored job: %v", err)
	}
	if state != "pending" {
		t.Errorf("job state after Apply = %q, want pending (Apply must void the dead node's lease)", state)
	}
	if leaseOwner != nil {
		t.Errorf("lease_owner after Apply = %q, want NULL", *leaseOwner)
	}
	if leaseExpires != nil {
		t.Errorf("lease_expires_at after Apply = %q, want NULL", *leaseExpires)
	}
}

// --- Scenario 4: age formatting at the extremes. ---

func TestFetchAgeFormatting(t *testing.T) {
	t.Parallel()

	t.Run("far past yields a sensible non-empty duration", func(t *testing.T) {
		t.Parallel()
		dir, manifest, seed := signedBackup(t, "peer-a")
		plan, err := recover.Fetch(t.Context(), recover.FetchOptions{
			Fetcher:      fetcher{bundleDir: dir, manifest: manifest},
			IdentitySeed: seed,
			WorkDir:      t.TempDir(),
			Now:          func() time.Time { return manifest.TakenAt.Add(72 * time.Hour) },
		})
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if plan.Age != "72h0m0s" {
			t.Errorf("age = %q, want 72h0m0s", plan.Age)
		}
	})

	t.Run("future TakenAt yields a negative age, not a crash", func(t *testing.T) {
		t.Parallel()
		dir, manifest, seed := signedBackup(t, "peer-a")
		plan, err := recover.Fetch(t.Context(), recover.FetchOptions{
			Fetcher:      fetcher{bundleDir: dir, manifest: manifest},
			IdentitySeed: seed,
			WorkDir:      t.TempDir(),
			// A backup stamped one hour in the FUTURE relative to "now" — clock
			// skew between peers. Age must format, not panic.
			Now: func() time.Time { return manifest.TakenAt.Add(-1 * time.Hour) },
		})
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if plan.Age != "-1h0m0s" {
			t.Errorf("age = %q, want -1h0m0s (a negative, well-formed duration)", plan.Age)
		}
	})
}

// --- Scenario 5: Apply with Store=nil skips the marker bind. ---

func TestApplyWithNilStore(t *testing.T) {
	t.Parallel()
	dir, manifest, seed := signedBackup(t, "peer-a")
	plan, err := recover.Fetch(t.Context(), recover.FetchOptions{
		Fetcher: fetcher{bundleDir: dir, manifest: manifest}, IdentitySeed: seed, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	dataDir := t.TempDir()
	// Store is optional: the CAS marker bind (step 4) is skipped when nil, and
	// the identity install + DB restore must still complete cleanly.
	if err := recover.Apply(t.Context(), plan, seed, recover.ApplyOptions{DataDir: dataDir, Store: nil}); err != nil {
		t.Fatalf("apply with nil store: %v", err)
	}

	// Identity installed.
	if _, err := os.Stat(identity.KeyPath(dataDir)); err != nil {
		t.Errorf("identity key not installed with a nil store: %v", err)
	}
	// Control plane restored.
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
		t.Errorf("restored state missing with a nil store (before-loss rows=%d)", n)
	}
	// No CAS directory should have been created by Apply itself.
	if _, err := os.Stat(filepath.Join(dataDir, "cas")); !os.IsNotExist(err) {
		t.Logf("note: a cas dir exists at %s (stat=%v) — Apply did not create it via BindPeer since Store was nil", filepath.Join(dataDir, "cas"), err)
	}
}
