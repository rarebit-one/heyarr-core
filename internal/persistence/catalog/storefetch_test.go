package catalog_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// gamePackage seeds a game want under a store gate and the assets one GOG
// fetch produced: a base installer and its data part on one edition, a DLC
// installer on another. It returns the blob of each file by name.
func gamePackage(t *testing.T, h *harness) map[string]string {
	t.Helper()
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	h.exec(t, `UPDATE works SET content_type = 'game', work_key = 'game:arrival' WHERE id = 'w1'`)
	h.setProfile(t, `[{"attribute":"store","op":"in","value":["gog","steam","epic"]}]`)
	h.exec(t, `INSERT INTO editions (id, work_id, edition_key, label, edition_type, language, attributes, created_at)
		VALUES ('e-base', 'w1', 'gog:windows:arrival:1', 'GOG Windows', 'installer', '', '{"note":"kept"}', ?),
		       ('e-dlc', 'w1', 'gog:windows:arrival-dlc:2', 'GOG Windows', 'installer', '', '{}', ?)`, stamp, stamp)
	blobs := map[string]string{"exe": hashOf('1'), "bin": hashOf('2'), "dlc": hashOf('3')}
	for name, edition := range map[string]string{"exe": "e-base", "bin": "e-base", "dlc": "e-dlc"} {
		h.exec(t, `INSERT INTO blobs (hash, size, mime, first_seen_at)
			VALUES (?, 1073741824, 'application/octet-stream', ?)`, blobs[name], stamp)
		h.exec(t, `INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash,
				source_path, role, filename, mime, identification_source, attributes, created_at, updated_at)
			VALUES (?, ?, NULL, 'managed', ?, ?, 'primary', ?, 'application/octet-stream',
				'desired-item', '{"store":"gog"}', ?, ?)`,
			"a-"+name, edition, blobs[name], "/srv/games/"+name, name, stamp, stamp)
	}
	return blobs
}

func verify(t *testing.T, h *harness, blobs map[string]string) {
	t.Helper()
	pkg := catalog.VerifiedStorePackage{
		Store: "gog", DRMFree: true, HasDRMFree: true,
		Blobs: []string{blobs["exe"], blobs["bin"], blobs["dlc"]},
	}
	if err := h.cat.RecordVerifiedStoreEditions(context.Background(),
		[]string{"e-base", "e-dlc", "e-base", ""}, pkg); err != nil {
		t.Fatal(err)
	}
}

func reconcile(t *testing.T, h *harness) catalog.ReconcileResult {
	t.Helper()
	got, err := h.cat.ReconcileDesired(context.Background(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A verified store package is the one thing that puts store facts on an
// edition (ADR-0120), and once it has, the game want it filled is satisfied
// under a store gate — the end of the path ADR-0118 left open, where a scanned
// installer part could only ever report undetermined.
func TestAVerifiedStorePackageSatisfiesTheGameWant(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	blobs := gamePackage(t, h)

	if got := reconcile(t, h); got.Content.Satisfaction == acquisition.SatisfactionSatisfied {
		t.Fatal("satisfied before any store verified the package")
	}
	verify(t, h, blobs)
	verify(t, h, blobs) // ingest is re-run; the second write changes nothing
	if n := h.eventsOfType(t, events.TypeEditionVerified); n != 2 {
		t.Errorf("%d %s events for two editions verified twice, want 2", n, events.TypeEditionVerified)
	}

	var raw string
	if err := h.db.Reader().QueryRow(`SELECT attributes FROM editions WHERE id = 'e-base'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var attrs map[string]any
	if err := json.Unmarshal([]byte(raw), &attrs); err != nil {
		t.Fatal(err)
	}
	if attrs["store"] != "gog" || attrs["drm_free"] != true || attrs["note"] != "kept" {
		t.Errorf("edition attributes %v, want the store facts added and the rest kept", attrs)
	}
	if files, _ := attrs["package_blobs"].([]any); len(files) != 3 {
		t.Errorf("package_blobs %v, want all three files of the fetch", attrs["package_blobs"])
	}

	if got := reconcile(t, h); got.Content.Satisfaction != acquisition.SatisfactionSatisfied {
		t.Errorf("content = %s after the store verified the package, want satisfied", got.Content.Satisfaction)
	}
}

// The store vouched for the whole package, never one file of it. A data part
// that goes missing un-satisfies the game; so does holding the DLC without the
// game it belongs to.
func TestAPackageMissingAnyFileDoesNotSatisfy(t *testing.T) {
	t.Parallel()
	for name, lose := range map[string][]string{
		"the base installer's data part":  {"a-bin"},
		"the DLC alone, without the game": {"a-exe", "a-bin"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			verify(t, h, gamePackage(t, h))
			if got := reconcile(t, h); got.Content.Satisfaction != acquisition.SatisfactionSatisfied {
				t.Fatalf("fixture: whole package is %s", got.Content.Satisfaction)
			}
			for _, id := range lose {
				h.exec(t, `UPDATE assets SET missing_since = ? WHERE id = ?`, stamp, id)
			}
			if got := reconcile(t, h); got.Content.Satisfaction == acquisition.SatisfactionSatisfied {
				t.Errorf("still satisfied with %v missing; a fragment of a package is not the game", lose)
			}
		})
	}
}

// Store facts written without the package they vouch for are a claim nobody
// can check is still whole, so they count for nothing.
func TestStoreFactsWithoutAPackageDoNotSatisfy(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	gamePackage(t, h)
	h.exec(t, `UPDATE editions SET attributes = '{"store":"gog","drm_free":true}'`)
	if got := reconcile(t, h); got.Content.Satisfaction == acquisition.SatisfactionSatisfied {
		t.Error("store facts with no package list satisfied the want")
	}
}

// A game is backed up at a second site only when every file of its package is
// there: placement is the package's, not the one asset that happened to be
// chosen as satisfying.
func TestAPackageIsPlacedOnlyWhenEveryFileIs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t)
	self, err := h.cat.SelfPeer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	blobs := gamePackage(t, h)
	verify(t, h, blobs)
	hold := func(b string) {
		h.exec(t, `INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, updated_at)
			VALUES (?, ?, 'present', 1024, ?)`, b, self, stamp)
	}

	// Replicate exactly the file reconciliation picked as satisfying — the one
	// a per-asset placement check would look at, and be fooled by.
	chosen := reconcile(t, h).Content.SatisfiedBy
	first := blobs[chosen[len("a-"):]]
	hold(first)
	if got := reconcile(t, h); got.Placement.Satisfaction == acquisition.SatisfactionSatisfied {
		t.Fatalf("placement satisfied with only %s of three package files replicated: %s", chosen, got.Placement.Detail)
	}
	for _, b := range blobs {
		if b != first {
			hold(b)
		}
	}
	if got := reconcile(t, h); got.Placement.Satisfaction != acquisition.SatisfactionSatisfied {
		t.Errorf("placement = %s with every file replicated (%s)", got.Placement.Satisfaction, got.Placement.Detail)
	}
}

// A store that does not say whether its copy is DRM-free leaves drm_free
// absent — undetermined, never a confident false.
func TestAStoreThatDoesNotSayLeavesDRMFreeAbsent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t)
	h.exec(t, `INSERT INTO editions (id, work_id, label, edition_type, language, attributes, created_at)
		VALUES ('e-depot', 'w1', 'Steam depot', 'depot', '', '{}', ?)`, stamp)
	if err := h.cat.RecordVerifiedStoreEditions(ctx, []string{"e-depot"},
		catalog.VerifiedStorePackage{Store: "steam", Blobs: []string{hashOf('9')}}); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := h.db.Reader().QueryRow(`SELECT attributes FROM editions WHERE id = 'e-depot'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var attrs map[string]any
	if err := json.Unmarshal([]byte(raw), &attrs); err != nil {
		t.Fatal(err)
	}
	if _, set := attrs["drm_free"]; set || attrs["store"] != "steam" {
		t.Errorf("attributes = %s, want the store and no drm_free", raw)
	}
}

// A package spans editions, so its completeness is the Work's: an edition-scoped
// want for the base game is satisfied by a whole package even though the DLC
// edition lies outside its scope.
func TestAnEditionScopedWantSeesItsWholePackage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	blobs := gamePackage(t, h)
	h.exec(t, `UPDATE desired_items SET scope = 'edition', edition_id = 'e-base' WHERE id = ?`, h.want)
	verify(t, h, blobs)
	if got := reconcile(t, h); got.Content.Satisfaction != acquisition.SatisfactionSatisfied {
		t.Errorf("edition-scoped want = %s with the whole package held, want satisfied", got.Content.Satisfaction)
	}
}

// A satisfied want's store fetch succeeded, so its backoff streak ends: losing
// the bytes later starts the next fetch from the first delay.
func TestASatisfiedWantClearsItsStoreBackoff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t)
	blobs := gamePackage(t, h)
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if _, err := h.cat.RecordStoreFetchAttempt(ctx, h.want, 4, at, at); err != nil {
		t.Fatal(err)
	}
	verify(t, h, blobs)
	reconcile(t, h)
	var n int
	if err := h.db.Reader().QueryRow(`SELECT count(*) FROM store_fetch_schedule WHERE desired_item_id = ?`, h.want).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("a satisfied want kept its fruitless store backoff")
	}
}
