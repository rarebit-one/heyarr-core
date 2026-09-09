package opensubtitles

import (
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Constructor builds the OpenSubtitles adapter for providers.BuildWith.
//
// It lives here rather than in the registry for the reason tvdb's and tmdb's do:
// internal/providers cannot import this package, because this one imports IT for
// the Provider and SubtitleProvider contracts. The injected constructor is how
// the worker and the controller — which own both — wire the two, and returning
// handled=false for any other kind lets it compose in a Chain beside the indexer,
// download and feed constructors, an unrecognised kind still falling through to
// the registry's honest "configured, not implemented" report.
func Constructor(r providers.Resolved, now func() time.Time) (providers.Provider, bool, error) {
	if r.Kind != providers.KindOpenSubtitles {
		return nil, false, nil
	}

	endpoint := ""
	if r.Endpoint != nil {
		endpoint = r.Endpoint.String()
	}

	// OpenSubtitles' declared scheme is AuthTokenBasic (ADR-0031, ADR-0085): an
	// api-key that authenticates a search AND a username+password that mint the
	// download token. TokenBasic() is the accessor that fits it and the only one
	// that will answer. The three secrets are revealed exactly here, at the point
	// they are handed to the client that must send them; Reveal() greps cleanly,
	// which is the whole argument for the Secret type.
	apiKey, username, password, _ := r.Credential.TokenBasic()

	client, err := New(Options{
		Name:      r.Name,
		Endpoint:  endpoint,
		APIKey:    apiKey.Reveal(),
		Username:  username,
		Password:  password.Reveal(),
		UserAgent: fmt.Sprintf("heyarr v%s", buildinfo.Version),
		Now:       now,
	})
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}
