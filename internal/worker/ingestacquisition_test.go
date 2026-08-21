package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
func (h *ingestHarness) selectAndComplete(t *testing.T, filename string, contents []byte) string {
	t.Helper()
	ctx := t.Context()

	h.fake.Offer("Arrival", offer("good", 2160, "hevc"))
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
			// A multi-file release. Ingesting it as one artifact would produce
			// a plausible blob of the wrong thing.
			name: "the transfer is a directory",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				mustRemove(t, path)
				if err := os.MkdirAll(path, 0o750); err != nil {
					t.Fatal(err)
				}
			},
			want: "multi-file release",
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
