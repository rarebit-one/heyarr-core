package tvdb

import (
	"time"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Constructor builds the TVDB feed adapter for providers.BuildWith.
//
// It lives here rather than in the registry for the same reason the indexer's
// and download client's do: internal/providers cannot import this package,
// because this one imports IT for the Provider and FeedProvider contracts. The
// cycle is the interface boundary working, and the injected constructor is how
// the two are wired by whoever owns both — the worker and the controller.
//
// Returning handled=false for any other kind means it composes in a Chain beside
// the indexer and download constructors, and an unrecognised kind still falls
// through to the registry's honest "configured, not implemented" report.
func Constructor(r providers.Resolved, now func() time.Time) (providers.Provider, bool, error) {
	if r.Kind != providers.KindTVDB {
		return nil, false, nil
	}

	endpoint := ""
	if r.Endpoint != nil {
		endpoint = r.Endpoint.String()
	}

	// TVDB's declared auth scheme is a single opaque token (ADR-0031, the API
	// key it exchanges for a bearer), so Token() is the accessor that fits it and
	// the only one that will answer. The credential is revealed exactly here, at
	// the point it is handed to the client that must send it — Reveal() greps
	// cleanly, which is the whole argument for the Secret type.
	token, _ := r.Credential.Token()

	client, err := New(Options{
		Name:     r.Name,
		Endpoint: endpoint,
		APIKey:   token.Reveal(),
		Now:      now,
	})
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}
