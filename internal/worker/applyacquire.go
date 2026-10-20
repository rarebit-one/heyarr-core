package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// FanOutStore is the part of the catalog the applier uses.
type FanOutStore interface {
	FanOutApplications(ctx context.Context) (map[string]catalog.FanOutApplication, error)
	RecordFanOutApplication(ctx context.Context, opHash, desiredItemID string, outcome catalog.FanOutOutcome, detail string) error
	EnsureFanOutWant(ctx context.Context, op catalogop.Op) (catalog.FanOutWant, error)
	RecordDirectRelease(ctx context.Context, desiredItemID string, cand acquisition.ReleaseCandidate) (bool, error)
	Acquisition(ctx context.Context, desiredItemID string) (catalog.AcquisitionRecord, error)
	SelectedCandidate(ctx context.Context, desiredItemID string) (catalog.Candidate, error)
	FanOutStale(ctx context.Context) (map[string]bool, error)
	BlockedFor(ctx context.Context, desiredItemID string) ([]catalog.BlockedRelease, error)
}

// OpsReader is the op-log read the applier needs.
type OpsReader interface {
	Ops(ctx context.Context) ([]string, error)
}

// ApplyAcquireHandler applies the acquire ops siblings published.
//
// # Own ops are skipped by signer
//
// The origin's op is in its own log too. It is recognised by `by` — this
// peer's public key — and recorded as `own`, so a site never fans out to
// itself.
//
// # Each op is applied once, and the outcome is durable
//
// catalog_op_applications holds what was done per op hash. A final outcome
// (`applied`, `held`, `own`) is revisited only when what it settled no longer
// stands: its want is gone (deleted, perhaps restored since) or back at idle
// without content (its grab failed). The others (`scope_unresolved`,
// `no_site_source`, `tombstoned`, `deferred`) are re-evaluated every pass
// until they settle or a newer op supersedes them; an unchanged outcome is not
// rewritten. A newer op for the same (work, scope, aspect) is a different hash
// and gets its own application.
//
// A want fan-out created is never searched by this site's scorer (it fetches
// the sibling's exact release or nothing), and a release this want already
// failed to fetch here is not selected again.
//
// # The source is this node's own
//
// A public release is joined by infohash: a magnet with no tracker, which the
// client resolves through DHT and the sibling that is already seeding. A
// private release carries no trackers (ADR-0119) and is not resolvable here in
// this increment: it is recorded `no_site_source`, visible on the want's
// absence, until a later increment searches this node's own indexers for it.
func ApplyAcquireHandler(ops OpsReader, cat FanOutStore, q ProbeEnqueuer, selfKey string, log *slog.Logger) HandlerFunc {
	return func(ctx context.Context, _ jobs.Job) error {
		tokens, err := ops.Ops(ctx)
		if err != nil {
			return err
		}
		view := catalogop.Evaluate(tokens)
		if len(view.Acquisitions) == 0 {
			return nil
		}
		done, err := cat.FanOutApplications(ctx)
		if err != nil {
			return err
		}
		stale, err := cat.FanOutStale(ctx)
		if err != nil {
			return err
		}
		var applied, skipped int
		for _, op := range view.Acquisitions {
			prior, seen := done[op.Hash]
			if seen && prior.Outcome.Final() && !stale[op.Hash] {
				continue
			}
			outcome, wantID, detail, err := applyOne(ctx, cat, q, op, selfKey, log)
			if err != nil {
				// A storage or queue error: leave the op for the next pass
				// rather than recording an outcome it did not reach.
				return fmt.Errorf("worker: applying acquire op %s: %w", op.Hash, err)
			}
			if seen && prior.Outcome == outcome && prior.Detail == detail && prior.DesiredItemID == wantID {
				// A re-evaluated outcome that has not moved: nothing to record
				// or report. (The store also refuses to write or emit for it.)
				continue
			}
			if err := cat.RecordFanOutApplication(ctx, op.Hash, wantID, outcome, detail); err != nil {
				return err
			}
			if outcome == catalog.FanOutApplied {
				applied++
			} else {
				skipped++
			}
		}
		if applied > 0 || skipped > 0 {
			log.Info("applied siblings' acquire ops", "applied", applied, "skipped", skipped)
		}
		return nil
	}
}

// applyOne resolves one op to an outcome. Errors are storage failures only;
// every expected shape is an outcome.
func applyOne(ctx context.Context, cat FanOutStore, q ProbeEnqueuer, op catalogop.Op, selfKey string, log *slog.Logger) (catalog.FanOutOutcome, string, string, error) {
	a := op.Acquire
	if selfKey != "" && op.By == selfKey {
		return catalog.FanOutOwn, "", "signed by this peer", nil
	}
	if a.Private {
		// Nothing is created for it: a want here would be searched by this
		// site's own scorer and could grab a different release, which a
		// fan-out want must never do (ADR-0119). Retried each pass.
		return catalog.FanOutNoSiteSource, "", "a private release publishes no trackers; resolving it through this site's own indexers is not built yet", nil
	}
	want, err := cat.EnsureFanOutWant(ctx, op)
	switch {
	case catalog.IsFanOutScopeUnresolved(err):
		return catalog.FanOutScopeUnresolved, "", err.Error(), nil
	case catalog.IsFanOutTombstoned(err):
		return catalog.FanOutTombstoned, "", "the work is tombstoned here", nil
	case catalog.IsFanOutNoProfile(err):
		return catalog.FanOutNoSiteSource, "", err.Error(), nil
	case err != nil:
		return "", "", "", err
	}
	detail := "from site " + a.Site
	if want.ProfileFallback != "" {
		detail += fmt.Sprintf("; profile %q unknown here, used %q", a.Profile, want.ProfileFallback)
	}
	// A release this want already failed to fetch here (a dead swarm, a
	// stalled transfer the stuck-grab sweep blocked) is not selected again:
	// that would loop the want through the same stall. With no other source,
	// the op waits for a newer one.
	blocked, err := cat.BlockedFor(ctx, want.DesiredItemID)
	if err != nil {
		return "", "", "", err
	}
	for _, b := range blocked {
		if b.CandidateID == "infohash:"+a.Infohash {
			return catalog.FanOutNoSiteSource, want.DesiredItemID,
				detail + "; the release did not fetch here and is blocked for this want", nil
		}
	}
	cand := acquisition.ReleaseCandidate{
		ID:       "infohash:" + a.Infohash,
		Title:    a.Title,
		Provider: catalog.FanOutProviderPrefix + a.Site,
		Source:   secret.Value(magnetFor(a)),
	}
	if a.Size > 0 {
		cand.Attributes = acquisition.Attributes{policy.AttrSizeBytes: policy.Num(a.Size)}
	}
	selected, err := cat.RecordDirectRelease(ctx, want.DesiredItemID, cand)
	if err != nil {
		return "", "", "", err
	}
	if !selected {
		// The want is past idle or already holds the content. One shape of
		// that is this op's own earlier, interrupted application: the direct
		// release committed and the process died (or the enqueue failed)
		// before the grab was queued, so the want rests SELECTED on this very
		// candidate with no outcome recorded. That is resumed, not held.
		resume, fanOutInFlight, err := selectedOn(ctx, cat, want.DesiredItemID, cand.ID)
		if err != nil {
			return "", "", "", err
		}
		if fanOutInFlight {
			// The want is still fetching a sibling's EARLIER choice. Once that
			// finishes, the want follows the sibling to this newer one; until
			// then the op is deferred, not held, so a later pass applies it.
			return catalog.FanOutDeferred, want.DesiredItemID, detail + "; the want is still fetching a sibling's earlier choice", nil
		}
		if !resume {
			// Otherwise the op changes nothing here: if the want is acquiring,
			// it is acquiring what this site chose.
			return catalog.FanOutHeld, want.DesiredItemID, detail + "; the want is already acquiring or satisfied", nil
		}
		detail += "; resumed an interrupted application"
	}
	// The enqueue's error is returned, not logged and swallowed as the search
	// beat does: an outcome recorded `applied` is final, so a grab that was
	// never queued would never be.
	if q != nil {
		if err := queueGrab(ctx, q, want.DesiredItemID, cand.ID); err != nil {
			return "", "", "", fmt.Errorf("queueing the grab for %s: %w", want.DesiredItemID, err)
		}
	}
	log.Info("a sibling's release is queued for acquisition here",
		"desired_item_id", want.DesiredItemID, "work_key", op.Target.WorkKey,
		"infohash", a.Infohash, "site", a.Site, "work_created", want.WorkCreated, "want_created", want.Created)
	return catalog.FanOutApplied, want.DesiredItemID, detail, nil
}

// selectedOn reports whether the want rests SELECTED on candidateID (an
// interrupted application to resume), and whether it is in flight on a
// DIFFERENT release it took from a sibling's op (a newer op to defer).
func selectedOn(ctx context.Context, cat FanOutStore, desiredItemID, candidateID string) (resume, fanOutInFlight bool, err error) {
	state, err := cat.Acquisition(ctx, desiredItemID)
	if err != nil {
		return false, false, err
	}
	if state.State.Phase == acquisition.PhaseIdle {
		return false, false, nil
	}
	sel, err := cat.SelectedCandidate(ctx, desiredItemID)
	if errors.Is(err, catalog.ErrNoCandidate) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if sel.CandidateID == candidateID {
		return state.State.Phase == acquisition.PhaseSelected, false, nil
	}
	return false, strings.HasPrefix(sel.Provider, catalog.FanOutProviderPrefix), nil
}

// magnetFor renders the magnet for a public release: the exact topic for the
// infohash and the display name. A 40-character hash is BitTorrent v1
// (`urn:btih:`); a 64-character one is a v2 SHA-256 hash, which a magnet names
// as a multihash (`urn:btmh:` with the sha2-256 prefix 0x12 0x20). Trackers
// the op carries are appended as `tr` parameters (they are credential-free by
// construction).
func magnetFor(a *catalogop.Acquire) string {
	v := url.Values{}
	v.Set("dn", a.Title)
	for _, t := range a.Trackers {
		v.Add("tr", t)
	}
	xt := "urn:btih:" + a.Infohash
	if len(a.Infohash) == 64 {
		xt = "urn:btmh:1220" + a.Infohash
	}
	return "magnet:?xt=" + xt + "&" + v.Encode()
}
