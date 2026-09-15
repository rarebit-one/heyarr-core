package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// Ingest of completed acquisitions (§65, §66, M3-13).
//
// # This is ONE SOURCE, not a second pipeline
//
// §65 lists many ways bytes arrive — a scanner, an upload, a rip, another
// peer, and a completed acquisition. §61 warns against content-specific job
// systems, and the same warning applies to source-specific ingest: what
// differs between a scanned file and a downloaded one is how it got there and
// what desired state it answers, and nothing else.
//
// So this handler does exactly three things the scanner's path does not, and
// then hands the bytes to the SAME pipeline:
//
//  1. It hashes what arrived, itself (invariant 1).
//  2. It records the answer against a want, driving §64's edges.
//  3. It marks the release when the bytes were bad, so the next search does
//     not choose it again.
//
// Everything after that — materialisation down ADR-0014's ladder, blob
// identity, Work/Edition resolution, the Asset, the replica, the events — is
// ingest.Pipeline, unchanged and unaware that this artifact came from a
// download client rather than from a walk.
//
// # Verification is not a formality
//
// A download client reports completion. That is a claim by a third party about
// bytes it fetched from strangers, and invariant 1 says a destination always
// verifies bytes itself and never trusts a claimed hash. §64 gives VERIFYING
// its own state because it is real work on real bytes.
//
// What is verified here is that the file EXISTS, is readable, is not empty,
// and hashes to something stable — because at this point in the milestone
// there is frequently nothing to compare against. Torrent infohashes are not
// BLAKE3 digests, and a release rarely publishes one. So the check that can be
// made is made, the digest is computed by us, and the digest we computed is
// what the asset is keyed on. A claimed hash, when one is ever available, is
// compared against ours and never substituted for it.

// acquisitionRoot is how an ingesting acquisition finds a library to land in.
//
// A completed download is not under a library root — it is wherever the
// download client put it — but ingest.Pipeline is root-oriented, because a
// root carries the content type identification needs and the materialisation
// mode ADR-0014 climbs. So a root has to be chosen, and the honest choice is
// the library whose content type matches the Work being wanted.
type acquisitionRoot interface {
	RootForContentType(ctx context.Context, contentType string) (ingest.Root, error)
}

// IngestAcquisitionHandler brings one completed acquisition under management.
//
// # Failure is a modelled edge, not a returned error
//
// Bad bytes, a missing file, an unreadable one — none of these are job
// failures. They are outcomes: the want returns to rest, the release is
// blocked so it is not chosen again, and the pipeline says why. Returning an
// error would put the job into a retry backoff and re-hash the same bad file
// five times before anybody saw the reason.
//
// What IS returned as an error is a failure to record the outcome — a database
// that will not accept the write — because that is a real failure and retrying
// it is the right response.
func IngestAcquisitionHandler(
	cat *catalog.Catalog, roots acquisitionRoot, pipeline *ingest.Pipeline,
	probes ProbeEnqueuer, log *slog.Logger,
) HandlerFunc {
	return func(ctx context.Context, job jobs.Job) error {
		var payload acquisition.IngestPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("worker: ingest_acquisition payload is not decodable: %w", err)
		}
		if payload.DesiredItemID == "" {
			return errors.New("worker: ingest_acquisition needs a desired item")
		}

		state, err := cat.Acquisition(ctx, payload.DesiredItemID)
		if err != nil {
			return err
		}
		// Only from VERIFYING. The job is deduped per want and the poll beat
		// re-enqueues, so arriving late — after another pass already ingested
		// — is the normal case rather than an error.
		if state.State.Phase != acquisition.PhaseVerifying {
			// LATE and NEVER are different situations and used to share a log
			// line (#240). "Arrived after the want moved on" describes a second
			// delivery of a transfer already handled; an operator reading it
			// about a want that never got there goes looking for an earlier
			// delivery that does not exist.
			//
			// Managed is what tells them apart: a want holding bytes has been
			// through this, and one holding none has not. Both are quiet
			// successes — invariant 9 makes a duplicate ordinary, and the
			// endpoint now advances a never-started want itself — so what is
			// at stake is only whether the sentence is true.
			if state.State.Managed {
				log.Info("an acquisition ingest arrived after the want moved on",
					"desired_item_id", payload.DesiredItemID,
					"phase", string(state.State.Phase))
			} else {
				log.Warn("an acquisition ingest arrived for a want that never reached verifying",
					"desired_item_id", payload.DesiredItemID,
					"phase", string(state.State.Phase))
			}
			return nil
		}

		acq, err := cat.AcquisitionFor(ctx, payload.DesiredItemID)
		if err != nil {
			return err
		}

		batch, verifyErr := verifyArtifacts(acq)
		if verifyErr != nil {
			return failAcquisition(ctx, cat, payload.DesiredItemID, acq,
				catalog.BlockVerificationFailed, verifyErr, log)
		}

		if _, err := cat.AdvanceAcquisition(ctx, payload.DesiredItemID,
			acquisition.TransitionVerified, batch.verifiedDetail()); err != nil {
			return err
		}

		results, linked, ingestErr := ingestArtifacts(ctx, cat, roots, pipeline, payload.DesiredItemID, batch)
		if ingestErr != nil {
			// A local problem, most likely, rather than a bad release — which
			// is why it is blocked under a DIFFERENT reason. See
			// catalog.BlockIngestFailed.
			return failAcquisition(ctx, cat, payload.DesiredItemID, acq,
				catalog.BlockIngestFailed, ingestErr, log)
		}

		if _, err := cat.AdvanceAcquisition(ctx, payload.DesiredItemID,
			acquisition.TransitionIngested, ingestedDetail(batch, results, linked)); err != nil {
			return err
		}

		// §66 puts probe in the pipeline, and it is enqueued rather than run
		// for the same reason the scanner's path enqueues it: a probe is a job
		// and may need a capability this worker does not have (ADR-0023). A pack
		// produced one result per file, each probed on its own blob.
		if probes != nil {
			for i, res := range results {
				if res.AssetID == "" {
					continue
				}
				enqueueProbe(ctx, probes, res, batch.artifacts[i].RelPath)
			}
		}

		// Satisfaction is NOT set here, deliberately.
		//
		// Ingest produces bytes; whether they satisfy the quality profile is
		// §56's question and reconciliation answers it (M3-05). Assuming yes
		// here is exactly how AVAILABLE and CONTENT_SATISFIED collapse into
		// each other, which ADR-0027 exists to prevent — and it would be a
		// particularly bad place to do it, because the bytes that just arrived
		// are the ones nobody has evaluated yet.
		if batch.pack {
			log.Info("a season pack was ingested",
				"desired_item_id", payload.DesiredItemID,
				"files", len(results), "linked_to_episodes", linked)
		} else {
			res := results[0]
			log.Info("an acquisition was ingested",
				"desired_item_id", payload.DesiredItemID,
				"asset_id", res.AssetID, "blob", res.BlobHash,
				"materialised", string(res.Materialised),
				"deduplicated", res.Deduplicated)
		}
		return nil
	}
}

// artifact is a verified file, ready for the pipeline.
//
// It carries NO hash, and that is a correction rather than an omission.
//
// The first version of this file hashed the file here, on the reasoning that
// invariant 1 says a destination verifies bytes itself. It does — but the CAS
// already does exactly that: cas.FS.Link hashes the source and returns the
// digest, and the blob is keyed on that answer. Nothing anywhere accepts a
// claimed hash.
//
// So hashing here computed a digest that nothing used, at the cost of reading
// a possibly 40 GB file twice. A sabotage found it: replacing this hash with a
// constant changed no test, because no test could depend on a value the
// system does not consult.
//
// What VERIFYING does, then, is establish that the artifact is INGESTABLE —
// present, readable, non-empty, a single file — and the digest is computed
// once, by the CAS, on the way in. Invariant 1 is upheld where the bytes are
// actually taken under management, which is the only place it can be upheld.
type artifact struct {
	Path    string
	RelPath string
	Size    int64
}

// verifiedTransfer is what a completed download resolved to: one file, or the
// video files of a multi-file release. `pack` distinguishes the two so the
// ingest and the log say which happened, and `path` is the top-level thing the
// client reported — the file, or the release directory.
type verifiedTransfer struct {
	artifacts []artifact
	pack      bool
	path      string
}

func (v verifiedTransfer) totalSize() int64 {
	var n int64
	for _, a := range v.artifacts {
		n += a.Size
	}
	return n
}

// verifiedDetail is the transition note recorded on VERIFYING. The single-file
// wording is unchanged; a pack says how many files it carried.
func (v verifiedTransfer) verifiedDetail() string {
	if v.pack {
		return fmt.Sprintf("%d bytes across %d files at %s",
			v.totalSize(), len(v.artifacts), filepath.Base(v.path))
	}
	a := v.artifacts[0]
	return fmt.Sprintf("%d bytes at %s", a.Size, filepath.Base(a.Path))
}

// ingestedDetail is the transition note recorded on INGESTED. The single-file
// wording is unchanged; a pack summarises the assets it produced and how many
// were linked to an episode item.
func ingestedDetail(v verifiedTransfer, results []ingest.Result, linked int) string {
	if v.pack {
		return fmt.Sprintf("%d assets from a season pack, %d linked to episodes",
			len(results), linked)
	}
	res := results[0]
	return fmt.Sprintf("asset %s, blob %s, %s", res.AssetID, res.BlobHash, res.Materialised)
}

// resolveTransferPath is where the completed transfer's bytes are.
//
// #102 resolves the client's path into one Heyarr can open and stores it as
// LocalPath; RemotePath carries the same value today. Preferring LocalPath
// keeps the distinction meaningful when the two diverge.
func resolveTransferPath(acq catalog.Acquisition) string {
	if p := strings.TrimSpace(acq.LocalPath); p != "" {
		return p
	}
	return strings.TrimSpace(acq.RemotePath)
}

// verifyArtifacts resolves the completed transfer to its ingestable files
// (invariant 1 — the bytes are hashed by the CAS on the way in, never trusted
// from a claim).
//
// A single file is the overwhelmingly common case and is unchanged: it yields
// one artifact. A directory is a multi-file release — a season pack, most often
// — and is NO LONGER refused (ADR-0093): its video files each become their own
// artifact, so one download can satisfy a whole season. Ingesting a directory
// as a single artifact would still be wrong (a plausible blob of the wrong
// thing), so the directory is enumerated rather than hashed whole.
func verifyArtifacts(acq catalog.Acquisition) (verifiedTransfer, error) {
	path := resolveTransferPath(acq)
	if path == "" {
		return verifiedTransfer{}, errors.New("the download client did not say where the bytes are")
	}

	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// The single most common operational failure in this class of
		// software, and it is worth naming precisely rather than as "no such
		// file": the transfer completed, the client says it is done, and the
		// path it reported is not one Heyarr can open. That is a path mapping
		// problem, not a missing file.
		return verifiedTransfer{}, fmt.Errorf(
			"the completed transfer is not at %s — the download client's path may "+
				"need mapping into one Heyarr can open", path)
	case err != nil:
		return verifiedTransfer{}, fmt.Errorf("could not examine %s: %w", path, err)
	}

	if info.IsDir() {
		arts, derr := verifyDirectory(path)
		if derr != nil {
			return verifiedTransfer{}, derr
		}
		return verifiedTransfer{artifacts: arts, pack: true, path: path}, nil
	}

	a, ferr := verifyFile(path, filepath.Base(path), info)
	if ferr != nil {
		return verifiedTransfer{}, ferr
	}
	return verifiedTransfer{artifacts: []artifact{a}, path: path}, nil
}

// verifyFile checks that one file is ingestable — present, non-empty, readable
// — and returns it as an artifact. It is the single-file check, factored so a
// pack's files are held to the same bar as a lone download.
func verifyFile(path, relPath string, info fs.FileInfo) (artifact, error) {
	if info.Size() == 0 {
		// An empty file hashes perfectly well, which is the problem: it would
		// become a legitimate-looking asset that plays nothing.
		return artifact{}, fmt.Errorf("%s is empty", path)
	}

	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return artifact{}, fmt.Errorf("could not read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	// Readability is proven by opening it, not assumed from a stat. A file
	// that stats fine and cannot be read is a permission problem, and finding
	// that out here rather than three steps into the pipeline is the
	// difference between a clear refusal and a confusing one.
	if _, err := f.Read(make([]byte, 1)); err != nil && !errors.Is(err, io.EOF) {
		return artifact{}, fmt.Errorf("could not read %s: %w", path, err)
	}

	return artifact{Path: path, RelPath: relPath, Size: info.Size()}, nil
}

// verifyDirectory enumerates the ingestable video files inside a multi-file
// release (ADR-0093, a season pack).
//
// Only video files are taken. Subtitles arrive through the extract path
// (ADR-0084) and are not double-handled here; samples, artwork, .nfo metadata
// and the contents of extras/sample directories are not content. Each surviving
// file is held to verifyFile's bar; a single unreadable or empty file is skipped
// rather than failing the whole release, since the other episodes are still
// worth having. A file's RelPath includes the release directory so that
// identification can read a season that only a nested folder carries
// ("Show S01/Show.S01E01.mkv").
//
// An empty directory, or one with no video files, is a clean failure — a
// verification refusal, blocked with a reason — never a panic.
func verifyDirectory(dir string) ([]artifact, error) {
	parent := filepath.Dir(dir)
	var arts []artifact

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if p != dir && isNonContentDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if !identification.IsVideoExt(filepath.Ext(d.Name())) || isSampleName(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(parent, p)
		if err != nil {
			rel = d.Name()
		}
		a, ferr := verifyFile(p, filepath.ToSlash(rel), info)
		if ferr != nil {
			// Skip the bad file, keep the pack. If none survive, the empty-pack
			// error below is what the caller sees.
			return nil
		}
		arts = append(arts, a)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("could not read the release directory %s: %w", dir, err)
	}
	if len(arts) == 0 {
		return nil, fmt.Errorf(
			"%s is a directory with no ingestable video files — a multi-file "+
				"release must carry at least one video file to ingest", dir)
	}
	return arts, nil
}

// nonContentDirs hold bonus material or samples, never an episode. A directory
// with one of these names is walked past without descending into it.
var nonContentDirs = map[string]bool{
	"sample": true, "samples": true, "extras": true, "extra": true,
	"featurettes": true, "featurette": true, "bonus": true, "trailer": true,
	"trailers": true, "behind the scenes": true, "deleted scenes": true,
	"proof": true, "screens": true,
}

func isNonContentDir(name string) bool {
	return nonContentDirs[strings.ToLower(strings.TrimSpace(name))]
}

// isSampleName recognises a sample clip a scene release ships beside the real
// files — "sample.mkv", "Show.S01E01.sample.mkv", "show-sample.mkv". Ingesting a
// 30-second sample as an episode is worse than the vanishingly rare chance a
// show's own name contains the word.
func isSampleName(name string) bool {
	lower := strings.ToLower(name)
	stem := strings.TrimSuffix(lower, filepath.Ext(lower))
	switch {
	case stem == "sample":
		return true
	case strings.HasPrefix(stem, "sample."), strings.HasPrefix(stem, "sample-"),
		strings.HasPrefix(stem, "sample "):
		return true
	case strings.Contains(stem, ".sample."), strings.Contains(stem, "-sample."),
		strings.HasSuffix(stem, ".sample"), strings.HasSuffix(stem, "-sample"):
		return true
	}
	return false
}

// ingestArtifacts hands the verified bytes to the M1 pipeline and links each
// produced asset to the Item it belongs to.
//
// # The Work the WANT points at is authoritative
//
// Before #224 this handler passed only the root and the path, so the pipeline
// re-identified from the filename — and attached the asset to whatever Work that
// produced. When the two disagreed (routinely: a release title is not a
// filename) the bytes landed on a second, path-derived Work and the want
// reported `assets: []` indefinitely. The want is passed as a WorkOverride so
// every file of a pack lands on the wanted series, not on a filename's guess.
//
// # Item linking: the want for a lone file, the FILE for a pack
//
// A single-file grab links the one asset to the one Item the want asserts
// (ADR-0086) — the item is a fact the want carries, not one the file reveals.
//
// A season pack is the fan-out ADR-0093 sequences after multi-file ingest: the
// grab was triggered by ONE episode want, but the pack holds many. Each file is
// its own asset and is mapped to the episode item its NAME parses to, resolved
// against the work's enumerated items by item_key. So one download satisfies
// every episode the pack contains, not only the triggering want. The item is
// still an asserted fact — the enumerated item_key — matched against the file's
// derived season/episode, never identity re-derived from a filename. A file that
// parses to no season/episode, or to an episode the work has no item for (a
// wrong-season file, an episode with no want yet), is left UNLINKED (item_id
// null) rather than attached to the wrong episode.
func ingestArtifacts(
	ctx context.Context, cat *catalog.Catalog, roots acquisitionRoot,
	pipeline *ingest.Pipeline, desiredItemID string, batch verifiedTransfer,
) (results []ingest.Result, linked int, err error) {
	sc, err := cat.SearchContextFor(ctx, desiredItemID)
	if err != nil {
		return nil, 0, err
	}
	root, err := roots.RootForContentType(ctx, sc.ContentType)
	if err != nil {
		return nil, 0, err
	}
	dw, err := cat.WorkForDesired(ctx, desiredItemID)
	if err != nil {
		return nil, 0, err
	}
	override := &ingest.WorkOverride{
		ContentType: dw.ContentType,
		WorkKey:     dw.WorkKey,
		Title:       dw.Title,
		SortTitle:   dw.SortTitle,
		Year:        dw.Year,
	}

	if !batch.pack {
		a := batch.artifacts[0]
		res, ierr := pipeline.Ingest(ctx, ingest.Request{
			RootID: root.ID, SourcePath: a.Path, RelPath: a.RelPath, Work: override,
		})
		if ierr != nil {
			return nil, 0, ierr
		}
		// Link to the Item the want points at (ADR-0086). Only item-scoped wants
		// carry one; a work- or edition-scoped grab leaves it unset.
		if res.AssetID != "" {
			_, itemID, _, _, terr := cat.DesiredItemTarget(ctx, desiredItemID)
			if terr != nil {
				return nil, 0, terr
			}
			if itemID != "" {
				if err := cat.SetAssetItem(ctx, res.AssetID, itemID); err != nil {
					return nil, 0, fmt.Errorf("linking the ingested asset to its item: %w", err)
				}
				linked++
			}
		}
		return []ingest.Result{res}, linked, nil
	}

	// A season pack: the work's enumerated items, keyed by their source-stable
	// item_key ("S01E01"), so each file can be mapped to the episode it is.
	items, err := cat.ItemsForWork(ctx, dw.ID)
	if err != nil {
		return nil, 0, err
	}
	itemByKey := make(map[string]string, len(items))
	for _, it := range items {
		itemByKey[strings.ToUpper(it.ItemKey)] = it.ID
	}

	// One registry for the whole pack — Identify would otherwise rebuild the
	// default rule set per file. This is the SAME parser the scanner uses, the
	// one-vocabulary property ADR-0093 relies on.
	reg := identification.Default()

	results = make([]ingest.Result, 0, len(batch.artifacts))
	for _, a := range batch.artifacts {
		res, ierr := pipeline.Ingest(ctx, ingest.Request{
			RootID: root.ID, SourcePath: a.Path, RelPath: a.RelPath, Work: override,
		})
		if ierr != nil {
			return nil, 0, ierr
		}
		results = append(results, res)
		if res.AssetID == "" {
			// Tombstoned (ADR-0073): no asset was recorded, nothing to link.
			continue
		}
		key, ok := episodeItemKey(reg, a.RelPath)
		if !ok {
			continue // unparseable season/episode → left unlinked
		}
		itemID, ok := itemByKey[key]
		if !ok {
			continue // no item for this episode (wrong season, or no want) → left unlinked
		}
		if err := cat.SetAssetItem(ctx, res.AssetID, itemID); err != nil {
			return nil, 0, fmt.Errorf("linking pack file %s to item %s: %w", a.RelPath, itemID, err)
		}
		linked++
	}
	return results, linked, nil
}

// episodeItemKey parses a pack file's season and episode and returns the
// item_key ("S01E01") they form. It returns false when the name yields no
// season/episode — a file that cannot be placed is left unlinked, never guessed
// onto an episode.
func episodeItemKey(reg *identification.Registry, relPath string) (string, bool) {
	c := reg.Identify(relPath, identification.Series)
	season, sok := intAttr(c.EditionAttributes["season"])
	episode, eok := intAttr(c.EditionAttributes["episode"])
	if !sok || !eok {
		return "", false
	}
	return fmt.Sprintf("S%02dE%02d", season, episode), true
}

// intAttr reads an integer out of an identification edition attribute, whatever
// concrete numeric type it arrived as.
func intAttr(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}

// failAcquisition takes the failure edge and blocks the release.
//
// Both halves matter and neither is sufficient. Without the transition the
// want sticks in VERIFYING forever; without the block the next search selects
// the same release and the download repeats until somebody notices the
// bandwidth.
func failAcquisition(
	ctx context.Context, cat *catalog.Catalog, desiredItemID string,
	acq catalog.Acquisition, reason catalog.BlockReason, cause error, log *slog.Logger,
) error {
	selected, err := cat.SelectedCandidate(ctx, desiredItemID)
	switch {
	case errors.Is(err, catalog.ErrNoCandidate):
		// No selected candidate to blame. Possible when an acquisition was
		// adopted rather than searched for; the want still has to leave
		// VERIFYING, so the transition happens and there is simply nothing to
		// block.
		log.Warn("an acquisition failed with no selected candidate to block",
			"desired_item_id", desiredItemID, "cause", cause)
	case err != nil:
		return err
	default:
		if _, err := cat.BlockRelease(ctx, catalog.BlockedRelease{
			DesiredItemID: desiredItemID,
			Provider:      selected.Provider,
			CandidateID:   selected.CandidateID,
			Title:         selected.Title,
			Detail:        cause.Error(),
			Reason:        reason,
		}); err != nil {
			return err
		}
	}

	if _, err := cat.AdvanceAcquisition(ctx, desiredItemID,
		acquisition.TransitionFail, cause.Error()); err != nil {
		return err
	}

	log.Warn("an acquisition did not survive verification",
		"desired_item_id", desiredItemID, "transfer", acq.ExternalID,
		"reason", string(reason), "cause", cause)

	// Not an error. The outcome is recorded, the want is at rest, and the
	// release will not be chosen again — retrying the job would re-hash the
	// same bad file and reach the same conclusion more slowly.
	return nil
}
