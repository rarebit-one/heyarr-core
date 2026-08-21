package indexers

import (
	"time"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Constructor builds the indexers this package implements, for
// providers.BuildWith.
//
// It lives here rather than in the registry for the same reason the download
// client's does: internal/providers cannot import this package, because this
// one imports IT for the Provider and Indexer contracts. The cycle is the
// interface boundary working rather than an accident of layering, and the
// injected constructor is how the two are wired by whoever owns both — the
// worker and the controller.
//
// Returning handled=false for a kind it does not implement means several
// constructors compose, and an unrecognised kind still falls through to the
// registry's honest "configured, not implemented" report.
func Constructor(r providers.Resolved, now func() time.Time) (providers.Provider, bool, error) {
	if r.Kind != providers.KindTorznab {
		return nil, false, nil
	}

	endpoint := ""
	if r.Endpoint != nil {
		endpoint = r.Endpoint.String()
	}

	// The credential is revealed exactly here, at the point it is handed to
	// the thing that must send it. Reveal() greps cleanly, which is the whole
	// argument for the Secret type: every place a credential leaves its
	// wrapper is one line somebody can find.
	client, err := New(Options{
		Name:         r.Name,
		Endpoint:     endpoint,
		APIKey:       r.APIKey.Reveal(),
		Capabilities: r.Capabilities,
		Now:          now,
	})
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}
