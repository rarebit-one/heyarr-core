package catalog_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// The upgrade scan and supersession, against a real database (§60, M3-06).
//
// The decision is table-tested in the domain. What these add is the query that
// finds the incumbent and the write that supersedes it — and the safety
// property the whole feature turns on: the incumbent goes only AFTER the
// replacement is under management.

// setTerminal gives the harness profile a terminal condition, so the "already
// as good as it gets" branch has something to reach.
func (h *harness) setTerminal(t *testing.T, terminal string) {
	t.Helper()
	h.exec(t, `UPDATE quality_profiles SET terminal = ? WHERE id = 'q1'`, terminal)
}

// setMonitor turns the harness want's monitoring on or off.
func (h *harness) setMonitor(t *testing.T, on bool) {
	t.Helper()
	v := 0
	if on {
		v = 1
	}
	h.exec(t, `UPDATE desired_items SET monitor = ? WHERE id = ?`, v, h.want)
}

const terminal2160 = `[{"attribute":"resolution","op":"gte","value":2160}]`

// seedAsset adds one more asset under the harness's work, in its own edition.
//
// seedSatisfying (reconcile_test.go) can only be called once per want: it uses
// a fixed edition label, and editions are unique on (work_id, edition_key). An
// upgrade needs TWO assets under one work by definition, so this takes a
// distinct key.
func (h *harness) seedAsset(t *testing.T, hash, editionKey, editionType string, height int64) string {
	t.Helper()
	h.exec(t, `INSERT INTO blobs (hash, size, mime, first_seen_at)
		VALUES (?, 8589934592, 'video/x-matroska', ?)`, hash, stamp)
	h.exec(t, `INSERT INTO editions
			(id, work_id, label, edition_key, edition_type, language, attributes, created_at)
		VALUES (?, 'w1', ?, ?, ?, 'en', '{}', ?)`,
		"e-"+hash, editionKey, editionKey, editionType, stamp)
	h.exec(t, `INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash,
			source_path, role, filename, mime, identification_source, created_at, updated_at)
		VALUES (?, ?, NULL, 'managed', ?, ?, 'primary', 'x.mkv',
			'video/x-matroska', 'path', ?, ?)`,
		"a-"+hash, "e-"+hash, hash, "/srv/"+editionKey+".mkv", stamp, stamp)
	h.exec(t, `INSERT INTO blob_probes
			(blob_hash, container, format_long, duration_seconds, bitrate_bps, streams,
			 bytes_read, materialised, probed_at)
		VALUES (?, 'matroska,webm', '', 7200.0, 8000000, ?, 1024, 0, ?)`,
		hash, `[{"type":"video","codec":"h264","height":`+itoa(height)+`,"profile":"High"},`+
			`{"type":"audio","codec":"aac","channels":6}]`, stamp)
	return "a-" + hash
}

// A satisfied, monitored, non-terminal want is eligible. This is the state the
// whole workflow exists for.
func TestScanFindsAnEligibleWant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	h.setProfile(t, gate1080)
	h.setTerminal(t, terminal2160)
	h.seedSatisfying(t, "blake3:"+repeat("a", 64), 1080, "web-dl")
	if _, err := h.cat.ReconcileDesired(ctx, h.want); err != nil {
		t.Fatal(err)
	}

	got, err := h.cat.ScanForUpgrades(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got.Considered != 1 {
		t.Fatalf("considered %d monitored wants, want 1", got.Considered)
	}
	if len(got.Eligible) != 1 {
		t.Fatalf("%d eligible, want 1", len(got.Eligible))
	}
	e := got.Eligible[0]
	if e.DesiredItemID != h.want {
		t.Errorf("eligible want = %q", e.DesiredItemID)
	}
	if e.IncumbentID == "" {
		t.Error("an eligible want names the incumbent it would replace")
	}
}

// A terminal incumbent has nothing left to want, so it is not eligible.
func TestScanSkipsATerminalWant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	h.setProfile(t, gate1080)
	h.setTerminal(t, terminal2160)
	// 2160p meets the terminal condition.
	h.seedSatisfying(t, "blake3:"+repeat("b", 64), 2160, "remux")
	if _, err := h.cat.ReconcileDesired(ctx, h.want); err != nil {
		t.Fatal(err)
	}

	got, err := h.cat.ScanForUpgrades(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Eligible) != 0 {
		t.Errorf("a terminal want was reported eligible: %+v", got.Eligible[0].Verdict)
	}
}

// THE case #98 says matters most: unmonitored, satisfied, and a strictly
// better copy is theoretically available. The operator said "get me this", not
// "keep improving this".
//
// Running the loop over unmonitored wants is how *arr installations
// re-download libraries nobody asked them to touch.
func TestScanNeverConsidersAnUnmonitoredWant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	h.setProfile(t, gate1080)
	h.setTerminal(t, terminal2160)
	// Satisfied and NOT terminal — so the only thing making it ineligible is
	// the operator's instruction.
	h.seedSatisfying(t, "blake3:"+repeat("c", 64), 1080, "web-dl")
	if _, err := h.cat.ReconcileDesired(ctx, h.want); err != nil {
		t.Fatal(err)
	}

	// Confirm it IS eligible while monitored, so the assertion below is about
	// monitoring and not about something else being wrong.
	if got, err := h.cat.ScanForUpgrades(ctx, 100); err != nil {
		t.Fatal(err)
	} else if len(got.Eligible) != 1 {
		t.Fatalf("setup: expected one eligible want, got %d", len(got.Eligible))
	}

	h.setMonitor(t, false)

	got, err := h.cat.ScanForUpgrades(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got.Considered != 0 {
		t.Errorf("an unmonitored want was considered at all (%d); the query should "+
			"not read it", got.Considered)
	}
	if len(got.Eligible) != 0 {
		t.Error("an unmonitored want was reported upgradable")
	}
}

// A want with nothing acceptable held is an ACQUISITION, not an upgrade. The
// search job owns it, and reporting it here would make two jobs fight over the
// same row.
func TestScanSkipsAnUnsatisfiedWant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	h.setProfile(t, `[{"attribute":"resolution","op":"gte","value":2160}]`)
	// 1080p fails the gate.
	h.seedSatisfying(t, "blake3:"+repeat("d", 64), 1080, "web-dl")
	if _, err := h.cat.ReconcileDesired(ctx, h.want); err != nil {
		t.Fatal(err)
	}

	got, err := h.cat.ScanForUpgrades(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Eligible) != 0 {
		t.Errorf("an unsatisfied want was reported upgradable: %+v", got.Eligible[0].Verdict)
	}
}

// A scan over a library with nothing upgradable emits nothing. Most wants are
// terminal, unmonitored or unsatisfied most of the time, and a beat that
// announced that every pass would be a heartbeat.
func TestAScanWithNothingToSayIsSilent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	h.setProfile(t, gate1080)
	h.setTerminal(t, terminal2160)
	h.seedSatisfying(t, "blake3:"+repeat("e", 64), 2160, "remux")
	if _, err := h.cat.ReconcileDesired(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	settled := h.eventCount(t)

	for range 5 {
		if _, err := h.cat.ScanForUpgrades(ctx, 100); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.eventCount(t); got != settled {
		t.Errorf("five scans emitted %d event(s); a library with nothing upgradable "+
			"must be silent", got-settled)
	}
}

// A scan is a READ. Even over an eligible library it writes nothing and emits
// nothing — the decision to act is a separate step, so the beat can run often
// without announcing the same available upgrade every pass.
func TestAScanOverAnEligibleLibraryStillEmitsNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	h.setProfile(t, gate1080)
	h.setTerminal(t, terminal2160)
	h.seedSatisfying(t, "blake3:"+repeat("f", 64), 1080, "web-dl")
	if _, err := h.cat.ReconcileDesired(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	settled := h.eventCount(t)

	got, err := h.cat.ScanForUpgrades(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Eligible) != 1 {
		t.Fatalf("setup: %d eligible", len(got.Eligible))
	}
	if n := h.eventCount(t); n != settled {
		t.Errorf("a scan emitted %d event(s); it is a read", n-settled)
	}
}

// THE safety property. An upgrade that fails after the incumbent is gone turns
// a satisfied want into an empty one — which is worse than never having
// upgraded at all.
func TestTheIncumbentSurvivesUntilTheReplacementIsUnderManagement(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	hash := "blake3:" + repeat("1", 64)
	h.setProfile(t, gate1080)
	h.seedSatisfying(t, hash, 1080, "web-dl")
	incumbent := "a-" + hash

	// The replacement does not exist yet — the download failed, the ingest
	// never ran, whatever. Supersession must refuse.
	err := h.cat.SupersedeIncumbent(ctx, h.want, incumbent, "a-replacement-that-never-arrived")
	if !errors.Is(err, catalog.ErrIncumbentNotSuperseded) {
		t.Fatalf("expected ErrIncumbentNotSuperseded, got %v", err)
	}

	// And the incumbent is still there, so the want is still satisfied.
	var n int
	if err := h.db.Reader().QueryRow(
		`SELECT count(*) FROM assets WHERE id = ?`, incumbent).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("the incumbent was removed even though the replacement never arrived")
	}
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	result, err := h.cat.ReconcileDesired(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content.Satisfaction != acquisition.SatisfactionSatisfied {
		t.Errorf("content = %s; a failed upgrade must leave the want satisfied",
			result.Content.Satisfaction)
	}
}

// Supersession is a LOGICAL delete (ADR-0018): the Asset row goes, the Blob
// stays, and gc_blobs reclaims it later if nothing else references it.
func TestSupersessionIsLogicalAndKeepsTheBytes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.setProfile(t, gate1080)

	oldHash := "blake3:" + repeat("2", 64)
	newHash := "blake3:" + repeat("3", 64)
	incumbent := h.seedAsset(t, oldHash, "1080p-web", "web-dl", 1080)
	replacement := h.seedAsset(t, newHash, "2160p-remux", "remux", 2160)

	before := h.eventCount(t)
	if err := h.cat.SupersedeIncumbent(ctx, h.want, incumbent, replacement); err != nil {
		t.Fatal(err)
	}

	// The asset is gone.
	var assets int
	if err := h.db.Reader().QueryRow(
		`SELECT count(*) FROM assets WHERE id = ?`, incumbent).Scan(&assets); err != nil {
		t.Fatal(err)
	}
	if assets != 0 {
		t.Error("the incumbent asset survived supersession")
	}

	// The BLOB is not. This is the whole of ADR-0018 and the first question
	// anyone reading the event log will have.
	var blobs int
	if err := h.db.Reader().QueryRow(
		`SELECT count(*) FROM blobs WHERE hash = ?`, oldHash).Scan(&blobs); err != nil {
		t.Fatal(err)
	}
	if blobs != 1 {
		t.Error("supersession unlinked bytes; ADR-0018 says a later GC sweep does that")
	}

	// And it emitted, saying so.
	if got := h.eventCount(t); got != before+1 {
		t.Fatalf("supersession emitted %d event(s), want 1", got-before)
	}
}

// Supersession is idempotent, because the job that calls it will be re-run
// (invariant 9).
func TestSupersessionIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.setProfile(t, gate1080)

	oldHash := "blake3:" + repeat("4", 64)
	newHash := "blake3:" + repeat("5", 64)
	incumbent := h.seedAsset(t, oldHash, "1080p-web", "web-dl", 1080)
	replacement := h.seedAsset(t, newHash, "2160p-remux", "remux", 2160)

	if err := h.cat.SupersedeIncumbent(ctx, h.want, incumbent, replacement); err != nil {
		t.Fatal(err)
	}
	settled := h.eventCount(t)

	for range 3 {
		if err := h.cat.SupersedeIncumbent(ctx, h.want, incumbent, replacement); err != nil {
			t.Fatalf("re-running supersession failed: %v", err)
		}
	}
	if got := h.eventCount(t); got != settled {
		t.Errorf("re-running supersession emitted %d event(s); it changed nothing",
			got-settled)
	}
	// And the replacement is untouched.
	var n int
	if err := h.db.Reader().QueryRow(
		`SELECT count(*) FROM assets WHERE id = ?`, replacement).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("the replacement was removed by a repeated supersession")
	}
}

// An asset cannot supersede itself. Refusing is better than deleting the asset
// that was supposed to survive.
func TestAnAssetCannotSupersedeItself(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.setProfile(t, gate1080)
	hash := "blake3:" + repeat("6", 64)
	h.seedSatisfying(t, hash, 1080, "web-dl")
	asset := "a-" + hash

	if err := h.cat.SupersedeIncumbent(ctx, h.want, asset, asset); err == nil {
		t.Fatal("an asset superseding itself must be refused")
	}
	var n int
	if err := h.db.Reader().QueryRow(
		`SELECT count(*) FROM assets WHERE id = ?`, asset).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("the asset was deleted by a self-supersession")
	}
}

// A missing replacement — one the scanner marked gone — is not under
// management, so it cannot be superseded to.
func TestAMissingReplacementCannotSupersede(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.setProfile(t, gate1080)

	oldHash := "blake3:" + repeat("7", 64)
	newHash := "blake3:" + repeat("8", 64)
	h.seedAsset(t, oldHash, "1080p-web", "web-dl", 1080)
	h.seedAsset(t, newHash, "2160p-remux", "remux", 2160)
	h.exec(t, `UPDATE assets SET missing_since = ? WHERE id = ?`, stamp, "a-"+newHash)

	err := h.cat.SupersedeIncumbent(ctx, h.want, "a-"+oldHash, "a-"+newHash)
	if !errors.Is(err, catalog.ErrIncumbentNotSuperseded) {
		t.Fatalf("expected ErrIncumbentNotSuperseded, got %v", err)
	}
}

// The upgrade decision uses the CURRENT profile, not a cached score. A profile
// edit changes what "better" means, and an upgrade reported against a standard
// nobody is using any more is worse than no upgrade at all.
func TestRaisingTheProfileMakesASatisfiedWantIneligible(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	h.setProfile(t, gate1080)
	h.setTerminal(t, terminal2160)
	h.seedSatisfying(t, "blake3:"+repeat("9", 64), 1080, "web-dl")
	if _, err := h.cat.ReconcileDesired(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	if got, err := h.cat.ScanForUpgrades(ctx, 100); err != nil {
		t.Fatal(err)
	} else if len(got.Eligible) != 1 {
		t.Fatalf("setup: %d eligible", len(got.Eligible))
	}

	// Raise the gate above what is held. The want is now MISSING, which is an
	// acquisition rather than an upgrade.
	h.setProfile(t, `[{"attribute":"resolution","op":"gte","value":2160}]`)
	if _, err := h.cat.ReconcileDesired(ctx, h.want); err != nil {
		t.Fatal(err)
	}

	got, err := h.cat.ScanForUpgrades(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Eligible) != 0 {
		t.Errorf("a want the profile no longer accepts was reported upgradable: %+v",
			got.Eligible[0].Verdict)
	}
}

// RecordUpgradeFound refuses to announce a non-upgrade, so an "upgrade found"
// in the log always means one was.
func TestRecordUpgradeFoundRefusesANonUpgrade(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	before := h.eventCount(t)

	err := h.cat.RecordUpgradeFound(ctx, h.want, acquisition.UpgradeVerdict{
		Status: acquisition.UpgradeNoBetterCandidate,
		Detail: "nothing better",
	})
	if err == nil {
		t.Fatal("announcing a non-upgrade must be refused")
	}
	if got := h.eventCount(t); got != before {
		t.Errorf("a refused announcement emitted %d event(s)", got-before)
	}
}

// And it does announce a real one, carrying the size of the improvement —
// which is what makes an upgrade reviewable rather than merely reported.
func TestRecordUpgradeFoundEmits(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	before := h.eventCount(t)

	if err := h.cat.RecordUpgradeFound(ctx, h.want, acquisition.UpgradeVerdict{
		Status:      acquisition.UpgradeAvailable,
		Candidate:   acquisition.ReleaseCandidate{ID: "c1", Title: "a better copy"},
		Improvement: 20,
		Detail:      "a candidate scores 20 against the 0 that is held",
	}); err != nil {
		t.Fatal(err)
	}
	if got := h.eventCount(t); got != before+1 {
		t.Fatalf("emitted %d event(s), want 1", got-before)
	}
}
