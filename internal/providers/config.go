package providers

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Kind is which service an entry configures.
//
// Separate from Capability because they answer different questions. A KIND is
// what software this is — the thing that decides which client code runs. A
// CAPABILITY is what it does for us — the thing routing matches on. Prowlarr
// is one kind with one capability today; a service that both indexed and
// downloaded would be one kind with two, and merging the two concepts would
// make that unrepresentable.
type Kind string

const (
	// KindProwlarr is the initial indexer (§59). Implemented in M3-09.
	KindProwlarr Kind = "prowlarr"
	// KindTransmission is the initial acquisition transport (§58).
	// Implemented in M3-10.
	KindTransmission Kind = "transmission"
	// KindFake is an in-process provider that talks to nothing.
	//
	// It is a first-class kind rather than a test-only construct because the
	// acceptance demo needs it: ADR-0026 says a real indexer can never run in
	// CI, so proving "Heyarr decides what should exist and explains its choice"
	// end to end requires something that behaves exactly like a provider and
	// needs no service. Making it a real kind means the demo exercises the same
	// registration, routing and health paths as production rather than a
	// parallel set that could rot.
	//
	// It is inert: it holds no credential, reaches nothing, and reports itself
	// healthy because there is nothing that could be unwell.
	KindFake Kind = "fake"
)

// Kinds lists every kind, in a stable order.
func Kinds() []Kind { return []Kind{KindProwlarr, KindTransmission, KindFake} }

// ParseKind validates a kind from configuration.
func ParseKind(s string) (Kind, error) {
	normalised := Kind(strings.ToLower(strings.TrimSpace(s)))
	for _, k := range Kinds() {
		if k == normalised {
			return k, nil
		}
	}
	names := make([]string, 0, len(Kinds()))
	for _, k := range Kinds() {
		names = append(names, string(k))
	}
	return "", fmt.Errorf("%q is not a provider type — it must be one of %s",
		s, strings.Join(names, ", "))
}

// DefaultCapabilities is what a kind can do when configuration does not say.
//
// Configuration MAY name capabilities explicitly, which is what lets a
// `metadata` provider be configured before any kind implements one. When it
// does not, the kind's defaults apply — because making every operator spell out
// that Prowlarr is an indexer is ceremony that teaches nothing.
func DefaultCapabilities(k Kind) []Capability {
	switch k {
	case KindProwlarr:
		return []Capability{CapabilityIndexer}
	case KindTransmission:
		return []Capability{CapabilityDownload}
	default:
		// A fake declares nothing by default: what it stands in for is the
		// whole of what a test or the demo is configuring, so it must say.
		return nil
	}
}

// needsEndpoint reports whether a kind must be told where the service is.
//
// The fake is the exception and the only one: it reaches nothing, so requiring
// an endpoint would be requiring a fiction.
func needsEndpoint(k Kind) bool { return k != KindFake }

// needsCredential reports whether a kind must be given one.
//
// Prowlarr authenticates every request with an API key, so a Prowlarr with no
// credential is a provider that will 401 on its first search — an hour later,
// looking like an indexer problem rather than a configuration one.
//
// Transmission does NOT: an operator running it on a trusted network with
// authentication off is an ordinary, supported deployment, and refusing to
// start would be Heyarr insisting on a policy the operator already declined.
func needsCredential(k Kind) bool { return k == KindProwlarr }

// Entry is one configured provider.
//
// The `koanf` tags mirror internal/config's convention so the block reads
// alongside `libraries:` in the same document.
type Entry struct {
	// Name is the operator's name, unique within an instance.
	Name string `koanf:"name"`
	// Type is which service this is.
	Type string `koanf:"type"`
	// Endpoint is where the service is. Required for everything but the fake.
	Endpoint string `koanf:"endpoint"`
	// APIKey is the credential, when the service takes one.
	//
	// It is a Secret, so it redacts in logs, errors and JSON. It arrives from
	// configuration as a plain string, which is why Secret has an
	// UnmarshalJSON — the redaction is on the way OUT, not the way in.
	APIKey Secret `koanf:"api_key"`
	// Capabilities overrides the kind's defaults. Empty means the defaults.
	Capabilities []string `koanf:"capabilities"`
	// Enabled allows an operator to keep a provider's configuration while
	// taking it out of routing. Defaults to true, so the common case needs no
	// key — and a disabled provider is still REPORTED, because "why is nothing
	// searching" should be answerable from GET /api/v1/providers rather than by
	// re-reading the config file.
	Enabled *bool `koanf:"enabled"`
}

// Resolved is an Entry that has been validated.
//
// A separate type so that "this came from configuration" and "this has been
// checked" are not the same value with a flag on it. Everything downstream
// takes a Resolved, so there is no path that skips validation.
type Resolved struct {
	Name         string
	Kind         Kind
	Endpoint     *url.URL
	APIKey       Secret
	Capabilities []Capability
	Enabled      bool
}

// Validate checks a configured provider block and returns what it resolved to.
//
// # This is the startup half of ADR-0025's asymmetry
//
// ADR-0023 made a configured-but-unresolvable BINARY a startup error, because
// somebody named that binary and silently using none is worse than not
// starting. The same reasoning applies to a provider whose configuration cannot
// possibly work: a malformed endpoint or a missing required credential is a
// typo, and a typo found at startup costs seconds where the same typo found at
// the first search costs an afternoon of looking at the wrong system.
//
// What is deliberately NOT checked here is whether anything answers. That
// inverts ADR-0023's asymmetry and it is the whole of ADR-0025: a download
// client that is down at 03:00 must not stop Heyarr from serving the library at
// 03:01. Reachability is checked continuously, by a job, and reported.
//
// Every message names the provider and the field, because an instance with six
// providers produces an error somebody has to act on without reading the source.
func Validate(entries []Entry) ([]Resolved, error) {
	out := make([]Resolved, 0, len(entries))
	seen := map[string]bool{}

	for i, e := range entries {
		name := strings.TrimSpace(e.Name)
		if name == "" {
			return nil, fmt.Errorf("providers[%d] has no name", i)
		}
		if seen[name] {
			// Two providers sharing a name make health, routing and a
			// candidate's Provider field ambiguous.
			return nil, fmt.Errorf("provider %q is configured twice", name)
		}
		seen[name] = true

		kind, err := ParseKind(e.Type)
		if err != nil {
			return nil, fmt.Errorf("provider %q: type: %w", name, err)
		}

		caps, err := resolveCapabilities(name, kind, e.Capabilities)
		if err != nil {
			return nil, err
		}

		endpoint, err := resolveEndpoint(name, kind, e.Endpoint)
		if err != nil {
			return nil, err
		}

		if needsCredential(kind) && e.APIKey.IsZero() {
			return nil, fmt.Errorf(
				"provider %q: api_key is required for a %s provider — "+
					"without it every request is refused, which looks like an indexer "+
					"fault hours later rather than a configuration one now",
				name, kind)
		}

		enabled := true
		if e.Enabled != nil {
			enabled = *e.Enabled
		}

		out = append(out, Resolved{
			Name:         name,
			Kind:         kind,
			Endpoint:     endpoint,
			APIKey:       e.APIKey,
			Capabilities: caps,
			Enabled:      enabled,
		})
	}
	return out, nil
}

func resolveCapabilities(name string, kind Kind, declared []string) ([]Capability, error) {
	if len(declared) == 0 {
		caps := DefaultCapabilities(kind)
		if len(caps) == 0 {
			return nil, fmt.Errorf(
				"provider %q: capabilities is required for a %s provider, "+
					"which has no default", name, kind)
		}
		return sortCapabilities(caps), nil
	}

	caps := make([]Capability, 0, len(declared))
	seen := map[Capability]bool{}
	for _, raw := range declared {
		c, err := ParseCapability(raw)
		if err != nil {
			return nil, fmt.Errorf("provider %q: capabilities: %w", name, err)
		}
		if seen[c] {
			continue
		}
		seen[c] = true
		caps = append(caps, c)
	}
	return sortCapabilities(caps), nil
}

// resolveEndpoint parses and sanity-checks the address of a service.
//
// Every failure here is a typo somebody can fix in ten seconds, and every one
// of them would otherwise surface as a runtime error in a background job.
func resolveEndpoint(name string, kind Kind, raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		if needsEndpoint(kind) {
			return nil, fmt.Errorf("provider %q: endpoint is required for a %s provider", name, kind)
		}
		return nil, nil //nolint:nilnil // a fake has no endpoint, and that is not an error
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("provider %q: endpoint %q is not a URL: %w", name, trimmed, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		// A bare "localhost:9091" parses without error and yields scheme
		// "localhost" — which is the single most common way to write this
		// wrong, and it produces a dial to nowhere rather than a parse failure.
		return nil, fmt.Errorf(
			"provider %q: endpoint %q must start with http:// or https:// — "+
				"a bare host:port parses as a URL scheme and dials nothing",
			name, trimmed)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("provider %q: endpoint %q names no host", name, trimmed)
	}
	if u.User != nil {
		// Credentials in a URL end up in logs, in error messages and in
		// process listings, and this package's whole redaction story assumes
		// they live in APIKey. Refuse rather than silently strip: an operator
		// whose credential vanished would have a provider that 401s for no
		// visible reason.
		return nil, fmt.Errorf(
			"provider %q: endpoint must not contain credentials — "+
				"put them in api_key, where they are kept out of logs and responses", name)
	}
	return u, nil
}

// Sorted returns entries by name, for rendering.
func Sorted(rs []Resolved) []Resolved {
	out := append([]Resolved(nil), rs...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
