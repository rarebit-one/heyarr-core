package sqlite

import (
	"database/sql"
	"strings"
	"testing"
)

// The schema's value is in what it refuses. These tests assert the refusals,
// because a constraint nobody has seen reject anything is a comment.

func exec(t *testing.T, db *DB, query string, args ...any) error {
	t.Helper()
	return db.InTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec(query, args...)
		return err
	})
}

func mustExec(t *testing.T, db *DB, query string, args ...any) {
	t.Helper()
	if err := exec(t, db, query, args...); err != nil {
		t.Fatalf("%s: %v", strings.SplitN(strings.TrimSpace(query), "\n", 2)[0], err)
	}
}

const ts = "2026-08-20T00:00:00Z"

func seedPeer(t *testing.T, db *DB) {
	t.Helper()
	mustExec(t, db, `INSERT INTO peers (id, name, site, is_self, created_at)
		VALUES ('p1', 'bartley', 'bartley-ridge', 1, ?)`, ts)
}

func seedWorkEdition(t *testing.T, db *DB) {
	t.Helper()
	mustExec(t, db, `INSERT INTO works (id, content_type, work_key, title, sort_title, created_at, updated_at)
		VALUES ('w1', 'movie', 'movie:arrival:2016', 'Arrival', 'arrival', ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO editions (id, work_id, created_at) VALUES ('e1', 'w1', ?)`, ts)
}

func seedBlob(t *testing.T, db *DB, hash string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO blobs (hash, size, first_seen_at) VALUES (?, 1024, ?)`, hash, ts)
}

const validHash = "blake3:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// ADR-0010: exactly one peer may claim to be this node. Two rows claiming self
// is unrecoverable once replication has run, so the database must refuse it
// rather than leaving it to a caller to remember.
func TestOnlyOnePeerCanBeSelf(t *testing.T) {
	db := openTestDB(t)
	seedPeer(t, db)
	err := exec(t, db, `INSERT INTO peers (id, name, is_self, created_at) VALUES ('p2', 'cove', 1, ?)`, ts)
	if err == nil {
		t.Fatal("a second peer was allowed to claim is_self")
	}
	// A non-self second peer is fine — that is Milestone 4.
	mustExec(t, db, `INSERT INTO peers (id, name, is_self, created_at) VALUES ('p2', 'cove', 0, ?)`, ts)
}

// ADR-0005: the blob primary key is the canonical byte identity, so a malformed
// one must never enter the catalog.
func TestBlobHashFormatIsEnforced(t *testing.T) {
	db := openTestDB(t)
	bad := map[string]string{
		"no algorithm prefix": strings.Repeat("a", 64),
		"wrong algorithm":     "sha256:" + strings.Repeat("a", 64),
		"too short":           "blake3:" + strings.Repeat("a", 63),
		"too long":            "blake3:" + strings.Repeat("a", 65),
		"uppercase hex":       "blake3:" + strings.Repeat("A", 64),
		"not hex":             "blake3:" + strings.Repeat("z", 64),
	}
	for name, hash := range bad {
		t.Run(name, func(t *testing.T) {
			err := exec(t, db, `INSERT INTO blobs (hash, size, first_seen_at) VALUES (?, 1, ?)`, hash, ts)
			if err == nil {
				t.Errorf("accepted malformed blob hash %q", hash)
			}
		})
	}
	if err := exec(t, db, `INSERT INTO blobs (hash, size, first_seen_at) VALUES (?, 1, ?)`, validHash, ts); err != nil {
		t.Errorf("rejected a well-formed hash: %v", err)
	}
}

// ADR-0020's central invariant, enforced in the database rather than trusted to
// every caller: a linked asset has no blob, and a managed or vault asset has one.
func TestAssetSourceClassInvariant(t *testing.T) {
	db := openTestDB(t)
	seedWorkEdition(t, db)
	seedBlob(t, db, validHash)

	t.Run("managed without a blob is rejected", func(t *testing.T) {
		err := exec(t, db, `INSERT INTO assets (id, edition_id, source_class, created_at, updated_at)
			VALUES ('a1', 'e1', 'managed', ?, ?)`, ts, ts)
		if err == nil {
			t.Error("a managed asset was allowed with no blob")
		}
	})

	t.Run("linked with a blob is rejected", func(t *testing.T) {
		err := exec(t, db, `INSERT INTO assets (id, edition_id, source_class, blob_hash, source_path, created_at, updated_at)
			VALUES ('a2', 'e1', 'linked', ?, '/home/j/Photos/x.heic', ?, ?)`, validHash, ts, ts)
		if err == nil {
			t.Error("a linked asset was allowed to carry a blob — that would reintroduce mutable blobs")
		}
	})

	t.Run("linked without a path is rejected", func(t *testing.T) {
		err := exec(t, db, `INSERT INTO assets (id, edition_id, source_class, created_at, updated_at)
			VALUES ('a3', 'e1', 'linked', ?, ?)`, ts, ts)
		if err == nil {
			t.Error("a linked asset was allowed with neither a blob nor a path — it would address nothing")
		}
	})

	t.Run("unknown source class is rejected", func(t *testing.T) {
		err := exec(t, db, `INSERT INTO assets (id, edition_id, source_class, blob_hash, created_at, updated_at)
			VALUES ('a4', 'e1', 'external', ?, ?, ?)`, validHash, ts, ts)
		if err == nil {
			t.Error("an unknown source_class was accepted")
		}
	})

	t.Run("the three valid shapes are accepted", func(t *testing.T) {
		mustExec(t, db, `INSERT INTO assets (id, edition_id, source_class, blob_hash, created_at, updated_at)
			VALUES ('ok1', 'e1', 'managed', ?, ?, ?)`, validHash, ts, ts)
		mustExec(t, db, `INSERT INTO assets (id, edition_id, source_class, source_path, created_at, updated_at)
			VALUES ('ok2', 'e1', 'linked', '/home/j/Photos/y.heic', ?, ?)`, ts, ts)
		mustExec(t, db, `INSERT INTO assets (id, edition_id, source_class, blob_hash, created_at, updated_at)
			VALUES ('ok3', 'e1', 'vault', ?, ?, ?)`, validHash, ts, ts)
	})
}

// Re-scanning a linked library must converge on the same rows rather than
// multiplying them.
func TestLinkedAssetsAreUniquePerPath(t *testing.T) {
	db := openTestDB(t)
	seedWorkEdition(t, db)
	mustExec(t, db, `INSERT INTO libraries (id, name, content_type, created_at) VALUES ('l1', 'photos', 'photo', ?)`, ts)
	mustExec(t, db, `INSERT INTO assets (id, edition_id, library_id, source_class, source_path, created_at, updated_at)
		VALUES ('a1', 'e1', 'l1', 'linked', '/home/j/Photos/x.heic', ?, ?)`, ts, ts)

	err := exec(t, db, `INSERT INTO assets (id, edition_id, library_id, source_class, source_path, created_at, updated_at)
		VALUES ('a2', 'e1', 'l1', 'linked', '/home/j/Photos/x.heic', ?, ?)`, ts, ts)
	if err == nil {
		t.Error("the same path was linked twice in one library")
	}
}

// §14: blobs are immutable and assets reference them. Deleting a blob out from
// under a live asset must be refused, not cascade.
func TestBlobsCannotBeDeletedWhileReferenced(t *testing.T) {
	db := openTestDB(t)
	seedWorkEdition(t, db)
	seedBlob(t, db, validHash)
	mustExec(t, db, `INSERT INTO assets (id, edition_id, source_class, blob_hash, created_at, updated_at)
		VALUES ('a1', 'e1', 'managed', ?, ?, ?)`, validHash, ts, ts)

	err := exec(t, db, `DELETE FROM blobs WHERE hash = ?`, validHash)
	if err == nil {
		t.Fatal("a blob was deleted while an asset still referenced it")
	}

	// Once the asset is gone, GC may reclaim it (ADR-0018).
	mustExec(t, db, `DELETE FROM assets WHERE id = 'a1'`)
	mustExec(t, db, `DELETE FROM blobs WHERE hash = ?`, validHash)
}

// A work is identified by (content_type, work_key) so that a rescan gets the
// same row rather than a duplicate (M1-11).
func TestWorkKeyIsUniquePerContentType(t *testing.T) {
	db := openTestDB(t)
	seedWorkEdition(t, db)

	err := exec(t, db, `INSERT INTO works (id, content_type, work_key, title, sort_title, created_at, updated_at)
		VALUES ('w2', 'movie', 'movie:arrival:2016', 'Arrival', 'arrival', ?, ?)`, ts, ts)
	if err == nil {
		t.Error("a duplicate work_key was accepted for the same content type")
	}
	// The same key under a different content type is a different work.
	mustExec(t, db, `INSERT INTO works (id, content_type, work_key, title, sort_title, created_at, updated_at)
		VALUES ('w3', 'book', 'movie:arrival:2016', 'Arrival', 'arrival', ?, ?)`, ts, ts)
}

// §12 lists thirteen content specialisations. Registering another must not be a
// migration — that is what the attributes column is for.
func TestNewContentTypesNeedNoSchemaChange(t *testing.T) {
	db := openTestDB(t)
	for _, ct := range []string{"movie", "episode", "book", "comic", "magazine", "paper", "audiobook"} {
		mustExec(t, db, `INSERT INTO works (id, content_type, work_key, title, sort_title, attributes, created_at, updated_at)
			VALUES (?, ?, ?, 'T', 't', json_object('kind', ?), ?, ?)`,
			"w-"+ct, ct, "k-"+ct, ct, ts, ts)
	}
	var n int
	if err := db.Reader().QueryRow(`SELECT count(DISTINCT content_type) FROM works`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Errorf("stored %d content types, want 7", n)
	}
}

func TestAttributesMustBeValidJSON(t *testing.T) {
	db := openTestDB(t)
	err := exec(t, db, `INSERT INTO works (id, content_type, work_key, title, sort_title, attributes, created_at, updated_at)
		VALUES ('w9', 'movie', 'k9', 'T', 't', 'not json', ?, ?)`, ts, ts)
	if err == nil {
		t.Error("invalid JSON was accepted into attributes")
	}
}

// STRICT tables: SQLite's default affinity would store 'banana' in an INTEGER
// column, and a catalog that silently accepts nonsense is worse than one that
// rejects it.
func TestTablesAreStrict(t *testing.T) {
	db := openTestDB(t)
	seedBlob(t, db, validHash)
	err := exec(t, db, `UPDATE blobs SET size = 'banana' WHERE hash = ?`, validHash)
	if err == nil {
		t.Error("a text value was accepted into an INTEGER column")
	}
}

func TestReplicaIsKeyedByBlobAndPeer(t *testing.T) {
	db := openTestDB(t)
	seedPeer(t, db)
	seedBlob(t, db, validHash)
	mustExec(t, db, `INSERT INTO replicas (blob_hash, peer_id, updated_at) VALUES (?, 'p1', ?)`, validHash, ts)

	err := exec(t, db, `INSERT INTO replicas (blob_hash, peer_id, updated_at) VALUES (?, 'p1', ?)`, validHash, ts)
	if err == nil {
		t.Error("a duplicate replica row was accepted for the same blob and peer")
	}
	err = exec(t, db, `INSERT INTO replicas (blob_hash, peer_id, state, updated_at) VALUES (?, 'p1', 'elsewhere', ?)`, validHash, ts)
	if err == nil {
		t.Error("an unknown replica state was accepted")
	}
}

// Cascades: deleting a library must not orphan its roots, and deleting a work
// must not orphan its editions.
func TestCascadesCleanUpChildren(t *testing.T) {
	db := openTestDB(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, content_type, created_at) VALUES ('l1', 'films', 'movie', ?)`, ts)
	mustExec(t, db, `INSERT INTO library_roots (id, library_id, path, created_at) VALUES ('r1', 'l1', '/srv/films', ?)`, ts)
	seedWorkEdition(t, db)

	mustExec(t, db, `DELETE FROM libraries WHERE id = 'l1'`)
	mustExec(t, db, `DELETE FROM works WHERE id = 'w1'`)

	for table, want := range map[string]int{"library_roots": 0, "editions": 0} {
		var n int
		if err := db.Reader().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("%s has %d rows after its parent was deleted, want %d", table, n, want)
		}
	}
}

// The rollback must actually drop what it created; a Down that "succeeds" while
// leaving tables behind is how a rollback leaves a database unusable.
func TestCoreMigrationRollsBackCleanly(t *testing.T) {
	db := openTestDB(t)

	var before int
	if err := db.Reader().QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN
		 ('peers','libraries','library_roots','works','editions','external_ids',
		  'assets','blobs','replicas','scanned_files','principals','api_tokens')`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 12 {
		t.Fatalf("expected 12 core tables after migrating, found %d", before)
	}

	migrateAllTheWayDown(t, db)

	var after int
	if err := db.Reader().QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN
		 ('peers','libraries','library_roots','works','editions','external_ids',
		  'assets','blobs','replicas','scanned_files','principals','api_tokens')`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Errorf("%d core tables survived the rollback, want 0", after)
	}
}
