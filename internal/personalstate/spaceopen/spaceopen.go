// Package spaceopen is how a device opens an encrypted space over the API
// (ADR-0049, ADR-0103): fetch the space's wrapped copies and its current key
// epoch, pick the copy sealed for THIS device's custody backend, unwrap it, and
// unroll the space's key history so content sealed under every earlier key
// stays readable. Every device-side reader — the CLI's space and vault
// commands, the device gateway — opens a space here, so none of them can open
// a rotated space with only its newest key.
package spaceopen

import (
	"context"
	"fmt"

	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
)

// Open opens spaceID for cust and returns a manager holding its keyring. found
// is false (with no error) when the space holds no copy wrapped for this device —
// the ADR-0049 confidentiality gate, reached before any content is fetched. A
// copy at an epoch other than the space's current one is an error: the peer
// drops superseded copies when a rotation lands, so seeing one means the peer
// and this device disagree about the space, and opening anyway would read the
// wrong chain.
func Open(ctx context.Context, c *apiclient.Client, cust client.Custody, spaceID string) (mgr *client.Manager, found bool, err error) {
	mine := cust.RecipientID()
	keys, err := c.SpaceKeys(ctx, spaceID)
	if err != nil {
		return nil, false, err
	}
	var own *apiclient.WrappedKey
	for i := range keys.WrappedKeys {
		if keys.WrappedKeys[i].Recipient == mine {
			own = &keys.WrappedKeys[i]
			break
		}
	}
	if own == nil {
		return nil, false, nil
	}
	if own.Epoch != keys.KeyEpoch {
		return nil, true, fmt.Errorf("space %s: this device's copy of the key seals epoch %d but the space is at epoch %d — "+
			"the peer should have dropped it; refusing to open with a superseded key (ADR-0103)", spaceID, own.Epoch, keys.KeyEpoch)
	}
	history, err := History(ctx, c, spaceID, keys.KeyEpoch)
	if err != nil {
		return nil, true, err
	}
	mgr = client.New()
	if err := mgr.OpenWithHistory(spaceID, own.Wrapped, keys.KeyEpoch, history, cust); err != nil {
		return nil, true, err
	}
	return mgr, true, nil
}

// History fetches a space's key chain as client history entries, up to and
// including epoch. A space at epoch 0 has none, and is not asked. Rows above
// epoch are dropped: the keys and the history are two reads, so a rotation that
// commits between them adds a row for an epoch this device's wrap does not open
// yet, and the chain up to epoch is still complete without it.
func History(ctx context.Context, c *apiclient.Client, spaceID string, epoch int) ([]client.HistoryEntry, error) {
	if epoch == 0 {
		return nil, nil
	}
	rows, err := c.KeyHistory(ctx, spaceID)
	if err != nil {
		return nil, fmt.Errorf("space %s: reading its key history: %w", spaceID, err)
	}
	out := make([]client.HistoryEntry, 0, len(rows))
	for _, r := range rows {
		if r.Epoch > epoch {
			continue
		}
		out = append(out, client.HistoryEntry{Epoch: r.Epoch, SealedPrev: r.SealedPrev})
	}
	return out, nil
}
