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
		VALUES ('p1', 'peer-a', 'site-a', 1, ?)`, ts)
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
	err := exec(t, db, `INSERT INTO peers (id, name, is_self, created_at) VALUES ('p2', 'peer-b', 1, ?)`, ts)
	if err == nil {
		t.Fatal("a second peer was allowed to claim is_self")
	}
	// A non-self second peer is fine — that is Milestone 4.
	mustExec(t, db, `INSERT INTO peers (id, name, is_self, created_at) VALUES ('p2', 'peer-b', 0, ?)`, ts)
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
			VALUES ('a2', 'e1', 'linked', ?, '/srv/media/photos/x.heic', ?, ?)`, validHash, ts, ts)
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
			VALUES ('ok2', 'e1', 'linked', '/srv/media/photos/y.heic', ?, ?)`, ts, ts)
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
		VALUES ('a1', 'e1', 'l1', 'linked', '/srv/media/photos/x.heic', ?, ?)`, ts, ts)

	err := exec(t, db, `INSERT INTO assets (id, edition_id, library_id, source_class, source_path, created_at, updated_at)
		VALUES ('a2', 'e1', 'l1', 'linked', '/srv/media/photos/x.heic', ?, ?)`, ts, ts)
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

// Devices (M2-05). The CHECK constraints here are the floor under the API's
// validation, not a duplicate of it: the invariant is the database's job, and
// a second writer — a migration, a repair script, a future import — must not be
// able to store a profile the planner cannot reason about.
func TestDeviceProfileConstraints(t *testing.T) {
	db := openTestDB(t)

	insert := func(id, key, extra string) error {
		return exec(t, db, `INSERT INTO devices
			(id, device_key, name, platform, max_width, max_height, max_bitrate_bps, supports_hdr,
			 containers, video_codecs, audio_codecs, created_at, updated_at, last_seen_at)
			VALUES (?, ?, 'TV', 'tvos', `+extra+`, ?, ?, ?)`, id, key, ts, ts, ts)
	}

	if err := insert("d1", "k1", `3840, 2160, 120000000, 1, '["mp4"]', '["h264"]', '["aac"]'`); err != nil {
		t.Fatalf("a valid device was rejected: %v", err)
	}

	for _, tc := range []struct {
		name  string
		extra string
	}{
		{"negative width", `-1, 2160, 0, 0, '[]', '[]', '[]'`},
		{"negative bitrate", `0, 0, -1, 0, '[]', '[]', '[]'`},
		{"hdr is not a boolean", `0, 0, 0, 2, '[]', '[]', '[]'`},
		{"containers is not JSON", `0, 0, 0, 0, 'mp4', '[]', '[]'`},
		// The one json_valid alone would let through. `"h264"` is perfectly
		// valid JSON and is not a list of codecs; a scalar smuggled into a list
		// column produces a planner that silently matches nothing, which looks
		// like a device that can play nothing rather than like corruption.
		{"a scalar where a list belongs", `0, 0, 0, 0, '[]', '"h264"', '[]'`},
		{"an object where a list belongs", `0, 0, 0, 0, '[]', '[]', '{"aac":true}'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := insert("d-"+tc.name, "k-"+tc.name, tc.extra); err == nil {
				t.Error("the database accepted it")
			}
		})
	}
}

// One device, one row. The uniqueness is what makes registration idempotent,
// and it is enforced here rather than by the handler remembering to check.
func TestDeviceKeyIsUnique(t *testing.T) {
	db := openTestDB(t)
	stmt := `INSERT INTO devices (id, device_key, name, created_at, updated_at, last_seen_at)
		VALUES (?, 'tv-living-room', 'TV', ?, ?, ?)`
	mustExec(t, db, stmt, "d1", ts, ts, ts)
	if err := exec(t, db, stmt, "d2", ts, ts, ts); err == nil {
		t.Error("two devices share a device_key; registration would multiply rows per app launch")
	}
}

// Consumption sessions (M2-06, ADR-0024). The constraints here are the floor
// under the domain's state machine, not a duplicate of it: the state machine
// runs in a process, and a migration or a repair script does not go through it.
func TestConsumptionSessionConstraints(t *testing.T) {
	db := openTestDB(t)
	seedSessionPrerequisites(t, db)

	insert := func(id, cols string) error {
		return exec(t, db, `INSERT INTO consumption_sessions
			(id, asset_id, device_id, verb, state, progress_locator, progress_unit,
			 created_at, updated_at, started_at, ended_at)
			VALUES (?, 'a1', 'dev1', `+cols+`)`, id)
	}

	if err := insert("s1", `'watch', 'playing', '12.5', 'seconds', '`+ts+`', '`+ts+`', '`+ts+`', NULL`); err != nil {
		t.Fatalf("a valid session was rejected: %v", err)
	}

	for _, tc := range []struct {
		name string
		cols string
	}{
		{"an unknown verb", `'skim', 'playing', '', '', '` + ts + `', '` + ts + `', NULL, NULL`},
		{"an unknown state", `'watch', 'buffering', '', '', '` + ts + `', '` + ts + `', NULL, NULL`},
		{"an unknown unit", `'watch', 'playing', '5', 'furlongs', '` + ts + `', '` + ts + `', NULL, NULL`},
		{
			// A number nobody can interpret. The reverse — a unit with no
			// locator — is legitimate and is not refused.
			"a locator with no unit",
			`'watch', 'playing', '12.5', '', '` + ts + `', '` + ts + `', NULL, NULL`,
		},
		{
			// The pairing that makes the history readable: terminal states
			// have an end, non-terminal ones do not.
			"a completed session with no end time",
			`'watch', 'completed', '', '', '` + ts + `', '` + ts + `', '` + ts + `', NULL`,
		},
		{
			"a playing session with an end time",
			`'watch', 'playing', '', '', '` + ts + `', '` + ts + `', '` + ts + `', '` + ts + `'`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := insert("s-"+tc.name, tc.cols); err == nil {
				t.Error("the database accepted it")
			}
		})
	}

	// A unit with no locator is a reader that knows how to measure and has not
	// measured yet. Legitimate, and explicitly not refused.
	if err := insert("s-unit-only", `'read', 'created', '', 'page', '`+ts+`', '`+ts+`', NULL, NULL`); err != nil {
		t.Errorf("a unit with no locator was refused: %v", err)
	}
}

// A session for a deleted asset is a dangling reference every read path would
// have to special-case; a session for a deleted device is history worth keeping
// over a delete that is either an operator or a bug.
func TestConsumptionSessionReferentialBehaviour(t *testing.T) {
	db := openTestDB(t)
	seedSessionPrerequisites(t, db)
	mustExec(t, db, `INSERT INTO consumption_sessions
		(id, asset_id, device_id, verb, state, created_at, updated_at)
		VALUES ('s1', 'a1', 'dev1', 'watch', 'created', ?, ?)`, ts, ts)

	if err := exec(t, db, `DELETE FROM devices WHERE id = 'dev1'`); err == nil {
		t.Error("deleting a device with sessions was allowed; the history would vanish with it")
	}

	mustExec(t, db, `DELETE FROM assets WHERE id = 'a1'`)
	var n int
	if err := db.Reader().QueryRow(`SELECT count(*) FROM consumption_sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d sessions survived their asset being deleted", n)
	}
}

// seedSessionPrerequisites writes the rows a session references.
func seedSessionPrerequisites(t *testing.T, db *DB) {
	t.Helper()
	mustExec(t, db, `INSERT INTO devices (id, device_key, name, created_at, updated_at, last_seen_at)
		VALUES ('dev1', 'k1', 'TV', ?, ?, ?)`, ts, ts, ts)
	mustExec(t, db, `INSERT INTO works (id, content_type, work_key, title, sort_title, created_at, updated_at)
		VALUES ('w1', 'movie', 'k', 'T', 't', ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO editions (id, work_id, created_at) VALUES ('e1', 'w1', ?)`, ts)
	mustExec(t, db, `INSERT INTO blobs (hash, size, first_seen_at) VALUES
		('blake3:1111111111111111111111111111111111111111111111111111111111111111', 1, ?)`, ts)
	mustExec(t, db, `INSERT INTO assets (id, edition_id, source_class, blob_hash, created_at, updated_at)
		VALUES ('a1', 'e1', 'managed',
		 'blake3:1111111111111111111111111111111111111111111111111111111111111111', ?, ?)`, ts, ts)
}

// Desired items (M3-02). Like the consumption session constraints above, these
// are the floor under the domain's validation rather than a duplicate of it:
// the validator runs in a process, and a migration or a repair script does not
// go through it.
//
// They exist as tests because of a sabotage that PASSED. Removing the
// scope/edition CHECK broke nothing, because every API path goes through
// desired.Item.Validate() first and the constraint could never fire. A
// constraint nobody has watched refuse anything is decoration, and the
// migration's own comment claims "the database is the one enforcing it" —
// which was not true until something reached past the validator to check.
func TestDesiredItemConstraints(t *testing.T) {
	db := openTestDB(t)
	seedDesiredFixtures(t, db)

	insert := func(id, scope string, edition any, profile string) error {
		return exec(t, db, `INSERT INTO desired_items
			(id, scope, work_id, edition_id, quality_profile_id, monitor, reason,
			 created_at, updated_at)
			VALUES (?, ?, 'w1', ?, ?, 1, '', ?, ?)`, id, scope, edition, profile, ts, ts)
	}

	if err := insert("d1", "work", nil, "q1"); err != nil {
		t.Fatalf("a valid work-scoped want was rejected: %v", err)
	}
	if err := insert("d2", "edition", "e1", "q1"); err != nil {
		t.Fatalf("a valid edition-scoped want was rejected: %v", err)
	}

	for _, tc := range []struct {
		name    string
		scope   string
		edition any
	}{
		// The pair the CHECK exists for. An edition id sitting unused on a
		// work-scoped row is exactly the kind of field something later reads
		// without checking the scope.
		{"a work scope carrying an edition", "work", "e1"},
		{"an edition scope with no edition", "edition", nil},
		{"a scope that is not a scope", "episode", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := insert("bad-"+tc.name, tc.scope, tc.edition, "q2"); err == nil {
				t.Error("the database accepted it")
			}
		})
	}
}

// The item scope (M12, ADR-0056): a want may point at one byte-less Item, and
// the scope/target CHECK the migration rebuilt desired_items to enforce is the
// floor under desired.Item.Validate — the same reason the work/edition
// constraints above exist. These assert the refusals a repair script or a
// migration, which do not go through the validator, would otherwise sail past.
func TestDesiredItemItemScopeConstraints(t *testing.T) {
	db := openTestDB(t)
	seedDesiredFixtures(t, db)
	mustExec(t, db, `INSERT INTO items
		(id, work_id, edition_id, item_key, title, published_at, attributes, created_at, updated_at)
		VALUES ('i1', 'w1', NULL, 'S02E01', 'The Return', NULL, '{}', ?, ?)`, ts, ts)

	insert := func(id, scope string, edition, item any) error {
		return exec(t, db, `INSERT INTO desired_items
			(id, scope, work_id, edition_id, item_id, quality_profile_id, monitor, reason,
			 created_at, updated_at)
			VALUES (?, ?, 'w1', ?, ?, 'q1', 1, '', ?, ?)`, id, scope, edition, item, ts, ts)
	}

	if err := insert("ok", "item", nil, "i1"); err != nil {
		t.Fatalf("a valid item-scoped want was rejected: %v", err)
	}

	for _, tc := range []struct {
		name          string
		scope         string
		edition, item any
	}{
		// Each scope names its own target and refuses the other two's.
		{"an item scope with no item", "item", nil, nil},
		{"an item scope carrying an edition", "item", "e1", "i1"},
		{"a work scope carrying an item", "work", nil, "i1"},
		{"an edition scope carrying an item", "edition", "e1", "i1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := insert("bad-"+tc.name, tc.scope, tc.edition, tc.item); err == nil {
				t.Error("the database accepted it")
			}
		})
	}
}

// The uniqueness index now coalesces item_id too: two item-scoped wants over the
// same item under one profile are one want written twice, and would sail through
// a naive index because NULL is not equal to itself.
func TestItemScopedUniquenessIsPerItemAndProfile(t *testing.T) {
	db := openTestDB(t)
	seedDesiredFixtures(t, db)
	mustExec(t, db, `INSERT INTO items
		(id, work_id, edition_id, item_key, title, published_at, attributes, created_at, updated_at)
		VALUES ('i1', 'w1', NULL, 'S02E01', 'The Return', NULL, '{}', ?, ?)`, ts, ts)

	stmt := `INSERT INTO desired_items
		(id, scope, work_id, edition_id, item_id, quality_profile_id, monitor, reason, created_at, updated_at)
		VALUES (?, 'item', 'w1', NULL, 'i1', ?, 1, '', ?, ?)`

	mustExec(t, db, stmt, "d1", "q1", ts, ts)
	if err := exec(t, db, stmt, "d2", "q2", ts, ts); err != nil {
		t.Fatalf("two profiles over one item must both exist (§61): %v", err)
	}
	if err := exec(t, db, stmt, "d3", "q1", ts, ts); err == nil {
		t.Error("a duplicate item-scoped want was accepted; the index does not coalesce item_id")
	}
}

// §61: never one version per title. Two wants over one work under DIFFERENT
// profiles are two wants and must both exist; the same profile twice is one
// want written twice.
//
// The index is over coalesce(edition_id, ”) rather than the bare column
// because NULL <> NULL in SQL: a naive unique index would let every
// work-scoped duplicate straight through, which is the single most likely
// defect in this migration.
func TestDesiredItemUniquenessIsPerTargetAndProfile(t *testing.T) {
	db := openTestDB(t)
	seedDesiredFixtures(t, db)

	stmt := `INSERT INTO desired_items
		(id, scope, work_id, edition_id, quality_profile_id, monitor, reason, created_at, updated_at)
		VALUES (?, 'work', 'w1', NULL, ?, 1, '', ?, ?)`

	mustExec(t, db, stmt, "d1", "q1", ts, ts)

	// A second profile over the same work is a second want — the point of §61.
	if err := exec(t, db, stmt, "d2", "q2", ts, ts); err != nil {
		t.Fatalf("two profiles over one work must both exist (§61): %v", err)
	}

	// The same profile again is one want written twice. This is the assertion
	// that fails if the index forgets that NULL is not equal to itself.
	if err := exec(t, db, stmt, "d3", "q1", ts, ts); err == nil {
		t.Error("a duplicate want was accepted; the uniqueness index is not covering " +
			"work-scoped rows, whose edition_id is NULL")
	}
}

// A want for a work that no longer exists is a dangling reference every read
// path would have to special-case. The profile is the opposite: deleting the
// standard while leaving the desire makes satisfaction unanswerable (§56).
func TestDesiredItemReferentialBehaviour(t *testing.T) {
	db := openTestDB(t)
	seedDesiredFixtures(t, db)
	mustExec(t, db, `INSERT INTO desired_items
		(id, scope, work_id, edition_id, quality_profile_id, monitor, reason, created_at, updated_at)
		VALUES ('d1', 'work', 'w1', NULL, 'q1', 1, '', ?, ?)`, ts, ts)

	if err := exec(t, db, `DELETE FROM quality_profiles WHERE id = 'q1'`); err == nil {
		t.Error("a quality profile still measuring a want was deleted; §56 now has " +
			"nothing to evaluate that want against")
	}

	mustExec(t, db, `DELETE FROM works WHERE id = 'w1'`)
	var n int
	if err := db.Reader().QueryRow(`SELECT count(*) FROM desired_items`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("deleting a work left %d want(s) pointing at nothing", n)
	}
}

func seedDesiredFixtures(t *testing.T, db *DB) {
	t.Helper()
	mustExec(t, db, `INSERT INTO quality_profiles
		(id, name, description, accept, prefer, terminal, seeded, created_at, updated_at) VALUES
		('q1', 'living-room', '', '[]', '[]', '[]', 1, ?, ?),
		('q2', 'archival', '', '[]', '[]', '[]', 1, ?, ?)`, ts, ts, ts, ts)
	mustExec(t, db, `INSERT INTO works
		(id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES ('w1', 'movie', 'movie:arrival:2016', 'Arrival', 'arrival', 2016, '{}', ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO editions
		(id, work_id, label, edition_type, language, attributes, created_at)
		VALUES ('e1', 'w1', '2160p', 'remux', 'en', '{}', ?)`, ts)
}

// Acquisition state (M3-03). The floor under the domain's Validate: the
// validator runs in a process, and a migration or a repair script does not go
// through it.
func TestAcquisitionStateConstraints(t *testing.T) {
	db := openTestDB(t)
	seedDesiredFixtures(t, db)
	mustExec(t, db, `INSERT INTO desired_items
		(id, scope, work_id, edition_id, quality_profile_id, monitor, reason, created_at, updated_at)
		VALUES ('d1', 'work', 'w1', NULL, 'q1', 1, '', ?, ?)`, ts, ts)

	insert := func(id, phase string, managed int, content, placement string) error {
		mustExec(t, db, `INSERT OR IGNORE INTO desired_items
			(id, scope, work_id, edition_id, quality_profile_id, monitor, reason, created_at, updated_at)
			VALUES (?, 'work', 'w1', NULL, 'q2', 1, '', ?, ?)`, id, ts, ts)
		return exec(t, db, `INSERT INTO acquisition_state
			(desired_item_id, phase, managed, content, placement, detail,
			 phase_entered_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, '', ?, ?, ?)`,
			id, phase, managed, content, placement, ts, ts, ts)
	}

	if err := insert("d1", "idle", 1, "satisfied", "converging"); err != nil {
		t.Fatalf("a valid acquisition state was rejected: %v", err)
	}

	for _, tc := range []struct {
		name                      string
		phase                     string
		managed                   int
		content, placement, extra string
	}{
		// §56's forbidden combination.
		{
			name: "placed but not obtained", phase: "idle", managed: 1,
			content: "not_satisfied", placement: "satisfied",
		},
		{
			name: "converging but not obtained", phase: "idle", managed: 1,
			content: "unknown", placement: "converging",
		},
		// Content satisfaction is a statement about bytes Heyarr HOLDS.
		{
			name: "satisfied while holding nothing", phase: "idle", managed: 0,
			content: "satisfied", placement: "unknown",
		},
		// The two §64 names that are NOT phases here.
		{
			name: "a missing phase", phase: "missing", managed: 0,
			content: "unknown", placement: "unknown",
		},
		{
			name: "an available phase", phase: "available", managed: 1,
			content: "unknown", placement: "unknown",
		},
		// Value sets differ per axis, and the difference is enforced.
		{
			name: "converging content", phase: "idle", managed: 1,
			content: "converging", placement: "unknown",
		},
		{
			name: "content not applicable", phase: "idle", managed: 1,
			content: "not_applicable", placement: "unknown",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := insert("bad-"+tc.name, tc.phase, tc.managed, tc.content, tc.placement); err == nil {
				t.Error("the database accepted it")
			}
		})
	}
}

// The combination that must stay legal, and is the one an upgrade search
// produces: searching for something better while already holding a satisfying
// copy. Refusing this is the bug the domain's own upgrade test found.
func TestAnUpgradeSearchIsALegalState(t *testing.T) {
	db := openTestDB(t)
	seedDesiredFixtures(t, db)
	mustExec(t, db, `INSERT INTO desired_items
		(id, scope, work_id, edition_id, quality_profile_id, monitor, reason, created_at, updated_at)
		VALUES ('d1', 'work', 'w1', NULL, 'q1', 1, '', ?, ?)`, ts, ts)

	if err := exec(t, db, `INSERT INTO acquisition_state
		(desired_item_id, phase, managed, content, placement, detail,
		 phase_entered_at, created_at, updated_at)
		VALUES ('d1', 'searching', 1, 'satisfied', 'satisfied', '', ?, ?, ?)`, ts, ts, ts); err != nil {
		t.Fatalf("a monitored want searching for an upgrade while already satisfied is "+
			"the normal case for a good library, not an error: %v", err)
	}
}

// A want's acquisition state does not outlive it.
func TestAcquisitionStateCascadesFromTheWant(t *testing.T) {
	db := openTestDB(t)
	seedDesiredFixtures(t, db)
	mustExec(t, db, `INSERT INTO desired_items
		(id, scope, work_id, edition_id, quality_profile_id, monitor, reason, created_at, updated_at)
		VALUES ('d1', 'work', 'w1', NULL, 'q1', 1, '', ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO acquisition_state
		(desired_item_id, phase, managed, content, placement, detail,
		 phase_entered_at, created_at, updated_at)
		VALUES ('d1', 'idle', 0, 'unknown', 'unknown', '', ?, ?, ?)`, ts, ts, ts)

	mustExec(t, db, `DELETE FROM desired_items WHERE id = 'd1'`)
	var n int
	if err := db.Reader().QueryRow(`SELECT count(*) FROM acquisition_state`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d acquisition row(s) outlived their want", n)
	}
}

// Inventory reports and replica freshness (00023, M4-07).
//
// The receipt log's value is in what it refuses. A report whose mode is a word
// nobody defined, or whose counts are negative, is a report that would later be
// read as a fact about a peer — and a table that accepts nonsense is worse than
// one that rejects it.
func TestInventoryReportConstraints(t *testing.T) {
	db := openTestDB(t)
	seedPeer(t, db)

	mustExec(t, db, `INSERT INTO inventory_reports
		(id, peer_id, mode, observed_at, received_at, entries, added, changed, removed, unknown)
		VALUES ('r1', 'p1', 'full', ?, ?, 2, 2, 0, 0, 0)`, ts, ts)

	if err := exec(t, db, `INSERT INTO inventory_reports
		(id, peer_id, mode, observed_at, received_at)
		VALUES ('r2', 'p1', 'sideways', ?, ?)`, ts, ts); err == nil {
		t.Error("a report mode nobody defined was accepted")
	}
	if err := exec(t, db, `INSERT INTO inventory_reports
		(id, peer_id, mode, observed_at, received_at, removed)
		VALUES ('r3', 'p1', 'full', ?, ?, -1)`, ts, ts); err == nil {
		t.Error("a negative removal count was accepted")
	}
	if err := exec(t, db, `INSERT INTO inventory_reports
		(id, peer_id, mode, observed_at, received_at)
		VALUES ('r4', 'nobody', 'full', ?, ?)`, ts, ts); err == nil {
		t.Error("a report was accepted for a peer that does not exist")
	}

	// A peer's receipts do not outlive it. Revocation is deletion (ADR-0012),
	// and reports about a peer nobody trusts any more are not evidence of
	// anything.
	mustExec(t, db, `DELETE FROM peers WHERE id = 'p1'`)
	var n int
	if err := db.Reader().QueryRow(`SELECT count(*) FROM inventory_reports`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d inventory report(s) outlived the peer they describe", n)
	}
}

// reported_at is nullable and NOT backfilled, and that is the column's
// meaning rather than an omission: NULL is exactly "no peer has ever confirmed
// this row through an inventory report". A migration that invented a
// confirmation time from verified_at or updated_at would manufacture the one
// fact the column exists to make unfakeable.
func TestReplicaFreshnessStartsUnconfirmed(t *testing.T) {
	db := openTestDB(t)
	seedPeer(t, db)
	seedBlob(t, db, validHash)
	mustExec(t, db, `INSERT INTO replicas (blob_hash, peer_id, state, verified_at, updated_at)
		VALUES (?, 'p1', 'present', ?, ?)`, validHash, ts, ts)

	var reported sql.NullString
	if err := db.Reader().QueryRow(
		`SELECT reported_at FROM replicas WHERE blob_hash = ? AND peer_id = 'p1'`, validHash).
		Scan(&reported); err != nil {
		t.Fatal(err)
	}
	if reported.Valid {
		t.Errorf("a row written by a local path claims a peer confirmed it at %q", reported.String)
	}
}
