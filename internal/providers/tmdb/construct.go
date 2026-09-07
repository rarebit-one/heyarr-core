package tmdb

import (
	"time"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Constructor builds the TMDB feed adapter for providers.BuildWith.
//
// It lives here rather than in the registry for the same reason TVDB's does:
// internal/providers cannot import this package, because this one imports IT for
// the Provider, FeedProvider and DiscoverySearcher contracts. The cycle is the
// interface boundary working, and the injected constructor is how the two are
// wired by whoever owns both — the worker and the controller.
//
// Returning handled=false for any other kind means it composes in a Chain beside
// the indexer, download, TVDB and other feed constructors, and an unrecognised
// kind still falls through to the registry's honest "configured, not
// implemented" report.
func Constructor(r providers.Resolved, now func() time.Time) (providers.Provider, bool, error) {
	if r.Kind != providers.KindTMDB {
		return nil, false, nil
	}

	endpoint := ""
	if r.Endpoint != nil {
		endpoint = r.Endpoint.String()
	}

	// TMDB's declared auth scheme is a single opaque token (ADR-0031) — a v4 read
	// access token this client sends as a bearer header against the v3 endpoints
	// — so Token() is the accessor that fits it and the only one that will
	// answer. The credential is revealed exactly here, at the point it is handed
	// to the client that must send it; Reveal() greps cleanly, which is the whole
	// argument for the Secret type.
	token, _ := r.Credential.Token()

	client, err := New(Options{
		Name:     r.Name,
		Endpoint: endpoint,
		Token:    token.Reveal(),
		Now:      now,
	})
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}
