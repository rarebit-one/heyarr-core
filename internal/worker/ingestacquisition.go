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

// IngestAcquisitionStore is the part of the catalog the acquisition ingest handler uses. A
// *catalog.Catalog satisfies it; the handler depends on no more than it calls.
type IngestAcquisitionStore interface {
	Acquisition(ctx context.Context, desiredItemID string) (catalog.AcquisitionRecord, error)
	AcquisitionFor(ctx context.Context, desiredItemID string) (catalog.Acquisition, error)
	AdvanceAcquisition(ctx context.Context, desiredItemID string, t acquisition.Transition, detail string) (catalog.AcquisitionRecord, error)
	BlockRelease(ctx context.Context, b catalog.BlockedRelease) (bool, error)
	DesiredItemTarget(ctx context.Context, desiredItemID string) (scope, itemID, aspect, language string, err error)
	ItemsForWork(ctx context.Context, workID string) ([]catalog.Item, error)
	SearchContextFor(ctx context.Context, desiredItemID string) (catalog.SearchContext, error)
	SelectedCandidate(ctx context.Context, desiredItemID string) (catalog.Candidate, error)
	RecordVerifiedStoreEditions(ctx context.Context, editionIDs []string, pkg catalog.VerifiedStorePackage) error
	SetAssetItem(ctx context.Context, assetID, itemID string) error
	WorkForDesired(ctx context.Context, desiredItemID string) (catalog.DesiredWork, error)
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
	cat IngestAcquisitionStore, roots acquisitionRoot, pipeline *ingest.Pipeline,
	probes ProbeEnqueuer, log *slog.Logger,
) HandlerFunc {
	return func(ctx context.Context, job jobs.Job) error {
		payload, err := decodePayload[acquisition.IngestPayload](job)
		if err != nil {
			return err
		}
		if payload.DesiredItemID == "" {
			return errors.New("worker: ingest_acquisition needs a desired item")
		}

		state, err := cat.Acquisition(ctx, payload.DesiredItemID)
		if err != nil {
			return err
		}
		// Ingest runs from VERIFYING, and also RESUMES any want left in
		// INGESTING — the phase itself is the proof it did not finish, because a
		// finished ingest is back at idle. Two ways a want sticks there: an
		// ingest that verified and advanced, then died in materialisation before
		// writing any bytes (managed=false, a crash or node OOM); OR one that
		// materialised its bytes (managed=true) but died before the
		// INGESTING→idle transition landed — or partway through a multi-file
		// pack. Both have no job driving them and sit in INGESTING forever; the
		// stuck-ingest watchdog (and `desired reingest`) re-enqueue this job, and
		// resuming here is what clears them.
		//
		// It used to resume only the managed=false case, on the reading that a
		// managed want in INGESTING was a duplicate delivery of finished work.
		// It is not: a finished want is at idle, so a managed want STILL in
		// INGESTING is one whose transition was lost, and no-oping it left the
		// watchdog re-driving it every pass forever (a 32 GB Yellowstone pack did
		// exactly this). Re-running is cheap and safe: the CAS keys blobs on a
		// hash of the source, so an already-held file is deduplicated — no
		// re-copy — and the transition then advances to idle, which is precisely
		// what stops the watchdog re-driving it.
		phase := state.State.Phase
		resuming := phase == acquisition.PhaseIngesting
		if phase != acquisition.PhaseVerifying && !resuming {
			// LATE and NEVER are different situations and used to share a log
			// line (#240). "Arrived after the want moved on" describes a second
			// delivery of a transfer already handled; an operator reading it
			// about a want that never got there goes looking for an earlier
			// delivery that does not exist.
			//
			// Managed is what tells them apart: a want holding bytes has been
			// through this, and one holding none has not. Both are quiet
			// successes — invariant 9 makes a duplicate ordinary — so what is
			// at stake is only whether the sentence is true. (An INGESTING want
			// is not here at all now — managed or not, it resumes above; this
			// branch is reached only for a want already back at idle, or one
			// somewhere else in the pipeline entirely.)
			if state.State.Managed {
				log.Info("an acquisition ingest arrived after the want moved on",
					"desired_item_id", payload.DesiredItemID,
					"phase", string(phase))
			} else {
				log.Warn("an acquisition ingest arrived for a want that never reached verifying",
					"desired_item_id", payload.DesiredItemID,
					"phase", string(phase))
			}
			return nil
		}

		acq, err := cat.AcquisitionFor(ctx, payload.DesiredItemID)
		if err != nil {
			return err
		}

		sc, err := cat.SearchContextFor(ctx, payload.DesiredItemID)
		if err != nil {
			return err
		}
		batch, verifyErr := verifyArtifacts(acq, sc.ContentType)
		if verifyErr == nil {
			batch, verifyErr, err = checkStoreManifest(ctx, cat, payload.DesiredItemID, batch)
			if err != nil {
				return err
			}
		}
		if verifyErr != nil {
			return failAcquisition(ctx, cat, payload.DesiredItemID, acq,
				catalog.BlockVerificationFailed, verifyErr, log)
		}

		// The VERIFYING→INGESTING edge is skipped on a resume: the want is
		// already there, and re-applying the transition would be illegal.
		if !resuming {
			if _, err := cat.AdvanceAcquisition(ctx, payload.DesiredItemID,
				acquisition.TransitionVerified, batch.verifiedDetail()); err != nil {
				return err
			}
		}

		results, linked, ingestErr := ingestArtifacts(ctx, cat, roots, pipeline, payload.DesiredItemID, sc, batch)
		if ingestErr != nil {
			// A local problem, most likely, rather than a bad release — which
			// is why it is blocked under a DIFFERENT reason. See
			// catalog.BlockIngestFailed.
			return failAcquisition(ctx, cat, payload.DesiredItemID, acq,
				catalog.BlockIngestFailed, ingestErr, log)
		}

		// A store package the store verified file by file earns its store facts
		// on the editions it filled (ADR-0120). Before INGESTED, so a want is
		// never reported ingested without them; idempotent, so a re-run is safe.
		storeErr, err := recordStorePackage(ctx, cat, payload.DesiredItemID, batch, results)
		if err != nil {
			return err
		}
		if storeErr != nil {
			// Ingested, but not the package the store vouched for: a file changed
			// while it was being hashed. Its bytes are held without store facts
			// (undetermined, never satisfying), and the acquisition fails like any
			// other verification failure.
			return failAcquisition(ctx, cat, payload.DesiredItemID, acq,
				catalog.BlockVerificationFailed, storeErr, log)
		}

		if _, err := cat.AdvanceAcquisition(ctx, payload.DesiredItemID,
			acquisition.TransitionIngested, ingestedDetail(batch, results, linked)); err != nil {
			return err
		}

		// §66 puts probe in the pipeline, and it is enqueued rather than run
		// for the same reason the scanner's path enqueues it: a probe is a job
		// and may need a capability this worker does not have (ADR-0023). A pack
		// produced one result per file, each probed on its own blob.
		// A game is not probed: its files are installers and archives, which
		// carry no stream for ffprobe to read, and reading tens of gigabytes of
		// them to learn that is the whole cost of the probe.
		if probes != nil && sc.ContentType != identification.Game {
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
			log.Info("a multi-file release was ingested",
				"desired_item_id", payload.DesiredItemID,
				"content_type", batch.contentType,
				"files", len(results), "linked_to_items", linked)
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

// verifiedTransfer is what a completed download resolved to: one file, or
// the primary files of a multi-file release. `path` is the top-level thing the
// client reported — the file, or the release directory.
type verifiedTransfer struct {
	artifacts   []artifact
	contentType string
	pack        bool
	path        string
	// storeSizes is, for a store package, the size the store stated for each
	// file by base name — carried through ingest so the bytes actually hashed
	// are held to it too, not only the bytes seen before ingest began.
	storeSizes map[string]int64
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
// were linked to an item.
func ingestedDetail(v verifiedTransfer, results []ingest.Result, linked int) string {
	if v.pack {
		return fmt.Sprintf("%d assets from a multi-file %s release, %d linked to items",
			len(results), v.contentType, linked)
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
// A single file yields one artifact. A directory is a multi-file release:
// each primary file for the wanted content type becomes its own artifact.
// Ingesting the directory as one blob would record the wrong thing, so it is
// enumerated rather than hashed whole.
func verifyArtifacts(acq catalog.Acquisition, contentType string) (verifiedTransfer, error) {
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
		return verifiedTransfer{}, nodeLocal(fmt.Errorf(
			"the completed transfer is not at %s — the download client's path may "+
				"need mapping into one Heyarr can open", path))
	case err != nil:
		return verifiedTransfer{}, fmt.Errorf("could not examine %s: %w", path, err)
	}

	if info.IsDir() {
		arts, derr := verifyDirectory(path, contentType)
		if derr != nil {
			return verifiedTransfer{}, derr
		}
		return verifiedTransfer{artifacts: arts, contentType: contentType, pack: true, path: path}, nil
	}

	a, ferr := verifyFile(path, filepath.Base(path), info)
	if ferr != nil {
		return verifiedTransfer{}, ferr
	}
	return verifiedTransfer{artifacts: []artifact{a}, contentType: contentType, path: path}, nil
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

// verifyDirectory enumerates the primary files inside a multi-file release.
// The content type selects the scanner's video, audio or book extension set;
// artwork, subtitles and scene metadata are left out. A single unreadable or
// empty file is skipped so the other primary files can still be held. RelPath
// includes the release directory for identification of nested seasons/discs.
//
// A directory with no primary files is a verification refusal, never a panic.
// When none survive because this node could not read them, the error says so
// and stays node-local (#813), so the release is not blocked for it.
func verifyDirectory(dir, contentType string) ([]artifact, error) {
	parent := filepath.Dir(dir)
	video := contentType == identification.Movie || contentType == identification.Series || contentType == "video"
	var arts []artifact
	var unreadable error

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			name := strings.ToLower(strings.TrimSpace(d.Name()))
			if p != dir && (name == "sample" || name == "samples" || video && isNonContentDir(d.Name())) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if !identification.IsPrimaryExt(contentType, filepath.Ext(d.Name())) || video && isSampleName(d.Name()) {
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
			if unreadable == nil && isNodeLocal(ferr) {
				unreadable = ferr
			}
			return nil
		}
		arts = append(arts, a)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("could not read the release directory %s: %w", dir, err)
	}
	if len(arts) == 0 && unreadable != nil {
		return nil, fmt.Errorf("%s holds primary files this node could not read: %w", dir, unreadable)
	}
	if len(arts) == 0 {
		kind := contentType
		if video {
			kind = "video"
		}
		return nil, fmt.Errorf(
			"%s is a directory with no ingestable %s files — a multi-file "+
				"release must carry at least one primary file to ingest", dir, kind)
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
	ctx context.Context, cat IngestAcquisitionStore, roots acquisitionRoot,
	pipeline *ingest.Pipeline, desiredItemID string, sc catalog.SearchContext, batch verifiedTransfer,
) (results []ingest.Result, linked int, err error) {
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

	// Only a series pack maps files to enumerated episode items. An album or
	// book bundle attaches each asset to the wanted Work without guessing an
	// item from its filename.
	itemByKey := map[string]string{}
	var reg *identification.Registry
	if sc.ContentType == identification.Series {
		items, err := cat.ItemsForWork(ctx, dw.ID)
		if err != nil {
			return nil, 0, err
		}
		for _, it := range items {
			itemByKey[strings.ToUpper(it.ItemKey)] = it.ID
		}
		// The same parser the scanner uses is shared by all episode files.
		reg = identification.Default()
	}

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
		if reg == nil {
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

// checkStoreManifest holds a store release to what the store vouched for
// (ADR-0120 §3). The store's check ran when the download finished; the files
// can change before this job runs, and the bundle walk skips a file it cannot
// read, so without this a package missing a part would be ingested — and
// recorded as the whole package — as if nothing had happened.
//
// It returns the batch narrowed to exactly the manifest's files — anything
// else in the directory (an older build's installer the store tool left
// behind) is not what the store vouched for and is neither ingested nor made
// part of the package — then the verification failure (the acquisition
// fails, as for any bad bytes), then an infrastructure error (the job
// retries). A release that is not a store release is returned unchanged.
func checkStoreManifest(
	ctx context.Context, cat IngestAcquisitionStore, desiredItemID string, batch verifiedTransfer,
) (narrowed verifiedTransfer, verifyErr, err error) {
	sel, err := cat.SelectedCandidate(ctx, desiredItemID)
	if err != nil {
		return batch, nil, fmt.Errorf("reading the selected release: %w", err)
	}
	if _, _, _, ok := acquisition.StoreFacts(sel.Attributes); !ok {
		return batch, nil, nil
	}
	if !batch.pack {
		return batch, errors.New("a store package must arrive as a directory with its manifest"), nil
	}
	raw, rerr := os.ReadFile(filepath.Join(batch.path, acquisition.StoreManifestName))
	if rerr != nil {
		return batch, errors.New("the store package has no readable manifest of what the store verified"), nil
	}
	var m acquisition.StoreManifest
	if jerr := json.Unmarshal(raw, &m); jerr != nil {
		return batch, errors.New("the store package's manifest is not readable JSON"), nil
	}
	// The store names files by base name only, so a name that appears twice —
	// in the manifest, or among the files on disk — cannot be matched to one
	// vouched-for file. It is refused as ambiguous rather than guessed at.
	named := make(map[string]bool, len(m.Files))
	for _, f := range m.Files {
		if named[f.Name] {
			return batch, fmt.Errorf("the store's manifest names %s twice; the package is ambiguous", f.Name), nil
		}
		named[f.Name] = true
	}
	held := make(map[string]int64, len(batch.artifacts))
	kept := batch.artifacts[:0:0]
	for _, a := range batch.artifacts {
		base := filepath.Base(a.Path)
		if !named[base] {
			continue
		}
		if _, dup := held[base]; dup {
			return batch, fmt.Errorf("two files named %s are in the package directory; the package is ambiguous", base), nil
		}
		held[base] = a.Size
		kept = append(kept, a)
	}
	if unmet := m.Unmet(held); len(unmet) > 0 {
		return batch, fmt.Errorf("the package no longer matches what the store verified: %s",
			strings.Join(unmet, "; ")), nil
	}
	batch.artifacts = kept
	batch.storeSizes = make(map[string]int64, len(m.Files))
	for _, f := range m.Files {
		batch.storeSizes[f.Name] = f.Size
	}
	return batch, nil, nil
}

// recordStorePackage writes a verified store package's facts onto the editions
// an ingest filled, when the selected release was a store release. Any other
// release is left alone: only a store fetch, which the GOG client completes only
// after the store vouched for every file, may put store facts on an edition.
//
// Every result is held to the size the store stated for its file (batch's
// storeSizes, from the manifest) before anything is recorded: the bytes hashed
// into the CAS are the ones that must match, and a file replaced or truncated
// after the pre-ingest check would otherwise be recorded as vouched for. A
// mismatch is the first return value — a verification failure — and nothing is
// recorded.
func recordStorePackage(
	ctx context.Context, cat IngestAcquisitionStore, desiredItemID string,
	batch verifiedTransfer, results []ingest.Result,
) (verifyErr, err error) {
	sel, err := cat.SelectedCandidate(ctx, desiredItemID)
	if err != nil {
		return nil, fmt.Errorf("reading the selected release: %w", err)
	}
	store, drmFree, hasDRMFree, ok := acquisition.StoreFacts(sel.Attributes)
	if !ok {
		return nil, nil
	}
	if len(results) != len(batch.artifacts) {
		return fmt.Errorf("ingested %d of the package's %d files", len(results), len(batch.artifacts)), nil
	}
	for i, r := range results {
		name := filepath.Base(batch.artifacts[i].Path)
		if want, ok := batch.storeSizes[name]; ok && want > 0 && r.BlobSize != want {
			return fmt.Errorf("%s was %d bytes when hashed, the store vouched for %d", name, r.BlobSize, want), nil
		}
	}
	ids := make([]string, 0, len(results))
	blobs := make([]string, 0, len(results))
	for _, r := range results {
		if r.EditionID != "" {
			ids = append(ids, r.EditionID)
		}
		if r.BlobHash != "" {
			blobs = append(blobs, r.BlobHash)
		}
	}
	return nil, cat.RecordVerifiedStoreEditions(ctx, ids, catalog.VerifiedStorePackage{
		Store: store, DRMFree: drmFree, HasDRMFree: hasDRMFree, Blobs: blobs,
	})
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

// errNodeLocal marks an ingest failure that is a property of THIS NODE rather
// than of the release (#813): no library for the content type, a path the
// download client reported that this node cannot open (path mapping), a
// permission this node lacks. The bytes may be perfectly good.
var errNodeLocal = errors.New("worker: a fault in this node's configuration")

type nodeLocalError struct{ err error }

func (e nodeLocalError) Error() string   { return e.err.Error() }
func (e nodeLocalError) Unwrap() []error { return []error{e.err, errNodeLocal} }

// nodeLocal marks err as this node's fault.
func nodeLocal(err error) error { return nodeLocalError{err: err} }

// isNodeLocal reports whether an ingest failure is this node's configuration
// rather than the release: marked by nodeLocal, no library root for the
// content type, a permission denied, or a file that is not where this node
// looked (a path mapping, or a client that moved it).
func isNodeLocal(err error) bool {
	return errors.Is(err, errNodeLocal) ||
		errors.Is(err, catalog.ErrNoRootForContent) ||
		errors.Is(err, fs.ErrPermission) ||
		errors.Is(err, fs.ErrNotExist)
}

// failAcquisition takes the failure edge, and blocks the release when the
// release is at fault.
//
// Both halves matter for a bad release. Without the transition the want sticks
// in VERIFYING forever; without the block the next search selects the same
// release and the download repeats until somebody notices the bandwidth.
//
// A fault of THIS NODE is not a verdict on the release (#813). Blocking it made
// a missing library or a wrong path map permanent: once the operator fixed the
// configuration, the want could never take the release it had already
// downloaded, and a fan-out want (ADR-0119) recorded no_site_source for good.
// So a node-local failure takes the failure edge WITHOUT a block and leaves the
// completed transfer where it is. Once the configuration is fixed, the next
// search (or, for a fan-out want, the next apply pass) selects the same
// release, the client answers the grab with the transfer it already holds, and
// the ingest runs again.
func failAcquisition(
	ctx context.Context, cat IngestAcquisitionStore, desiredItemID string,
	acq catalog.Acquisition, reason catalog.BlockReason, cause error, log *slog.Logger,
) error {
	if isNodeLocal(cause) {
		if _, err := cat.AdvanceAcquisition(ctx, desiredItemID,
			acquisition.TransitionFail, cause.Error()); err != nil {
			return err
		}
		log.Warn("an acquisition failed on this node's configuration; its release is not blocked",
			"desired_item_id", desiredItemID, "transfer", acq.ExternalID,
			"reason", string(reason), "cause", cause)
		return nil
	}

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
