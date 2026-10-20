package worker

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"

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
// catalog_op_applications holds what was done per op hash. A final outcome is
// not revisited; `scope_unresolved` (the op names an edition or item this
// node has not materialised) is retried every pass until it resolves or a
// newer op supersedes it. A newer op for the same (work, scope, aspect) is a
// different hash and gets its own application.
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
		var applied, skipped int
		for _, op := range view.Acquisitions {
			if prior, ok := done[op.Hash]; ok && prior.Outcome.Final() {
				continue
			}
			outcome, wantID, detail, err := applyOne(ctx, cat, q, op, selfKey, log)
			if err != nil {
				// A storage error: leave the op for the next pass rather than
				// recording an outcome it did not reach.
				return fmt.Errorf("worker: applying acquire op %s: %w", op.Hash, err)
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
		// The want is past idle or already holds the content: either way the
		// op changes nothing here. If it is acquiring, it is acquiring what
		// this site chose; convergence on the SAME bytes is the next pass's
		// job once a newer op says so.
		return catalog.FanOutHeld, want.DesiredItemID, detail + "; the want is already acquiring or satisfied", nil
	}
	enqueueGrab(ctx, q, want.DesiredItemID, cand.ID, log)
	log.Info("a sibling's release is queued for acquisition here",
		"desired_item_id", want.DesiredItemID, "work_key", op.Target.WorkKey,
		"infohash", a.Infohash, "site", a.Site, "work_created", want.WorkCreated, "want_created", want.Created)
	return catalog.FanOutApplied, want.DesiredItemID, detail, nil
}

// magnetFor renders the trackerless magnet for a public release: the
// infohash and the display name. Trackers the op carries are appended as
// `tr` parameters (they are credential-free by construction).
func magnetFor(a *catalogop.Acquire) string {
	v := url.Values{}
	v.Set("dn", a.Title)
	for _, t := range a.Trackers {
		v.Add("tr", t)
	}
	return "magnet:?xt=urn:btih:" + a.Infohash + "&" + v.Encode()
}
