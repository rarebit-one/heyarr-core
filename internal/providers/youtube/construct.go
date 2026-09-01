package youtube

import (
	"time"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Constructor builds the youtube feed adapter for providers.BuildWith.
//
// It lives here rather than in the registry for the same reason the TVDB,
// indexer, download and podcast constructors do: internal/providers cannot
// import this package, because this one imports IT for the Provider and
// FeedProvider contracts. The injected constructor is how the two are wired by
// whoever owns both — the worker and the controller.
//
// Returning handled=false for any other kind means it composes in a Chain beside
// the other constructors, and an unrecognised kind still falls through to the
// registry's honest "configured, not implemented" report.
func Constructor(r providers.Resolved, now func() time.Time) (providers.Provider, bool, error) {
	if r.Kind != providers.KindYoutube {
		return nil, false, nil
	}
	// No endpoint and no credential: a channel feed's address is the followed
	// source's own FeedRef, handed to Enumerate per poll, and a public feed
	// authenticates nothing. So construction takes only the name and the clock.
	client, err := New(Options{Name: r.Name, Now: now})
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}
