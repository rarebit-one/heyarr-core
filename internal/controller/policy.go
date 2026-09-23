package controller

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// seedQualityProfiles makes sure this installation has profiles to point a
// DesiredItem at (§62, M3-01).
//
// It belongs to the controller for the same reason the libraries block does:
// policy is control-plane configuration (§7), and the worker that later
// evaluates a candidate against a profile learns it from the database, not
// from a file it might disagree about.
//
// It runs at EVERY start, not only the first. That is deliberate and it is the
// same reasoning as re-enqueueing a scan: the interesting case is not the
// first start but the hundredth, where a Heyarr upgraded from a version that
// had fewer defaults should acquire the new ones. Seeding converges on the
// profile name and never updates an existing row, so this costs one indexed
// lookup per default per start and cannot revert an operator's edit.
func seedQualityProfiles(ctx context.Context, db *sqlite.DB, cfg config.Config, log *slog.Logger) error {
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Logger: log})
	if err != nil {
		return fmt.Errorf("controller: opening the event log: %w", err)
	}
	cat, err := catalog.New(catalog.Options{
		DB:       db,
		Events:   eventLog,
		PeerName: cfg.Peer.Name,
		PeerSite: cfg.Peer.Site,
		Logger:   log,
	})
	if err != nil {
		return fmt.Errorf("controller: opening the catalog: %w", err)
	}
	// The household language preference (§62, #558) is applied to the seeded
	// video profiles here, so `everyday`/`living-room`/`archival` prefer the
	// wanted language without a bespoke `everyday-en` clone. It is a no-op when
	// unconfigured, and — because seeding converges on the name and never
	// overwrites an existing row — it shapes the profiles a FRESH install gets;
	// an operator editing a live profile's language uses `quality-profile set`.
	seeded := policy.WithLanguageDefaults(policy.Defaults(), cfg.Language.Policy())
	if _, err := cat.SeedQualityProfiles(ctx, seeded); err != nil {
		return fmt.Errorf("controller: seeding quality profiles: %w", err)
	}
	return nil
}
