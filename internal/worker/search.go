package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The search job (§60, §63, M3-12) — where the two lanes of this milestone
// meet.
//
// # It orchestrates and decides nothing
//
// It asks the registry for candidates, hands them to M3-04's evaluator, stores
// what came back, and drives M3-03's machine from the answer. There is no
// second opinion here about what is better: if this file ever needed one, that
// would be a finding about the evaluator rather than a place to add a
// heuristic, because two notions of "better" drift and then "why did it take
// that one" has two answers.
//
// # A search that finds nothing is not a failure
//
// It is a modelled edge with its own transition, and the handler returns nil.
// Failing the job would put it into retry backoff, which turns an unavailable
// release into an indexer hammering loop — and the release is not going to
// become available because Heyarr asked more often. Backing off belongs on the
// SCHEDULE that enqueues searches, not on the job that runs one.
//
// # A search that finds twelve unacceptable releases is not a failure either
//
// It leaves twelve explained rejections and returns to rest. §60 keeps
// explainable rejection reasons among the things Heyarr retains precisely
// because a want that goes quiet with no record is the failure mode of the
// software it replaces.

// candidateLimit bounds what one search asks each indexer for.
//
// Not a limit anyone meets — a real search returns tens — but a bound on what
// a misconfigured or hostile indexer can make the evaluator score, and what
// one search can put in the candidates table. §63's scorer is linear in
// candidates times rules, and both sides of that are attacker-influenced.
const candidateLimit = 200

// candidateRetention is how long a superseded candidate set is kept.
//
// Long enough that "why did it pick that release, last Tuesday" is answerable
// from the rows rather than only from the event log; short enough that a
// library of wants nobody searches any more does not accumulate forever.
//
// The event log keeps the DECISION permanently — acquisition.search_completed
// records what was found and what was chosen — so what expires here is the
// per-candidate detail, which is the part that is bulky and the part that
// stops being interesting once the search that produced it is stale.
const candidateRetention = 30 * 24 * time.Hour

// SearchHandler runs one want's search.
func SearchHandler(
	reg *providers.Registry, cat *catalog.Catalog, log *slog.Logger,
) HandlerFunc {
	return func(ctx context.Context, job jobs.Job) error {
		var payload acquisition.SearchPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("worker: search_release payload is not decodable: %w", err)
		}
		if payload.DesiredItemID == "" {
			return errors.New("worker: search_release needs a desired item")
		}

		// The prune runs here, on every search, and it is GLOBAL rather than
		// scoped to this want. Replacement already bounds the table in the
		// normal path; what accumulates is the last set belonging to wants
		// nobody searches any more, which a per-want prune can never reach.
		if n, err := cat.PruneCandidates(ctx, time.Now().UTC().Add(-candidateRetention)); err != nil {
			// Not fatal. Failing to tidy must not stop a search from running:
			// the table growing is a slow problem and a search not happening
			// is an immediate one.
			log.Warn("could not prune superseded candidates", "error", err)
		} else if n > 0 {
			log.Info("pruned superseded candidates", "rows", n)
		}

		sc, err := cat.SearchContextFor(ctx, payload.DesiredItemID)
		if err != nil {
			return err
		}

		// Something is already in flight for this want — it is downloading, or
		// verifying. Searching now would be racing the acquisition it already
		// started, so the honest answer is to do nothing.
		//
		// Returning nil rather than an error: the job did exactly what it
		// should, and failing it would retry the same refusal on a backoff.
		if _, err := acquisition.Advance(sc.State.Phase, acquisition.TransitionSearch); err != nil {
			log.Info("skipping a search: something is already in flight",
				"desired_item_id", payload.DesiredItemID, "phase", string(sc.State.Phase))
			return nil
		}

		if _, err := cat.AdvanceAcquisition(ctx, payload.DesiredItemID,
			acquisition.TransitionSearch, ""); err != nil {
			return err
		}

		result, err := reg.Search(ctx, providers.Query{
			Title:       sc.Title,
			Year:        sc.Year,
			ContentType: sc.ContentType,
			Limit:       candidateLimit,
		})
		if err != nil {
			// No indexer at all should be unreachable — the job is
			// capability-routed on `indexer`, so a node with none never claims
			// it (ADR-0025). If it happens anyway, return the want to rest
			// rather than leaving it stuck in SEARCHING forever.
			if _, ferr := cat.AdvanceAcquisition(ctx, payload.DesiredItemID,
				acquisition.TransitionFail, err.Error()); ferr != nil {
				return ferr
			}
			return fmt.Errorf("worker: searching for %s: %w", payload.DesiredItemID, err)
		}
		for _, f := range result.Failures {
			// Visible rather than fatal. One indexer being down must not
			// discard what the others returned (§60 keeps operational
			// visibility), and it must not be silent either.
			log.Warn("an indexer could not answer",
				"provider", f.Provider, "detail", f.Detail,
				"desired_item_id", payload.DesiredItemID)
		}

		if len(result.Candidates) == 0 {
			// Nothing found. A modelled edge, recorded so the want does not go
			// quiet — RecordSearch emits even with nothing to store, which is
			// the only trace an empty search leaves.
			if _, err := cat.RecordSearch(ctx, payload.DesiredItemID, nil); err != nil {
				return err
			}
			if _, err := cat.AdvanceAcquisition(ctx, payload.DesiredItemID,
				acquisition.TransitionNoCandidates,
				fmt.Sprintf("%d indexer(s) had nothing", result.Consulted)); err != nil {
				return err
			}
			log.Info("a search found nothing",
				"desired_item_id", payload.DesiredItemID, "consulted", result.Consulted)
			return nil
		}

		// §63's scorer, and nothing else decides.
		ranked := acquisition.EvaluateAll(result.Candidates, sc.Profile)

		outcome, err := cat.RecordSearch(ctx, payload.DesiredItemID, ranked)
		if err != nil {
			return err
		}
		if _, err := cat.AdvanceAcquisition(ctx, payload.DesiredItemID,
			acquisition.TransitionCandidatesFound,
			fmt.Sprintf("%d candidate(s), %d acceptable", outcome.Found, outcome.Acceptable)); err != nil {
			return err
		}

		if outcome.SelectedCandidateID == "" {
			// Found things, took none. The rejections are already durable —
			// that is the deliverable — and the want returns to rest.
			if _, err := cat.AdvanceAcquisition(ctx, payload.DesiredItemID,
				acquisition.TransitionRejectAll,
				fmt.Sprintf("%d candidate(s), none acceptable", outcome.Found)); err != nil {
				return err
			}
			log.Info("a search found nothing acceptable",
				"desired_item_id", payload.DesiredItemID, "candidates", outcome.Found)
			return nil
		}

		if _, err := cat.AdvanceAcquisition(ctx, payload.DesiredItemID,
			acquisition.TransitionSelect,
			fmt.Sprintf("selected %s", outcome.SelectedCandidateID)); err != nil {
			return err
		}
		log.Info("a search selected a release",
			"desired_item_id", payload.DesiredItemID,
			"candidate", outcome.SelectedCandidateID,
			"candidates", outcome.Found, "acceptable", outcome.Acceptable)
		return nil
	}
}
