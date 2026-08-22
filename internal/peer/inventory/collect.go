package inventory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// Store is the half of a content store an inventory needs: what is on disk.
//
// It is an interface here rather than a *cas.FS because the collector's
// contract is "walk what is addressable", and nothing about it needs the
// filesystem. A remote or object-backed store answering the same question
// answers it honestly.
type Store interface {
	Walk(ctx context.Context, fn func(cas.Descriptor) error) error
}

// Quarantine lists blobs the store has moved aside because their bytes stopped
// matching their own name (ADR-0018).
//
// Separate from Store because it is a question about a local layout. A store
// with no quarantine has no honest answer, and the right way to say so is to
// not implement this rather than to return an empty list — an inventory that
// silently reported "nothing is corrupt" from a store that cannot tell would
// be the most expensive kind of wrong.
type Quarantine interface {
	QuarantinedBlobs() ([]cas.Quarantined, error)
}

// Options configure a Collect.
type Options struct {
	// Store is what this peer holds. Required.
	Store Store
	// Quarantine is what this peer holds and cannot serve. Optional: a peer
	// whose store cannot quarantine reports nothing corrupt, which is true of
	// that store.
	Quarantine Quarantine
	// Verified answers "when did this peer last confirm these bytes hash to
	// their own name", for the blobs it can answer for.
	//
	// Optional, and nil is not a defect. Collecting an inventory reads a
	// directory; it does not re-hash a library. A peer with no verification
	// record reports no verification time rather than inventing one from the
	// collection clock — see Entry.VerifiedAt.
	Verified func(hashing.Hash) (time.Time, bool)
	// Now is injected so an observation's timestamp is testable (ADR-0017).
	Now func() time.Time
}

// Collect observes what this peer's store actually holds.
//
// It reads the STORE, never a catalog. A collector that read the peer's own
// catalog would report the controller's beliefs back to the controller, and
// the exchange would confirm nothing it did not already assume — which is the
// precise failure this issue exists to prevent.
//
// Quarantined blobs are included as `corrupt`. They are the case an inventory
// derived from "which files are addressable" gets wrong on its own: the peer
// still HAS those bytes and cannot serve them, and both "present" and
// "absent" are lies about that.
func Collect(ctx context.Context, opts Options) (Snapshot, error) {
	if opts.Store == nil {
		return Snapshot{}, errors.New("inventory: a content store is required — " +
			"an inventory is what is on disk, and there is no other place to read it from")
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	observedAt := now().UTC()

	byHash := map[string]Entry{}
	if err := opts.Store.Walk(ctx, func(d cas.Descriptor) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := Entry{
			BlobHash:     d.Hash.String(),
			State:        StatePresent,
			BytesPresent: d.Size,
		}
		if opts.Verified != nil {
			if at, ok := opts.Verified(d.Hash); ok {
				at = at.UTC()
				e.VerifiedAt = &at
			}
		}
		byHash[e.BlobHash] = e
		return nil
	}); err != nil {
		return Snapshot{}, fmt.Errorf("inventory: walking the content store: %w", err)
	}

	if opts.Quarantine != nil {
		quarantined, err := opts.Quarantine.QuarantinedBlobs()
		if err != nil {
			return Snapshot{}, fmt.Errorf("inventory: listing quarantined blobs: %w", err)
		}
		for _, q := range quarantined {
			hash := q.Hash.String()
			if _, addressable := byHash[hash]; addressable {
				// The bytes were re-acquired after the corrupt copy was moved
				// aside, and the addressable copy is the one that can be
				// served. Quarantine keeps the evidence; it does not keep the
				// blob broken.
				continue
			}
			byHash[hash] = Entry{BlobHash: hash, State: StateCorrupt, BytesPresent: 0}
		}
	}

	return Snapshot{ObservedAt: observedAt, byHash: byHash}, nil
}
