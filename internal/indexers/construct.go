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
	// Torznab and Newznab are one wire protocol (ADR-0028): the same client
	// serves both, because Torznab is Newznab plus a torrent extension and this
	// package implements the protocol, not the product. A newznab-kind provider
	// is a usenet indexer whose releases carry an .nzb source rather than a
	// magnet — a distinction that lives in the release, not in the search.
	if r.Kind != providers.KindTorznab && r.Kind != providers.KindNewznab {
		return nil, false, nil
	}

	endpoint := ""
	if r.Endpoint != nil {
		endpoint = r.Endpoint.String()
	}

	// Torznab's declared auth scheme is a single opaque token (ADR-0031), so
	// Token() is the accessor that fits it — and it is the only one that will
	// answer, which is what stops a basic credential's password being sent as
	// an apikey query parameter.
	//
	// A provider validated as torznab always resolves to a token credential,
	// so the false branch is unreachable through Constructor's kind check; an
	// empty key is what a hand-built Resolved would deserve, and the request
	// then fails with a 401 rather than with half of somebody else's password
	// on the wire.
	token, _ := r.Credential.Token()

	// The credential is revealed exactly here, at the point it is handed to
	// the thing that must send it. Reveal() greps cleanly, which is the whole
	// argument for the Secret type: every place a credential leaves its
	// wrapper is one line somebody can find.
	client, err := New(Options{
		Name:         r.Name,
		Endpoint:     endpoint,
		APIKey:       token.Reveal(),
		Capabilities: r.Capabilities,
		Now:          now,
	})
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}
