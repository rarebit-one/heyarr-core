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

	// The credential comes out of its wrapper in credentialFor, which is the
	// one place in this package where it does.
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

// credentialFor is where the RPC credential leaves its wrapper.
//
// # This used to be a parser, and that was the bug
//
// Transmission's RPC is HTTP basic auth, so it needs a username as well as a
// password. Before ADR-0031 a provider Entry carried one credential field, so
// #102 packed both into it as "user:pass" with the username defaulting to
// "transmission" — and split on the first colon to get them back out.
//
// A password containing a colon was therefore silently cut in half: Heyarr
// authenticated as the wrong user with a truncated password, got a 401, and
// reported an unreachable download client. The configuration was correct and
// nothing said otherwise.
//
// The credential is now TYPED by the provider's declared auth scheme, so there
// is nothing here to parse. Basic() either yields the pair the operator wrote,
// byte for byte, or reports that this provider does not have a basic
// credential at all.
func credentialFor(r providers.Resolved) (user, pass string) {
	username, password, ok := r.Credential.Basic()
	if !ok {
		// A Transmission entry always resolves to a basic credential, so this
		// is unreachable through Constructor's kind check. It is not an error
		// because a wrong-shaped credential is a configuration mistake that
		// providers.Validate already refuses at startup; reaching here would
		// mean something built a Resolved by hand, and the safe reading of a
		// credential we cannot use is "there is none".
		return "", ""
	}
	// The credential is revealed exactly here, at the point it is handed to
	// the thing that must send it. Reveal() greps cleanly, which is the whole
	// argument for the Secret type.
	return username, password.Reveal()
}
