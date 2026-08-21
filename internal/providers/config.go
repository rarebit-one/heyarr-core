package providers

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// Kind is which PROTOCOL an entry speaks — not which product it is.
//
// Separate from Capability because they answer different questions. A KIND
// decides which client code runs. A CAPABILITY is what it does for us, and is
// what routing matches on. A service that both indexed and downloaded would be
// one kind with two capabilities, and merging the concepts would make that
// unrepresentable.
//
// # Protocols, not products (ADR-0028)
//
// `torznab`, not `prowlarr`. Heyarr binds to the documented interface, so
// Prowlarr is one implementation of it and Jackett is another — and a tracker
// exposing Torznab natively needs no integration at all.
//
// A product name here would look harmless and would be wrong in a way that
// compounds: it puts a vendor in the configuration an operator writes, in the
// fixture corpus directory, and in the client package name, and undoing it
// later means a migration plus every deployment's config. `transmission` is
// the honest exception — the RPC it speaks has no name of its own and no
// second implementation.
type Kind string

const (
	// KindTorznab is an indexer speaking Torznab (§59, ADR-0028). Prowlarr and
	// Jackett both serve it; so do some trackers directly.
	KindTorznab Kind = "torznab"
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
func Kinds() []Kind { return []Kind{KindTorznab, KindTransmission, KindFake} }

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
	case KindTorznab:
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
func needsCredential(k Kind) bool { return k == KindTorznab }

// Offer is a canned answer a fake indexer gives to one search title.
type Offer struct {
	// Title is the work title this answers for, matched case-insensitively.
	Title string `koanf:"title"`
	// Candidates is what to return.
	Candidates []OfferedCandidate `koanf:"candidates"`
}

// OfferedCandidate is one release a fake offers.
type OfferedCandidate struct {
	// ID is the provider's identity for the release. Required: it is the
	// evaluator's tie-break, and a candidate without one cannot be ranked
	// deterministically.
	ID    string `koanf:"id"`
	Title string `koanf:"title"`
	// Attributes is what is known about it, keyed by §62's attribute names.
	//
	// A key that is ABSENT means "could not be determined", exactly as it does
	// for a real provider — so a fake can exercise §63's `undetermined` path,
	// which is most of how a degraded node behaves and is otherwise
	// unreachable without a real indexer that omits a field.
	Attributes map[string]any `koanf:"attributes"`
}

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
	// PathMap translates the download client's filesystem namespace into
	// Heyarr's. Meaningful only for a download provider.
	//
	// It lives on the PROVIDER rather than in a global table keyed by host,
	// which is where the *arr stack puts it. Keying by host breaks the moment
	// a client is reachable by two names, and putting it on a different
	// settings page from the client it describes means the two drift. §59
	// centralises provider configuration precisely to stop that: the mapping
	// is part of how you reach this provider.
	PathMap []PathMapping `koanf:"path_map"`

	// Label tags this instance's transfers in a shared download client, so
	// Heyarr can tell its own from the operator's — and from a second Heyarr's.
	// Empty means the client's default.
	Label string `koanf:"label"`

	// Offers is what a FAKE indexer answers with, and is meaningless for
	// anything else.
	//
	// It exists because the milestone's central claim — Heyarr decides what
	// should exist and explains its choice, with no real indexer present — is
	// only demonstrable end to end if the fake can be given something to
	// offer. A fake that always answers nothing proves the empty-search edge
	// and nothing beyond it.
	//
	// It is configuration rather than a test fixture for the same reason Fake
	// itself is production code: the acceptance demo drives the real binary
	// over a real socket, so anything it needs has to be reachable from a
	// config file. Validation refuses it on a non-fake, so it cannot quietly
	// become a way to fabricate results from a real provider.
	Offers []Offer `koanf:"offers"`

	// Enabled allows an operator to keep a provider's configuration while
	// taking it out of routing. Defaults to true, so the common case needs no
	// key — and a disabled provider is still REPORTED, because "why is nothing
	// searching" should be answerable from GET /api/v1/providers rather than by
	// re-reading the config file.
	Enabled *bool `koanf:"enabled"`
}

// PathMapping is one prefix substitution, as configuration writes it.
//
// Declared here rather than imported from internal/downloads because
// internal/downloads imports THIS package for the Provider contract, and the
// other direction would be a cycle. The download client converts it into its
// own ordered form.
type PathMapping struct {
	Remote string `koanf:"remote"`
	Local  string `koanf:"local"`
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
	PathMap      []PathMapping
	Label        string
	Enabled      bool
	// Offers is a fake indexer's canned answers, already converted into domain
	// candidates so nothing downstream has to parse configuration.
	Offers map[string][]acquisition.ReleaseCandidate
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

		if err := validatePathMap(name, e.PathMap); err != nil {
			return nil, err
		}

		offers, err := resolveOffers(name, kind, e.Offers)
		if err != nil {
			return nil, err
		}

		out = append(out, Resolved{
			Name:         name,
			Kind:         kind,
			Endpoint:     endpoint,
			APIKey:       e.APIKey,
			Capabilities: caps,
			PathMap:      e.PathMap,
			Label:        strings.TrimSpace(e.Label),
			Enabled:      enabled,
			Offers:       offers,
		})
	}
	return out, nil
}

// resolveOffers turns a fake's canned answers into domain candidates.
//
// It refuses `offers` on anything that is not a fake, which is the point of
// validating it at all: without that, `offers` on a real Prowlarr entry would
// be silently ignored, and an operator would be looking at a config file that
// says something Heyarr does not do. Worse, it would read as a way to
// fabricate results from a real provider.
func resolveOffers(
	name string, kind Kind, offers []Offer,
) (map[string][]acquisition.ReleaseCandidate, error) {
	if len(offers) == 0 {
		return nil, nil
	}
	if kind != KindFake {
		return nil, fmt.Errorf(
			"provider %q: offers is only meaningful for a fake provider, not a %s — "+
				"a real provider answers with what its service returned", name, kind)
	}

	out := make(map[string][]acquisition.ReleaseCandidate, len(offers))
	for i, o := range offers {
		title := strings.TrimSpace(o.Title)
		if title == "" {
			return nil, fmt.Errorf("provider %q: offers[%d] has no title to answer for", name, i)
		}
		key := strings.ToLower(title)
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("provider %q: offers %q twice", name, title)
		}
		candidates := make([]acquisition.ReleaseCandidate, 0, len(o.Candidates))
		for j, c := range o.Candidates {
			id := strings.TrimSpace(c.ID)
			if id == "" {
				// The evaluator's tie-break. A candidate without one cannot be
				// ranked deterministically, and a fake that produced one would
				// be teaching the demo a shape a real provider is refused for.
				return nil, fmt.Errorf(
					"provider %q: offers[%d].candidates[%d] has no id, which is the "+
						"ranking's tie-break", name, i, j)
			}
			attrs, err := parseOfferedAttributes(c.Attributes)
			if err != nil {
				return nil, fmt.Errorf("provider %q: offers[%d].candidates[%d]: %w",
					name, i, j, err)
			}
			candidates = append(candidates, acquisition.ReleaseCandidate{
				ID: id, Title: strings.TrimSpace(c.Title), Provider: name, Attributes: attrs,
			})
		}
		out[key] = candidates
	}
	return out, nil
}

// parseOfferedAttributes converts YAML scalars into policy values.
//
// It routes through JSON deliberately: policy.Value already infers its kind
// from a JSON scalar, and that inference is what makes `"1080"` a caught typo
// rather than a working rule. A second, YAML-shaped conversion here would be a
// second place for the two to disagree.
func parseOfferedAttributes(in map[string]any) (acquisition.Attributes, error) {
	if len(in) == 0 {
		return acquisition.Attributes{}, nil
	}
	out := make(acquisition.Attributes, len(in))
	for rawName, rawValue := range in {
		attr := policy.Attribute(strings.ToLower(strings.TrimSpace(rawName)))
		kind, known := policy.KindOf(attr)
		if !known {
			return nil, fmt.Errorf("there is no attribute called %q", rawName)
		}
		encoded, err := json.Marshal(rawValue)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rawName, err)
		}
		var v policy.Value
		if err := json.Unmarshal(encoded, &v); err != nil {
			return nil, fmt.Errorf("%s: %w", rawName, err)
		}
		if v.Kind != kind {
			return nil, fmt.Errorf("%s is a %s attribute and was given %s",
				rawName, kind, v.String())
		}
		out[attr] = v
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

// validatePathMap refuses a mapping that cannot work.
//
// Shape only — the ordering and the resolution live in internal/downloads,
// which owns what a mapping MEANS. What is checked here is what configuration
// can be wrong about: an empty side, or a relative path where an absolute one
// is required. Every one of those would otherwise surface as "ingest cannot
// find the file" hours later, looking like an ingest fault.
func validatePathMap(name string, maps []PathMapping) error {
	seen := map[string]bool{}
	for i, m := range maps {
		remote := strings.TrimSpace(m.Remote)
		local := strings.TrimSpace(m.Local)
		switch {
		case remote == "":
			return fmt.Errorf("provider %q: path_map[%d] has no remote prefix", name, i)
		case local == "":
			return fmt.Errorf("provider %q: path_map[%d] has no local prefix", name, i)
		case !strings.HasPrefix(remote, "/"):
			return fmt.Errorf(
				"provider %q: path_map[%d] remote %q must be absolute — it is a prefix "+
					"of what the download client reports, and that is absolute", name, i, remote)
		case !strings.HasPrefix(local, "/"):
			return fmt.Errorf(
				"provider %q: path_map[%d] local %q must be absolute", name, i, local)
		case seen[remote]:
			// One of them would never apply, and which is an accident of
			// ordering.
			return fmt.Errorf("provider %q: path_map maps %q twice", name, remote)
		}
		seen[remote] = true
	}
	return nil
}

// Sorted returns entries by name, for rendering.
func Sorted(rs []Resolved) []Resolved {
	out := append([]Resolved(nil), rs...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
