package worker

import (
	"context"
	"log/slog"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// upgradeScanBatch bounds one sweep, for the same reason reconcileBatch does:
// the failure mode of a very large library should be "the scan takes several
// passes" rather than "the scan times out and nothing is ever upgraded".
const upgradeScanBatch = 5000

// UpgradeScanner runs the upgrade scan. A
// *catalog.Catalog satisfies it; the handler depends on no more than it calls.
type UpgradeScanner interface {
	ScanForUpgrades(ctx context.Context, limit int) (catalog.UpgradeScanResult, error)
}

// UpgradeScanHandler finds the wants that could be improved (§60, M3-06).
//
// # It looks, and does not search
//
// The scan answers the ELIGIBILITY question — monitored, satisfied, not yet
// terminal — and stops there. Finding out whether anything BETTER is actually
// on offer needs a provider round trip, which is the search job's work
// (M3-12). Doing it here would make a library-wide sweep perform one network
// call per want, on a timer.
//
// # Silent when there is nothing to say
//
// A scan over a library with nothing upgradable writes nothing and emits
// nothing. Most wants are terminal or unmonitored or not satisfied most of the
// time, and a beat that announced that every pass would be a heartbeat.
//
// # Idempotent
//
// It will be re-run (invariant 9). It reads and concludes; running it twice
// over an unchanged library reaches the same conclusions and records them the
// same way.
func UpgradeScanHandler(cat UpgradeScanner, log *slog.Logger) HandlerFunc {
	return func(ctx context.Context, job jobs.Job) error {
		payload, err := decodeOptionalPayload[acquisition.UpgradeScanPayload](job)
		if err != nil {
			return err
		}

		result, err := cat.ScanForUpgrades(ctx, upgradeScanBatch)
		if err != nil {
			return err
		}

		// Scoped runs report only the want that was asked about. The filter is
		// applied after the scan rather than pushed into the query because the
		// scoped form exists for an operator poking at one want, not for a hot
		// path — and one extra pass over a slice is cheaper than a second
		// query shape to keep correct.
		eligible := result.Eligible
		if payload.DesiredItemID != "" {
			eligible = nil
			for _, e := range result.Eligible {
				if e.DesiredItemID == payload.DesiredItemID {
					eligible = append(eligible, e)
				}
			}
		}

		if len(eligible) == 0 {
			// The normal case for a healthy library. Nothing logged, nothing
			// emitted.
			return nil
		}

		for _, e := range eligible {
			log.Info("a want could be improved",
				"desired_item_id", e.DesiredItemID,
				"work_id", e.WorkID,
				"incumbent", e.IncumbentID,
				"why", e.Verdict.Detail)
		}
		log.Info("upgrade scan swept",
			"monitored", result.Considered, "eligible", len(eligible))
		return nil
	}
}
