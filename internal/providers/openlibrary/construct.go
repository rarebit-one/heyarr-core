package openlibrary

import (
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Constructor builds the Open Library enrich adapter for providers.BuildWith.
//
// It lives here rather than in the registry for the reason tmdb's and
// opensubtitles' do: internal/providers cannot import this package, because this
// one imports IT for the Provider and EnrichProvider contracts. The injected
// constructor is how the worker and the controller — which own both — wire the
// two, and returning handled=false for any other kind lets it compose in a Chain
// beside the indexer, download, feed and subtitle constructors.
//
// Open Library is KEYLESS (ADR-0087): there is no credential to reveal, so unlike
// the video and subtitle constructors this one reads none. The only configured
// value is an optional endpoint (tests point it at a fixture server) and a
// User-Agent, defaulted to a descriptive one Open Library can attribute traffic
// to.
func Constructor(r providers.Resolved, now func() time.Time) (providers.Provider, bool, error) {
	if r.Kind != providers.KindOpenLibrary {
		return nil, false, nil
	}

	endpoint := ""
	if r.Endpoint != nil {
		endpoint = r.Endpoint.String()
	}

	client, err := New(Options{
		Name:      r.Name,
		Endpoint:  endpoint,
		UserAgent: fmt.Sprintf("heyarr/%s ( +https://github.com/rarebit-one/heyarr-core )", buildinfo.Version),
		Now:       now,
	})
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}
