package downloads

import (
	"time"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Constructor builds the download clients this package implements, for
// providers.BuildWith.
//
// It lives here rather than in the registry because internal/providers cannot
// import this package — this one imports IT, for the Provider contract. That
// cycle is the interface boundary working rather than an accident of layering,
// and the injected constructor is how the two are wired together by whoever
// owns both: the worker and the controller.
//
// Returning handled=false for a kind it does not implement means several
// constructors compose, and an unrecognised kind still falls through to the
// registry's honest "configured, not implemented" report.
func Constructor(r providers.Resolved, now func() time.Time) (providers.Provider, bool, error) {
	if r.Kind != providers.KindTransmission {
		return nil, false, nil
	}

	// Configuration validated the SHAPE of the mapping; this converts it into
	// the ordered form and is where a mapping that is wrong in a way only this
	// package knows about would be caught. Both checks exist because
	// configuration cannot import this package either.
	maps := make([]Mapping, 0, len(r.PathMap))
	for _, m := range r.PathMap {
		maps = append(maps, Mapping{Remote: m.Remote, Local: m.Local})
	}
	pathMap, err := ParsePathMap(r.Name, maps)
	if err != nil {
		return nil, true, err
	}

	endpoint := ""
	if r.Endpoint != nil {
		endpoint = r.Endpoint.String()
	}

	// The credential is revealed exactly here, at the point it is handed to
	// the thing that must send it. Reveal() greps cleanly, which is the whole
	// argument for the Secret type: every place a credential leaves its
	// wrapper is one line somebody can find.
	user, pass := credentialFor(r)
	client, err := New(Options{
		Name:         r.Name,
		Endpoint:     endpoint,
		Password:     pass,
		Username:     user,
		PathMap:      pathMap,
		Label:        r.Label,
		Capabilities: r.Capabilities,
		Now:          now,
	})
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}

// credentialFor splits the RPC credential.
//
// Transmission's RPC uses HTTP basic auth, which needs a username as well as a
// password — but a provider Entry has one credential field, because most
// services take one. Rather than adding a second field that only one kind uses,
// the username defaults to "transmission" and can be overridden by writing
// "user:pass" in api_key.
//
// This is a small ugliness and it is written down rather than hidden: the
// alternative is a config field that is empty for every provider but this one,
// which teaches every operator about a detail only one of them needs.
func credentialFor(r providers.Resolved) (user, pass string) {
	secret := r.APIKey.Reveal()
	if secret == "" {
		// Authentication off, which is an ordinary supported deployment on a
		// trusted network — providers.needsCredential says so.
		return "", ""
	}
	if u, p, found := splitCredential(secret); found {
		return u, p
	}
	return "transmission", secret
}

// splitCredential separates "user:pass" when a credential carries both.
func splitCredential(secret string) (user, pass string, found bool) {
	for i := range len(secret) {
		if secret[i] == ':' {
			return secret[:i], secret[i+1:], true
		}
	}
	return "", secret, false
}
