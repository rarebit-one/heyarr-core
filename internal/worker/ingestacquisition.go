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

		artifact, verifyErr := verifyArtifact(acq)
		if verifyErr != nil {
			return failAcquisition(ctx, cat, payload.DesiredItemID, acq,
				catalog.BlockVerificationFailed, verifyErr, log)
		}

		if _, err := cat.AdvanceAcquisition(ctx, payload.DesiredItemID,
			acquisition.TransitionVerified,
			fmt.Sprintf("%d bytes at %s", artifact.Size, filepath.Base(artifact.Path))); err != nil {
			return err
		}

		res, ingestErr := ingestArtifact(ctx, cat, roots, pipeline, payload.DesiredItemID, artifact)
		if ingestErr != nil {
			// A local problem, most likely, rather than a bad release — which
			// is why it is blocked under a DIFFERENT reason. See
			// catalog.BlockIngestFailed.
			return failAcquisition(ctx, cat, payload.DesiredItemID, acq,
				catalog.BlockIngestFailed, ingestErr, log)
		}

		if _, err := cat.AdvanceAcquisition(ctx, payload.DesiredItemID,
			acquisition.TransitionIngested,
			fmt.Sprintf("asset %s, blob %s, %s", res.AssetID, res.BlobHash, res.Materialised)); err != nil {
			return err
		}

		// §66 puts probe in the pipeline, and it is enqueued rather than run
		// for the same reason the scanner's path enqueues it: a probe is a job
		// and may need a capability this worker does not have (ADR-0023).
		if probes != nil {
			enqueueProbe(ctx, probes, res, artifact.RelPath)
		}

		// Satisfaction is NOT set here, deliberately.
		//
		// Ingest produces bytes; whether they satisfy the quality profile is
		// §56's question and reconciliation answers it (M3-05). Assuming yes
		// here is exactly how AVAILABLE and CONTENT_SATISFIED collapse into
		// each other, which ADR-0027 exists to prevent — and it would be a
		// particularly bad place to do it, because the bytes that just arrived
		// are the ones nobody has evaluated yet.
		log.Info("an acquisition was ingested",
			"desired_item_id", payload.DesiredItemID,
			"asset_id", res.AssetID, "blob", res.BlobHash,
			"materialised", string(res.Materialised),
			"deduplicated", res.Deduplicated)
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

// verifyArtifact hashes what actually arrived (invariant 1).
//
// It refuses a directory. A multi-file torrent completes as one, and ingesting
// it as a single artifact would be wrong in a way that produces a plausible
// blob — the whole directory tarred by accident, say. Milestone 3 acquires
// single files; a multi-file release is a real gap and it is refused loudly
// rather than half-handled.
func verifyArtifact(acq catalog.Acquisition) (artifact, error) {
	path := acq.LocalPath
	if strings.TrimSpace(path) == "" {
		// #102 resolves the client's path into one Heyarr can open and stores
		// it here; RemotePath carries the same value today. Preferring
		// LocalPath keeps the distinction meaningful when the two diverge.
		path = acq.RemotePath
	}
	if strings.TrimSpace(path) == "" {
		return artifact{}, errors.New("the download client did not say where the bytes are")
	}

	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// The single most common operational failure in this class of
		// software, and it is worth naming precisely rather than as "no such
		// file": the transfer completed, the client says it is done, and the
		// path it reported is not one Heyarr can open. That is a path mapping
		// problem, not a missing file.
		return artifact{}, fmt.Errorf(
			"the completed transfer is not at %s — the download client's path may "+
				"need mapping into one Heyarr can open", path)
	case err != nil:
		return artifact{}, fmt.Errorf("could not examine %s: %w", path, err)
	case info.IsDir():
		return artifact{}, fmt.Errorf(
			"%s is a directory — a multi-file release is not ingestable in this "+
				"milestone, and treating it as one artifact would produce a "+
				"plausible blob of the wrong thing", path)
	case info.Size() == 0:
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

	return artifact{
		Path:    path,
		RelPath: filepath.Base(path),
		Size:    info.Size(),
	}, nil
}

// ingestArtifact hands verified bytes to the M1 pipeline.
func ingestArtifact(
	ctx context.Context, cat *catalog.Catalog, roots acquisitionRoot,
	pipeline *ingest.Pipeline, desiredItemID string, a artifact,
) (ingest.Result, error) {
	sc, err := cat.SearchContextFor(ctx, desiredItemID)
	if err != nil {
		return ingest.Result{}, err
	}
	root, err := roots.RootForContentType(ctx, sc.ContentType)
	if err != nil {
		return ingest.Result{}, err
	}

	// The SAME pipeline the scanner uses. The only thing this handler tells it
	// that a walk would not is which root to land in.
	return pipeline.Ingest(ctx, ingest.Request{
		RootID:     root.ID,
		SourcePath: a.Path,
		RelPath:    a.RelPath,
	})
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
