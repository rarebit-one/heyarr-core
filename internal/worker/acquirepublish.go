package worker

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// AcquirePublisher publishes the release a grab handed to a download client as
// an acquire op on the catalog op-log (ADR-0119), so a sibling with fan-out
// enabled acquires the same bytes itself. A nil publisher publishes nothing:
// that is a single-site node, or one without an identity key, and the grab is
// unaffected either way.
type AcquirePublisher interface {
	// Publish records the op for the want. candidateID is the release the
	// grab handed to the client, and transferID is what the client called the
	// transfer: for a torrent client it is the infohash, which is the
	// identity the op pins. A transfer that is not a torrent (an HTTP
	// enclosure, a usenet article) has nothing a sibling could pin and is
	// skipped without error. The facts published are those of candidateID,
	// never of whatever is selected by the time the op is written.
	Publish(ctx context.Context, desiredItemID, candidateID, transferID string) error
}

// ErrSelectionChanged reports that the want's selected release is no longer
// the one the grab handed to the client: a by-hand override landed between
// the grab's SelectedSource and the publish. The grab must not advance the
// want on the old transfer's behalf; the grab fails and its retry takes the current one.
//
// It is catalog.ErrSelectionChanged, so the grab treats the publisher's check
// and the guarded transition's alike.
var ErrSelectionChanged = catalog.ErrSelectionChanged

// AcquireFactsReader is the catalog read the publisher needs.
type AcquireFactsReader interface {
	AcquireFacts(ctx context.Context, desiredItemID string) (catalog.AcquireFacts, error)
}

// AcquireOpStore is the op-log store the publisher appends to: the catalog
// tombstone store satisfies it.
type AcquireOpStore interface {
	Heads(ctx context.Context) ([]string, error)
	RecordOps(ctx context.Context, ops []string) error
}

// CatalogAcquirePublisher is the AcquirePublisher a two-site media node runs:
// it signs the op under this peer's identity (ADR-0012) and appends it to the
// same log the delete tombstones ride (ADR-0073); catalogsync carries it.
type CatalogAcquirePublisher struct {
	Facts  AcquireFactsReader
	Ops    AcquireOpStore
	Signer ed25519.PrivateKey
	// Site is this node's site name (peer.site); the op names it as the origin.
	Site string
	Now  func() time.Time
	Log  *slog.Logger
}

// Publish implements AcquirePublisher.
func (p *CatalogAcquirePublisher) Publish(ctx context.Context, desiredItemID, candidateID, transferID string) error {
	if p == nil || p.Facts == nil || p.Ops == nil || len(p.Signer) != ed25519.PrivateKeySize {
		return nil
	}
	infohash := infohashOf(transferID)
	if infohash == "" {
		// Not a torrent: an HTTP enclosure or a usenet article. There is no
		// swarm identity a sibling could join, so nothing to publish. Direct-
		// route content converges per site through its followed source.
		return nil
	}
	facts, err := p.Facts.AcquireFacts(ctx, desiredItemID)
	if errors.Is(err, catalog.ErrNoSelection) {
		// The selection went away between the grab and now; the op would name
		// a release the want no longer wants. Nothing to publish.
		return nil
	}
	if err != nil {
		return err
	}
	// The facts must be the grabbed release's. A by-hand override that lands
	// between the grab's SelectedSource and this read swaps the selected row
	// under us; publishing its title, privacy and selected-at with the old
	// transfer's infohash would tell siblings to fetch a release the origin
	// did not choose. Skip: the override enqueues its own grab, which
	// publishes its own op.
	if candidateID != "" && facts.CandidateID != candidateID {
		return fmt.Errorf("%w: grabbed %s, now selected %s", ErrSelectionChanged, candidateID, facts.CandidateID)
	}
	// A candidate whose id is the infohash must agree with the client's: a
	// disagreement means the client took a different release than the one
	// selected (a .torrent that resolved elsewhere), and publishing the
	// client's hash would tell a sibling to fetch bytes this site did not
	// choose. Refuse rather than guess.
	if cid, ok := strings.CutPrefix(facts.CandidateID, "infohash:"); ok && !strings.EqualFold(cid, infohash) {
		return fmt.Errorf("worker: want %s selected %s but the client took %s; not publishing", desiredItemID, facts.CandidateID, infohash)
	}
	site := clipBytes(p.Site, catalogop.MaxSite)
	if site == "" {
		site = "unnamed"
	}
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	// The op's selected-at is the PERSISTED selection time, not now: a grab
	// re-run after a crash between RecordOps and the transition republishes
	// the same selection with the same time, so it can never outrank a
	// genuinely newer choice made elsewhere in the meantime.
	selectedAt := facts.SelectedAt
	if selectedAt.IsZero() {
		selectedAt = now
	}
	heads, err := p.Ops.Heads(ctx)
	if err != nil {
		return fmt.Errorf("worker: reading catalog-op heads: %w", err)
	}
	// Display-only fields are clipped to the op's limits rather than refused:
	// the client already holds the transfer and the row is written, so a
	// refusal here would retry forever and leave the want SELECTED (invariant
	// 9 cuts both ways). The profile and site names are identities a sibling
	// matches on; clipping one only costs the sibling its profile fallback.
	token, err := catalogop.SignAcquire(p.Signer, facts.ContentType, facts.WorkKey, catalogop.Acquire{
		Scope:      facts.Scope,
		Aspect:     facts.Aspect,
		Profile:    clipBytes(facts.Profile, catalogop.MaxProfile),
		Infohash:   infohash,
		Title:      clipBytes(facts.CandidateTitle, catalogop.MaxTitle),
		WorkTitle:  clipBytes(facts.WorkTitle, catalogop.MaxTitle),
		Year:       facts.Year,
		Size:       facts.Size,
		SelectedAt: selectedAt,
		Site:       site,
		// Trackers are never published in this increment: the sibling joins
		// the swarm by infohash (DHT), or resolves a private release through
		// its own indexers. A tracker URL from the source would risk a passkey.
		Trackers: nil,
		Private:  facts.Private,
	}, heads, now)
	if errors.Is(err, catalogop.ErrMalformed) {
		// The want cannot be said as an op: a scope key past MaxScope (an item
		// key a source supplied, unbounded here), a profile name the clip left
		// empty. That is a property of the want, so a retry would fail the same
		// way while the client already holds the transfer and the want rests in
		// SELECTED. Publish nothing and let the grab advance; a sibling acquires
		// this one through its own search.
		if p.Log != nil {
			p.Log.Warn("not publishing a grabbed release: the want cannot be expressed as an acquire op",
				"desired_item_id", desiredItemID, "work_key", facts.WorkKey, "error", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("worker: signing acquire op for %s: %w", desiredItemID, err)
	}
	if err := p.Ops.RecordOps(ctx, []string{token}); err != nil {
		return fmt.Errorf("worker: recording acquire op for %s: %w", desiredItemID, err)
	}
	if p.Log != nil {
		p.Log.Info("published the chosen release for siblings to acquire",
			"desired_item_id", desiredItemID, "work_key", facts.WorkKey, "infohash", infohash)
	}
	return nil
}

// infohashOf returns the lower-case infohash a transfer id carries, or "" when
// the id is not one (40 or 64 hex characters, case-insensitive).
func infohashOf(transferID string) string {
	s := strings.ToLower(strings.TrimSpace(transferID))
	if len(s) != 40 && len(s) != 64 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return s
}

// clipBytes truncates s to at most max bytes on a rune boundary.
func clipBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
