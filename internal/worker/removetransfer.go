package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/downloads"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// RemoveTransferStore is the part of the catalog the removal reads.
type RemoveTransferStore interface {
	AcquisitionByExternal(ctx context.Context, provider, externalID string) (catalog.Acquisition, error)
}

// RemoveTransferHandler takes a transfer a by-hand decision displaced out of
// its download client, with its partial data (#888).
//
// The want has already moved on: SelectByHand or CancelInFlightRelease
// cancelled it off this transfer and dropped the row in the same transaction.
// Until this runs the transfer keeps downloading something nobody wants, and
// the poll logs it as a labelled transfer with no row.
//
// # It removes only a transfer no want has taken up again
//
// The row is the guard. A person who replaced a release and then chose it
// again has a grab that re-adopted the same transfer (an add of a transfer
// already present returns it), and that grab wrote a row naming it. Removing
// it then would undo the second choice, so a row naming the transfer means
// leave it.
//
// # Idempotent (invariant 9)
//
// A transfer already gone — removed by an earlier run whose completion was
// lost, or by the operator in the client — is the outcome this job wants, so
// the client's "not ours" for it is success. The data is deleted with it: the
// transfer was never completed into the library (a displaced want was QUEUED
// or DOWNLOADING), so its bytes are a partial download of a release the
// operator turned down.
func RemoveTransferHandler(reg *providers.Registry, cat RemoveTransferStore, log *slog.Logger) HandlerFunc {
	return func(ctx context.Context, job jobs.Job) error {
		p, err := decodePayload[acquisition.RemoveTransferPayload](job)
		if err != nil {
			return err
		}
		if p.Provider == "" || p.ExternalID == "" {
			return fmt.Errorf("%w: worker: remove_transfer needs a provider and a transfer", jobs.ErrPermanent)
		}

		switch _, err := cat.AcquisitionByExternal(ctx, p.Provider, p.ExternalID); {
		case err == nil:
			log.Info("not removing a displaced transfer: a want has taken it up again",
				"desired_item_id", p.DesiredItemID, "provider", p.Provider, "transfer", p.ExternalID)
			return nil
		case !errors.Is(err, catalog.ErrNoAcquisitionRow):
			return err
		}

		var client providers.Downloader
		for _, d := range reg.Downloaders() {
			if d.Name() == p.Provider {
				client = d
				break
			}
		}
		if client == nil {
			return fmt.Errorf("worker: no download client named %q here to remove %s from", p.Provider, p.ExternalID)
		}
		r, ok := client.(providers.Remover)
		if !ok {
			log.Warn("a displaced transfer cannot be removed: its client does not remove; remove it there by hand",
				"desired_item_id", p.DesiredItemID, "provider", p.Provider, "transfer", p.ExternalID)
			return nil
		}
		if err := r.Remove(ctx, p.ExternalID, true); err != nil {
			if errors.Is(err, downloads.ErrNotOurs) {
				// Already gone. What this job was for.
				return nil
			}
			return fmt.Errorf("worker: removing %s from %s: %w", p.ExternalID, p.Provider, err)
		}
		log.Info("removed a transfer a by-hand decision displaced",
			"desired_item_id", p.DesiredItemID, "provider", p.Provider,
			"transfer", p.ExternalID, "reason", p.Reason)
		return nil
	}
}
