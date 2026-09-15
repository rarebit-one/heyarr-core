package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// Ingest of completed acquisitions (§65, §66, M3-13).
//
// Everything here runs against the FAKE indexer and a real filesystem: no
// download client, no real indexer, no network. What is being tested is the
// join — a file on disk, hashed by us, becoming a managed asset and driving
// §64's edges — and the refusals, which are as much the deliverable as the
// successes.

// ingestHarness extends the search harness with the storage a real ingest
// needs: a CAS, a library root, and the pipeline itself.
type ingestHarness struct {
	*searchHarness
	pipeline  *ingest.Pipeline
	root      string
	downloads string
	queue     *jobs.Queue
}

func newIngestHarness(t *testing.T) *ingestHarness {
	t.Helper()
	base := newSearchHarness(t)
	ctx := t.Context()

	dir := t.TempDir()
	libraryRoot := filepath.Join(dir, "library")
	downloadDir := filepath.Join(dir, "downloads")
	casRoot := filepath.Join(dir, "cas")
	for _, d := range []string{libraryRoot, downloadDir, casRoot} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	base.exec(t, `INSERT INTO libraries (id, name, content_type, enabled, created_at)
		VALUES ('lib1', 'films', 'movie', 1, ?)`, stamp)
	base.exec(t, `INSERT INTO library_roots
		(id, library_id, path, ingest_mode, enabled, created_at)
		VALUES ('root1', 'lib1', ?, 'reflink', 1, ?)`, libraryRoot, stamp)

	store, err := cas.OpenFS(casRoot)
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := ingest.New(ingest.Options{
		Store:      NewCASByteStore(store),
		Catalog:    base.cat,
		Identifier: identification.NewRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{
		Writer: base.db.Writer(), Reader: base.db.Reader(),
	})
	if err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(jobs.Options{
		Writer: base.db.Writer(), Reader: base.db.Reader(), Events: eventLog,
	})
	if err != nil {
		t.Fatal(err)
	}

	_ = ctx
	return &ingestHarness{
		searchHarness: base, pipeline: pipeline,
		root: libraryRoot, downloads: downloadDir, queue: queue,
	}
}

// selectAndComplete drives the want to VERIFYING with a real file on disk,
// which is the state an ingest job actually finds.
// offerID names the candidate the search selects; it defaults to "good" (a
// movie want accepts any title), and a series test passes an episode id like
// "S01E01" so the offer clears ADR-0093's episode-containment gate.
func (h *ingestHarness) selectAndComplete(t *testing.T, filename string, contents []byte, offerID ...string) string {
	t.Helper()
	ctx := t.Context()

	id := "good"
	if len(offerID) > 0 {
		id = offerID[0]
	}
	h.fake.Offer("Arrival", offer(id, 2160, "hevc"))
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}
	if got := h.state(t).Name(); got != "SELECTED" {
		t.Fatalf("setup: want is %s, expected SELECTED", got)
	}

	path := filepath.Join(h.downloads, filename)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := h.cat.RecordAcquisition(ctx, catalog.Acquisition{
		ID: NewAcquisitionID(), DesiredItemID: h.want,
		Provider: "fake-downloader", ExternalID: "infohash-1",
		ExternalName: filename, RemotePath: path, LocalPath: path,
		BytesTotal: int64(len(contents)), BytesDone: int64(len(contents)),
	}); err != nil {
		t.Fatal(err)
	}

	for _, tr := range []acquisition.Transition{
		acquisition.TransitionQueue,
		acquisition.TransitionStartDownload,
		acquisition.TransitionDownloaded,
	} {
		if _, err := h.cat.AdvanceAcquisition(ctx, h.want, tr, ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.state(t).Phase; got != acquisition.PhaseVerifying {
		t.Fatalf("setup: phase is %s, expected verifying", got)
	}
	return path
}

func (h *ingestHarness) ingest(t *testing.T) error {
	t.Helper()
	payload, err := json.Marshal(acquisition.IngestPayload{DesiredItemID: h.want})
	if err != nil {
		t.Fatal(err)
	}
	handler := IngestAcquisitionHandler(
		h.cat, h.cat, h.pipeline, h.queue, slog.New(slog.DiscardHandler))
	return handler(t.Context(), jobs.Job{Type: acquisition.IngestJobType, Payload: payload})
}

func (h *ingestHarness) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := h.db.Reader().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The happy path: a completed acquisition becomes a managed asset with a
// verified blob, and the want reaches AVAILABLE.
//
// AVAILABLE, not CONTENT_SATISFIED — ingest produces bytes, and whether they
// satisfy the profile is reconciliation's question (§56, M3-05). Asserting
// CONTENT_SATISFIED here would be asserting that this handler answered a
// question it must not answer.
func TestACompletedAcquisitionBecomesAManagedAsset(t *testing.T) {
	h := newIngestHarness(t)
	h.selectAndComplete(t, "Arrival.2016.2160p.mkv", []byte("the actual bytes of a film"))

	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}

	state := h.state(t)
	if state.Phase != acquisition.PhaseIdle {
		t.Fatalf("phase = %s, want idle", state.Phase)
	}
	if !state.Managed {
		t.Error("after an ingest Heyarr holds bytes for this want")
	}
	if got := state.Name(); got != "AVAILABLE" {
		t.Fatalf("state = %s, want AVAILABLE — whether the bytes SATISFY is "+
			"reconciliation's question, not this handler's", got)
	}
	if state.Content != acquisition.SatisfactionUnknown {
		t.Errorf("content = %s; a fresh ingest leaves satisfaction unevaluated", state.Content)
	}

	if n := h.count(t, `SELECT count(*) FROM assets`); n != 1 {
		t.Errorf("%d assets, want 1", n)
	}
	if n := h.count(t, `SELECT count(*) FROM blobs`); n != 1 {
		t.Errorf("%d blobs, want 1", n)
	}
}

// Invariant 1, and the load-bearing test of this issue.
//
// Heyarr hashes what ARRIVED. The asset's blob is keyed on the digest Heyarr
// computed, never on anything a download client claimed — so a file whose
// contents differ produces a different blob, and there is no path by which a
// claimed hash becomes an identity.
func TestTheBlobIsKeyedOnTheDigestHeyarrComputed(t *testing.T) {
	h := newIngestHarness(t)
	contents := []byte("bytes that arrived")
	h.selectAndComplete(t, "Arrival.2016.2160p.mkv", contents)

	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}

	var stored string
	if err := h.db.Reader().QueryRow(`SELECT hash FROM blobs`).Scan(&stored); err != nil {
		t.Fatal(err)
	}

	// Computed independently of the handler, from the bytes on disk.
	want := hashOf(t, contents)
	if stored != want {
		t.Fatalf("the blob is %s; the bytes on disk hash to %s — the asset must be "+
			"keyed on what Heyarr computed", stored, want)
	}
}

// A verification that nobody has watched reject anything is decoration. Each
// case is a way a "completed" download is not something we can ingest.
func TestVerificationRefusals(t *testing.T) {
	cases := []struct {
		name string
		// prepare mutates the download directory after the transfer is
		// recorded, so the acquisition row points at something unusable.
		prepare func(t *testing.T, path string)
		want    string
	}{
		{
			// The most common operational failure in this class of software:
			// the client says done, and the path it reported is not one Heyarr
			// can open.
			name:    "the file is not where the client said",
			prepare: func(t *testing.T, path string) { t.Helper(); mustRemove(t, path) },
			want:    "may need mapping",
		},
		{
			// Hashes perfectly well, which is the problem — it would become a
			// legitimate-looking asset that plays nothing.
			name: "the file is empty",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "is empty",
		},
		{
			// A directory is a multi-file release and is now ingestable
			// (ADR-0093) — but an EMPTY one has nothing to ingest, and one with
			// no video files is the same. It fails cleanly with a reason rather
			// than panicking on a directory the walk found nothing in.
			name: "the transfer is a directory with no video files",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				mustRemove(t, path)
				if err := os.MkdirAll(path, 0o750); err != nil {
					t.Fatal(err)
				}
				// A stray .nfo — present, but not a video file to ingest.
				if err := os.WriteFile(filepath.Join(path, "release.nfo"),
					[]byte("scene notes"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "no ingestable video files",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newIngestHarness(t)
			path := h.selectAndComplete(t, "Arrival.2016.2160p.mkv", []byte("something"))
			tc.prepare(t, path)

			// NOT an error: the outcome is recorded, so retrying would re-hash
			// the same bad file and reach the same conclusion more slowly.
			if err := h.ingest(t); err != nil {
				t.Fatalf("a bad artifact is an outcome, not a job failure: %v", err)
			}

			state := h.state(t)
			if state.Phase != acquisition.PhaseIdle {
				t.Errorf("phase = %s; a failed verification must not stick in verifying",
					state.Phase)
			}
			if state.Managed {
				t.Error("nothing was ingested, so nothing is held")
			}
			if n := h.count(t, `SELECT count(*) FROM assets`); n != 0 {
				t.Errorf("%d asset(s) were created from bytes that did not verify", n)
			}

			// And the reason survives, which is what makes it answerable.
			blocked, err := h.cat.BlockedFor(t.Context(), h.want)
			if err != nil {
				t.Fatal(err)
			}
			if len(blocked) != 1 {
				t.Fatalf("%d blocked release(s), want 1", len(blocked))
			}
			if !strings.Contains(blocked[0].Detail, tc.want) {
				t.Errorf("the block should explain %q, said: %s", tc.want, blocked[0].Detail)
			}
			if blocked[0].CandidateID != "good" {
				t.Errorf("blocked %q, expected the release that was selected", blocked[0].CandidateID)
			}
		})
	}
}

// THE loop this issue exists to break.
//
// A release that failed must not be selected again by the next search — and
// the mark has to survive RecordSearch replacing the candidate set, which is
// exactly what happens between the two searches below.
func TestAFailedReleaseIsNotChosenAgain(t *testing.T) {
	h := newIngestHarness(t)
	path := h.selectAndComplete(t, "Arrival.2016.2160p.mkv", []byte("something"))
	mustRemove(t, path)

	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}
	if got := h.state(t).Phase; got != acquisition.PhaseIdle {
		t.Fatalf("setup: phase = %s", got)
	}

	// The indexer still offers the same release — it has no idea it went badly
	// — plus a worse one that the profile accepts.
	h.fake.Offer("Arrival",
		offer("good", 2160, "hevc"),
		offer("second-best", 1080, "h264"))

	if err := h.run(t); err != nil {
		t.Fatal(err)
	}

	sel, err := h.cat.SelectedCandidate(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	if sel.CandidateID == "good" {
		t.Fatal("the search chose the release that just failed to verify — this is " +
			"the infinite-download loop, and the mark did not survive the candidate " +
			"set being replaced")
	}
	if sel.CandidateID != "second-best" {
		t.Errorf("selected %q, expected the next acceptable release", sel.CandidateID)
	}
}

// When everything on offer has already failed, that is a DIFFERENT outcome
// from finding nothing, and an operator needs to be able to tell them apart.
func TestASearchWhereEverythingHasAlreadyFailed(t *testing.T) {
	h := newIngestHarness(t)
	path := h.selectAndComplete(t, "Arrival.2016.2160p.mkv", []byte("x"))
	mustRemove(t, path)
	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}

	// Only the failed release is on offer this time.
	h.fake.Offer("Arrival", offer("good", 2160, "hevc"))
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}

	if got := h.state(t).Phase; got != acquisition.PhaseIdle {
		t.Errorf("phase = %s, want idle", got)
	}
	if _, err := h.cat.SelectedCandidate(t.Context(), h.want); !errors.Is(err, catalog.ErrNoCandidate) {
		t.Errorf("something was selected from a set of releases that have all failed: %v", err)
	}
}

// Invariant 9: the job WILL be re-run, and re-running it must not produce a
// second asset.
func TestIngestingTwiceProducesOneAsset(t *testing.T) {
	h := newIngestHarness(t)
	h.selectAndComplete(t, "Arrival.2016.2160p.mkv", []byte("the actual bytes"))

	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}
	// The second run arrives after the want has left VERIFYING, which is what
	// a re-queued job actually looks like.
	for range 3 {
		if err := h.ingest(t); err != nil {
			t.Fatalf("a repeat ingest must be harmless: %v", err)
		}
	}

	if n := h.count(t, `SELECT count(*) FROM assets`); n != 1 {
		t.Errorf("%d assets after four ingests, want 1", n)
	}
	if n := h.count(t, `SELECT count(*) FROM blobs`); n != 1 {
		t.Errorf("%d blobs after four ingests, want 1", n)
	}
}

// ADR-0014's ladder, asserted by the RUNG reached rather than by the outcome.
// A copy and a reflink both produce a correct file; only one of them is the
// feature, and on a same-filesystem store the cheap rung must be the one taken.
func TestTheMaterialisationRungIsRecorded(t *testing.T) {
	h := newIngestHarness(t)
	h.selectAndComplete(t, "Arrival.2016.2160p.mkv", []byte("bytes worth not copying"))

	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}

	rec, err := h.cat.Acquisition(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	// The detail on the transition records what actually happened, so an
	// operator can see it without reading the CAS.
	if !strings.Contains(rec.Detail, "blake3:") {
		t.Errorf("the ingest detail should name the blob, said: %s", rec.Detail)
	}
	// Assert the rung POSITIVELY — which one was reached — rather than
	// "not a copy".
	//
	// A negative assertion here would pass for a detail string that recorded
	// no rung at all, and would keep passing if the field were dropped. This
	// test exists because a copy and a reflink both produce a correct file and
	// only one of them is the feature, so the thing under test is the rung
	// itself, not the outcome.
	//
	// The harness puts the download directory and the CAS under ONE
	// t.TempDir() by construction, so a cheap rung is reachable by
	// construction and not by luck. If that ever stops being true this test
	// starts failing, which is the correct response: it would mean the fixture
	// no longer exercises what it claims to.
	var rung string
	for _, r := range []string{string(ingest.Reflink), string(ingest.Hardlink), string(ingest.Copy)} {
		if strings.Contains(rec.Detail, r) {
			rung = r
			break
		}
	}
	switch rung {
	case "":
		t.Fatalf("no materialisation rung recorded at all in %q — a detail that does "+
			"not say how the bytes arrived cannot answer whether the ladder worked",
			rec.Detail)
	case string(ingest.Copy):
		t.Errorf("the store and the download directory are on one filesystem, so a "+
			"copy means ADR-0014's ladder did not reach a cheap rung: %s", rec.Detail)
	default:
		t.Logf("materialised by %s", rung)
	}
}

// Nothing the download client still holds is deleted by Heyarr (§60, ADR-0018).
func TestIngestLeavesTheDownloadClientsCopyAlone(t *testing.T) {
	h := newIngestHarness(t)
	path := h.selectAndComplete(t, "Arrival.2016.2160p.mkv", []byte("still seeding"))

	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the download client's copy was removed — it may still be seeding, "+
			"and reaching outside ADR-0018's logical delete for it is worse than "+
			"leaving it: %v", err)
	}
	// The acquisition row survives too. Forgetting the transfer is a separate
	// decision from ingesting it, and conflating them would mean an ingest
	// could orphan bytes the client is still serving.
	if _, err := h.cat.AcquisitionFor(t.Context(), h.want); err != nil {
		t.Errorf("the acquisition row was dropped by the ingest: %v", err)
	}
}

// An ingest that arrives when the want has moved on is the normal case for a
// deduped job on a beat, not an error.
func TestAnIngestForAWantThatMovedOnIsHarmless(t *testing.T) {
	h := newIngestHarness(t)
	h.selectAndComplete(t, "Arrival.2016.2160p.mkv", []byte("bytes"))
	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}

	before := h.count(t, `SELECT count(*) FROM assets`)
	if err := h.ingest(t); err != nil {
		t.Fatalf("a late ingest is not an error: %v", err)
	}
	if got := h.count(t, `SELECT count(*) FROM assets`); got != before {
		t.Errorf("a late ingest created %d asset(s)", got-before)
	}
}

// A content type nothing is configured to hold is named precisely, because
// "no such root" would send an operator looking for a missing directory when
// the actual problem is a library they have not made.
func TestNoLibraryForTheContentType(t *testing.T) {
	h := newIngestHarness(t)
	h.exec(t, `UPDATE libraries SET content_type = 'series' WHERE id = 'lib1'`)
	h.selectAndComplete(t, "Arrival.2016.2160p.mkv", []byte("bytes"))

	if err := h.ingest(t); err != nil {
		t.Fatalf("a missing library is an outcome, not a job failure: %v", err)
	}

	blocked, err := h.cat.BlockedFor(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 1 {
		t.Fatalf("%d blocked, want 1", len(blocked))
	}
	if !strings.Contains(blocked[0].Detail, "create a library") {
		t.Errorf("the reason should name the real problem, said: %s", blocked[0].Detail)
	}
	// Blocked as an INGEST failure rather than a verification one: the bytes
	// were fine, and this is a local configuration problem.
	if blocked[0].Reason != catalog.BlockIngestFailed {
		t.Errorf("reason = %s, want ingest_failed — the release was not at fault",
			blocked[0].Reason)
	}
}

// The load-bearing test of ADR-0080, and the inverse of the one above.
//
// A followed RSS article is typed `document`. Before `document` was a
// registered content type, a `document` library could not be created, so the
// SAME RootForContentType lookup that succeeds for a movie here raised
// ErrNoRootForContent on every article ingest — the diagnosed gap. With a
// `document` library configured, a captured article's bytes must find that root
// and land as a managed asset rather than being blocked.
//
// The want reaches VERIFYING through the ordinary grab flow, exactly as the
// movie tests do — how it got to a completed download on disk is not this
// test's subject. What IS the subject is the routing: a document-typed Work
// resolving to a document library root on ingest.
func TestADocumentAcquisitionLandsInItsLibrary(t *testing.T) {
	h := newIngestHarness(t)

	// The library and the wanted Work are `document`, as a followed feed makes
	// them. RootForContentType keys on the Work's content type, so both must be
	// document for the article's blob to have somewhere to go.
	h.exec(t, `UPDATE libraries SET content_type = 'document' WHERE id = 'lib1'`)
	h.exec(t, `UPDATE works SET content_type = 'document' WHERE id = 'w1'`)

	// A captured article is a self-contained single-file .html (ADR-0063).
	h.selectAndComplete(t, "an-article.html", []byte("<html><body>the captured article</body></html>"))

	if err := h.ingest(t); err != nil {
		t.Fatalf("a document ingest must succeed once a document library exists: %v", err)
	}

	// Not blocked: the whole point is that RootForContentType now finds a root
	// instead of raising ErrNoRootForContent.
	blocked, err := h.cat.BlockedFor(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 0 {
		t.Fatalf("%d blocked, want 0 — a document library exists, so nothing should block: %+v",
			len(blocked), blocked)
	}

	state := h.state(t)
	if !state.Managed {
		t.Error("after an ingest Heyarr holds bytes for the article")
	}
	if got := state.Name(); got != "AVAILABLE" {
		t.Fatalf("state = %s, want AVAILABLE", got)
	}
	if n := h.count(t, `SELECT count(*) FROM assets`); n != 1 {
		t.Errorf("%d assets, want 1", n)
	}
	// The asset attached to a document Work, not a phantom of another type.
	if ct := h.scalar(t, `SELECT w.content_type FROM works w
		JOIN editions e ON e.work_id = w.id
		JOIN assets a ON a.edition_id = e.id LIMIT 1`); ct != "document" {
		t.Errorf("the asset's work is %q, want document", ct)
	}
	// The captured HTML carries a media type, so OPDS can advertise it (ADR-0080).
	if mime := h.scalar(t, `SELECT COALESCE(mime, '') FROM assets LIMIT 1`); mime != "text/html" {
		t.Errorf("asset mime = %q, want text/html", mime)
	}
}

// Season-pack ingest (ADR-0093, the slice after the containment gate).
//
// A pack is one download that holds a whole season. Ingesting it must produce
// one asset per episode file and map each to the episode item its name parses
// to, so a single grab satisfies every episode the pack contains — not only the
// want that triggered it.

// setupSeries turns the harness's movie fixture into a series with `episodes`
// enumerated items (S01E01…), and rewrites the harness want into the item-scoped
// S01E01 want that triggers the grab. The other episodes get their own
// item-scoped wants, each started, so a reconcile can find them. It returns the
// item_key → want-id map.
func (h *ingestHarness) setupSeries(t *testing.T, episodes int) map[string]string {
	t.Helper()
	ctx := t.Context()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)

	// The library and the wanted Work become series; the title stays "Arrival"
	// so the fake indexer's offer still matches the search that reaches SELECTED.
	h.exec(t, `UPDATE libraries SET content_type = 'series' WHERE id = 'lib1'`)
	h.exec(t, `UPDATE works SET content_type = 'series' WHERE id = 'w1'`)
	h.exec(t, `INSERT INTO editions (id, work_id, label, edition_type, language, attributes, created_at)
		VALUES ('e-s01', 'w1', 'Season 01', 'web-dl', 'en', '{}', ?)`, stamp)

	wants := make(map[string]string, episodes)
	for e := 1; e <= episodes; e++ {
		key := fmt.Sprintf("S01E%02d", e)
		itemID := "it-" + strings.ToLower(key)
		h.exec(t, `INSERT INTO items (id, work_id, edition_id, item_key, title, attributes, created_at, updated_at)
			VALUES (?, 'w1', 'e-s01', ?, ?, '{}', ?, ?)`, itemID, key, key, stamp, stamp)

		if e == 1 {
			// Reuse the harness want (already started) as the triggering want.
			h.exec(t, `UPDATE desired_items
				SET scope = 'item', item_id = ?, edition_id = NULL, aspect = 'primary', language = ''
				WHERE id = ?`, itemID, h.want)
			wants[key] = h.want
			continue
		}
		wantID := uuid.Must(uuid.NewV7()).String()
		h.exec(t, `INSERT INTO desired_items
			(id, scope, work_id, edition_id, item_id, aspect, language,
			 quality_profile_id, monitor, reason, created_at, updated_at)
			VALUES (?, 'item', 'w1', NULL, ?, 'primary', '', 'q1', 1, '', ?, ?)`,
			wantID, itemID, stamp, stamp)
		if _, err := h.cat.StartAcquisition(ctx, wantID); err != nil {
			t.Fatal(err)
		}
		wants[key] = wantID
	}
	return wants
}

// selectAndCompletePack drives the triggering want to VERIFYING with a
// multi-file release (a directory) on disk, which is the state an ingest job
// finds for a season pack. `files` are release-relative paths → contents.
func (h *ingestHarness) selectAndCompletePack(t *testing.T, dirName string, files map[string][]byte) string {
	t.Helper()
	ctx := t.Context()

	h.fake.Offer("Arrival", offer("S01E01", 2160, "hevc"))
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}
	if got := h.state(t).Name(); got != "SELECTED" {
		t.Fatalf("setup: want is %s, expected SELECTED", got)
	}

	dir := filepath.Join(h.downloads, dirName)
	var total int64
	for name, contents := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		total += int64(len(contents))
	}

	if _, err := h.cat.RecordAcquisition(ctx, catalog.Acquisition{
		ID: NewAcquisitionID(), DesiredItemID: h.want,
		Provider: "fake-downloader", ExternalID: "infohash-pack",
		ExternalName: dirName, RemotePath: dir, LocalPath: dir,
		BytesTotal: total, BytesDone: total,
	}); err != nil {
		t.Fatal(err)
	}
	for _, tr := range []acquisition.Transition{
		acquisition.TransitionQueue,
		acquisition.TransitionStartDownload,
		acquisition.TransitionDownloaded,
	} {
		if _, err := h.cat.AdvanceAcquisition(ctx, h.want, tr, ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.state(t).Phase; got != acquisition.PhaseVerifying {
		t.Fatalf("setup: phase is %s, expected verifying", got)
	}
	return dir
}

// linkedItemKey returns the item_key an asset is linked to, or "" for an
// unlinked asset, keyed by the asset's source filename base.
func (h *ingestHarness) assetItemKeys(t *testing.T) map[string]string {
	t.Helper()
	rows, err := h.db.Reader().Query(`
		SELECT a.source_path, COALESCE(i.item_key, '')
		FROM assets a LEFT JOIN items i ON i.id = a.item_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var src, key string
		if err := rows.Scan(&src, &key); err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(src)] = key
	}
	return out
}

// THE season-pack test: six episode files, six assets, each mapped to its item.
func TestASeasonPackIngestsEveryEpisodeAndLinksEachToItsItem(t *testing.T) {
	h := newIngestHarness(t)
	h.setupSeries(t, 6)

	files := map[string][]byte{}
	for e := 1; e <= 6; e++ {
		name := fmt.Sprintf("Arrival.S01E%02d.1080p.WEB-DL.DDP5.1.H.264-NTb.mkv", e)
		files[name] = []byte(fmt.Sprintf("episode %d distinct bytes", e))
	}
	h.selectAndCompletePack(t, "Arrival.S01.1080p.WEB-DL.DDP5.1.H.264-NTb", files)

	if err := h.ingest(t); err != nil {
		t.Fatalf("a season pack must ingest, not be refused as multi-file: %v", err)
	}

	// One asset and one blob per episode file — the pack is not hashed whole.
	if n := h.count(t, `SELECT count(*) FROM assets`); n != 6 {
		t.Errorf("%d assets, want 6 — one per episode file", n)
	}
	if n := h.count(t, `SELECT count(*) FROM blobs`); n != 6 {
		t.Errorf("%d blobs, want 6", n)
	}

	// Each episode item has exactly one asset linked to it, by item_key.
	for e := 1; e <= 6; e++ {
		key := fmt.Sprintf("S01E%02d", e)
		n := h.count(t,
			`SELECT count(*) FROM assets a JOIN items i ON i.id = a.item_id WHERE i.item_key = ?`, key)
		if n != 1 {
			t.Errorf("item %s has %d assets linked, want 1", key, n)
		}
	}

	// The triggering want holds bytes.
	if !h.state(t).Managed {
		t.Error("the triggering want holds no bytes after its pack ingested")
	}
}

// The fan-out: a pack triggered by ONE want satisfies every episode want. Each
// sibling want, after a reconcile, finds the asset the single download produced
// — one transfer, one blob per file, many wants served.
func TestASeasonPackSatisfiesEverySiblingEpisodeWant(t *testing.T) {
	h := newIngestHarness(t)
	wants := h.setupSeries(t, 6)

	files := map[string][]byte{}
	for e := 1; e <= 6; e++ {
		files[fmt.Sprintf("Arrival.S01E%02d.1080p.WEB-DL.mkv", e)] = []byte(fmt.Sprintf("episode %d distinct bytes", e))
	}
	h.selectAndCompletePack(t, "Arrival.S01.1080p.WEB-DL", files)
	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}

	// Only the triggering want went through the handler; the siblings are
	// served by reconciliation over the shared assets (§3.2, ADR-0093).
	for key, wantID := range wants {
		res, err := h.cat.ReconcileDesired(t.Context(), wantID)
		if err != nil {
			t.Fatalf("reconciling %s: %v", key, err)
		}
		if !res.State.Managed {
			t.Errorf("episode want %s holds no bytes after the pack ingested — "+
				"the single download did not fan out to it", key)
		}
	}
}

// A file the pack parser cannot place — no season/episode in the name — is
// ingested but left UNLINKED, never guessed onto an episode.
func TestAnUnparseablePackFileIsLeftUnlinked(t *testing.T) {
	h := newIngestHarness(t)
	h.setupSeries(t, 2)

	h.selectAndCompletePack(t, "Arrival.S01.WEB-DL", map[string][]byte{
		"Arrival.S01E01.1080p.WEB-DL.mkv": []byte("the pilot bytes"),
		"bonus-featurette.mkv":            []byte("something unparseable"),
	})
	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}

	keys := h.assetItemKeys(t)
	if got := keys["Arrival.S01E01.1080p.WEB-DL.mkv"]; got != "S01E01" {
		t.Errorf("the pilot linked to %q, want S01E01", got)
	}
	if got := keys["bonus-featurette.mkv"]; got != "" {
		t.Errorf("the unplaceable file linked to %q — it must be left unlinked, "+
			"not guessed onto an episode", got)
	}
}

// A file for a season the work has no item for is ingested but left unlinked —
// the same safe direction, so an S02 file never lands on an S01 episode.
func TestAWrongSeasonPackFileIsLeftUnlinked(t *testing.T) {
	h := newIngestHarness(t)
	h.setupSeries(t, 2) // only S01E01, S01E02 exist as items

	h.selectAndCompletePack(t, "Arrival.S01.WEB-DL", map[string][]byte{
		"Arrival.S01E01.1080p.WEB-DL.mkv": []byte("the pilot bytes"),
		"Arrival.S02E01.1080p.WEB-DL.mkv": []byte("a stray next-season file"),
	})
	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}

	keys := h.assetItemKeys(t)
	if got := keys["Arrival.S01E01.1080p.WEB-DL.mkv"]; got != "S01E01" {
		t.Errorf("the pilot linked to %q, want S01E01", got)
	}
	if got := keys["Arrival.S02E01.1080p.WEB-DL.mkv"]; got != "" {
		t.Errorf("the S02 file linked to %q — a wrong-season file must be left unlinked", got)
	}
	// And both files still became assets: an unplaceable file is ingested, not dropped.
	if n := h.count(t, `SELECT count(*) FROM assets`); n != 2 {
		t.Errorf("%d assets, want 2 — an unlinked file is still ingested", n)
	}
}

// A nested season folder, where only the directory names the season
// ("Show S01/Season 01/E01 - Pilot.mkv"), still maps each file to its item.
func TestASeasonPackWithNestedSeasonFolders(t *testing.T) {
	h := newIngestHarness(t)
	h.setupSeries(t, 2)

	h.selectAndCompletePack(t, "Arrival S01", map[string][]byte{
		"Season 01/E01 - Pilot.mkv":    []byte("pilot bytes"),
		"Season 01/E02 - Handover.mkv": []byte("second bytes"),
	})
	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}

	keys := h.assetItemKeys(t)
	if got := keys["E01 - Pilot.mkv"]; got != "S01E01" {
		t.Errorf("the pilot linked to %q, want S01E01 (season from the folder)", got)
	}
	if got := keys["E02 - Handover.mkv"]; got != "S01E02" {
		t.Errorf("episode 2 linked to %q, want S01E02", got)
	}
}

// A sample clip beside the episodes is not ingested as content.
func TestASeasonPackIgnoresSamples(t *testing.T) {
	h := newIngestHarness(t)
	h.setupSeries(t, 1)

	h.selectAndCompletePack(t, "Arrival.S01.WEB-DL", map[string][]byte{
		"Arrival.S01E01.1080p.WEB-DL.mkv":        []byte("the real pilot"),
		"Arrival.S01E01.1080p.WEB-DL.sample.mkv": []byte("a 30-second sample"),
		"Sample/clip.mkv":                        []byte("a sample in its own directory"),
		"release.nfo":                            []byte("scene notes"),
	})
	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}

	if n := h.count(t, `SELECT count(*) FROM assets`); n != 1 {
		t.Errorf("%d assets, want 1 — only the real episode, not the samples or the .nfo", n)
	}
}

// A single-file series episode is unchanged: its one asset links to the item the
// WANT asserts (ADR-0086), via the download's item, not by parsing the filename.
func TestASingleEpisodeStillLinksViaTheWantsItem(t *testing.T) {
	h := newIngestHarness(t)
	h.setupSeries(t, 2)

	// A lone file, not a directory — the single-file path. The series want is
	// item-scoped S01E01, so the offer must name that episode to clear the gate.
	h.selectAndComplete(t, "Arrival.S01E01.1080p.WEB-DL.mkv", []byte("the pilot bytes"), "S01E01")
	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}

	if n := h.count(t, `SELECT count(*) FROM assets`); n != 1 {
		t.Fatalf("%d assets, want 1", n)
	}
	if got := h.assetItemKeys(t)["Arrival.S01E01.1080p.WEB-DL.mkv"]; got != "S01E01" {
		t.Errorf("the single episode linked to %q, want S01E01", got)
	}
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

// hashOf computes the digest independently of the handler, so the assertion
// that the blob is keyed on what Heyarr computed is checked against an
// independent answer rather than against the handler's own.
func hashOf(t *testing.T, b []byte) string {
	t.Helper()
	h, _, err := hashing.HashReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return h.String()
}

// scalar reads one string out of the database, for asserting on a column.
func (h *ingestHarness) scalar(t *testing.T, query string, args ...any) string {
	t.Helper()
	var out string
	if err := h.db.Reader().QueryRow(query, args...).Scan(&out); err != nil {
		t.Fatalf("querying (%s): %v", query, err)
	}
	return out
}

// THE assertion #224 exists for.
//
// The want is for "Arrival" (work_key `movie:arrival:2016`). The file that
// arrives is named nothing like it — which is the ORDINARY case, not a corner:
// release titles carry extensions, scene tags and the indexer's own
// normalisation, so a filename that happens to parse back to the same work key
// is the lucky outcome.
//
// Before this, the pipeline re-identified from the filename and attached the
// asset to a second, path-derived Work. The bytes were hashed, stored,
// verified and catalogued — and the want that asked for them reported
// `assets: []` forever, in a state indistinguishable from patience.
func TestAnAcquisitionAttachesToTheWantsWorkWhenTheFilenameDisagrees(t *testing.T) {
	h := newIngestHarness(t)
	h.selectAndComplete(t, "alpine-standard-3.23.2-armv7.iso", []byte("the bytes that arrived"))

	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}

	if n := h.count(t, `SELECT count(*) FROM assets`); n != 1 {
		t.Fatalf("%d assets, want 1", n)
	}
	// The want's own Work, by id. Asserting a COUNT of works would pass for an
	// asset attached to the wrong one of two, and asserting the work key would
	// pass for a second row that happened to be keyed the same way.
	gotWork := h.scalar(t, `SELECT e.work_id FROM assets a JOIN editions e ON e.id = a.edition_id`)
	wantWork := h.scalar(t, `SELECT work_id FROM desired_items WHERE id = ?`, h.want)
	if gotWork != wantWork {
		t.Errorf("the asset is on work %s; the want points at %s — "+
			"the bytes arrived and the want will report assets: [] forever",
			gotWork, wantWork)
	}
}

// And no second Work is created, which is the other half of the same defect.
//
// Separate from the assertion above because they fail differently: an asset
// could be attached correctly while a stray Work was still created beside it,
// and a library that grows a phantom Work per acquisition is its own problem
// (#227 is that problem arriving from the scanner).
func TestAnAcquisitionDoesNotCreateASecondWork(t *testing.T) {
	h := newIngestHarness(t)
	before := h.count(t, `SELECT count(*) FROM works`)

	h.selectAndComplete(t, "alpine-standard-3.23.2-armv7.iso", []byte("the bytes that arrived"))
	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}

	if after := h.count(t, `SELECT count(*) FROM works`); after != before {
		t.Errorf("works went from %d to %d — an acquisition invented a Work "+
			"for content somebody had already named", before, after)
	}
}

// The asset says a WANT identified it, not a path heuristic.
//
// identification_source exists to answer "why is this asset on this Work". An
// acquisition answering "path-heuristic" would name a heuristic that had no
// part in the decision — and would be indistinguishable from the guess this
// issue is about, in the one column an operator would check.
func TestAnAcquiredAssetRecordsThatTheWantIdentifiedIt(t *testing.T) {
	h := newIngestHarness(t)
	h.selectAndComplete(t, "alpine-standard-3.23.2-armv7.iso", []byte("the bytes that arrived"))
	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}

	if got := h.scalar(t, `SELECT identification_source FROM assets`); got != "desired-item" {
		t.Errorf("identification_source = %q, want desired-item", got)
	}
}

// The override replaces the WORK and nothing else.
//
// The risk in fixing #224 is over-reaching: a want names what was wanted, not
// which of its editions arrived, and letting it dictate the asset's role would
// file a subtitle as if it were the film. The filename here is a subtitle, and
// the want is for a movie.
func TestTheWantNamesTheWorkAndThePathStillNamesTheFile(t *testing.T) {
	h := newIngestHarness(t)
	h.selectAndComplete(t, "Arrival.2016.2160p.en.srt", []byte("1\n00:00:01,000 --> 00:00:02,000\nhello\n"))
	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}

	// Still on the want's Work — the override did its job.
	gotWork := h.scalar(t, `SELECT e.work_id FROM assets a JOIN editions e ON e.id = a.edition_id`)
	wantWork := h.scalar(t, `SELECT work_id FROM desired_items WHERE id = ?`, h.want)
	if gotWork != wantWork {
		t.Fatalf("the asset is on work %s, want %s", gotWork, wantWork)
	}
	// And the file's own facts still come from the path.
	if got := h.scalar(t, `SELECT role FROM assets`); got != "subtitle" {
		t.Errorf("role = %q, want subtitle — the want named the Work, not the file", got)
	}
}
