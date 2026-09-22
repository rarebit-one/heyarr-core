package musicbrainz

import (
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Constructor builds the MusicBrainz enrich adapter for providers.BuildWith.
//
// It lives here rather than in the registry for the reason tmdb's and
// opensubtitles' do: internal/providers cannot import this package, because this
// one imports IT for the Provider and EnrichProvider contracts. Returning
// handled=false for any other kind lets it compose in a Chain beside the other
// constructors.
//
// MusicBrainz and the Cover Art Archive are KEYLESS (ADR-0087): there is no
// credential to reveal. MusicBrainz REQUIRES a descriptive User-Agent, so this
// constructor supplies one identifying heyarr and its version — the one piece of
// configuration a keyless enrich provider carries.
func Constructor(r providers.Resolved, now func() time.Time) (providers.Provider, bool, error) {
	if r.Kind != providers.KindMusicBrainz {
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
