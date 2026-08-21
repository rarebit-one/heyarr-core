package providers

import (
	"strings"
	"testing"
)

// Configuration validation — the startup half of ADR-0025's asymmetry.
//
// Every refusal here is a typo somebody can fix in ten seconds, and every one
// of them would otherwise surface hours later as a runtime failure in a
// background job, looking like a fault in the external service rather than in
// the configuration.
//
// What is deliberately NOT here is a reachability check. That is the whole of
// ADR-0025 and it has its own test below.

func ptr[T any](v T) *T { return &v }

func TestValidateRefusals(t *testing.T) {
	cases := []struct {
		name    string
		entries []Entry
		// wantErr is a fragment the message must contain. It always includes
		// the provider's name where there is one, because an instance with six
		// providers produces an error somebody has to act on.
		wantErr []string
	}{
		{
			name:    "no name",
			entries: []Entry{{Type: "torznab", Endpoint: "https://x.invalid", APIKey: "k"}},
			wantErr: []string{"providers[0]", "no name"},
		},
		{
			name: "the same name twice",
			entries: []Entry{
				{Name: "dup", Type: "torznab", Endpoint: "https://a.invalid", APIKey: "k"},
				{Name: "dup", Type: "torznab", Endpoint: "https://b.invalid", APIKey: "k"},
			},
			wantErr: []string{"dup", "configured twice"},
		},
		{
			name:    "a type that does not exist",
			entries: []Entry{{Name: "p", Type: "sonarr", Endpoint: "https://x.invalid"}},
			wantErr: []string{"p", "type", "not a provider type", "torznab"},
		},
		{
			name:    "no endpoint",
			entries: []Entry{{Name: "p", Type: "torznab", APIKey: "k"}},
			wantErr: []string{"p", "endpoint is required"},
		},
		{
			// The single most common way to write this wrong: it parses fine
			// and yields scheme "localhost", then dials nothing.
			name:    "a bare host and port",
			entries: []Entry{{Name: "p", Type: "torznab", Endpoint: "localhost:9696", APIKey: "k"}},
			wantErr: []string{"p", "endpoint", "http://"},
		},
		{
			name:    "a scheme that is not http",
			entries: []Entry{{Name: "p", Type: "torznab", Endpoint: "ftp://x.invalid", APIKey: "k"}},
			wantErr: []string{"p", "must start with http"},
		},
		{
			name:    "a URL with no host",
			entries: []Entry{{Name: "p", Type: "torznab", Endpoint: "http://", APIKey: "k"}},
			wantErr: []string{"p", "names no host"},
		},
		{
			// Credentials in a URL end up in logs, error messages and process
			// listings, and would bypass the whole redaction story.
			name: "credentials embedded in the endpoint",
			entries: []Entry{
				{Name: "p", Type: "torznab", Endpoint: "https://user:pw@x.invalid", APIKey: "k"},
			},
			wantErr: []string{"p", "must not contain credentials", "api_key"},
		},
		{
			// A Prowlarr with no key 401s on its first search — an hour later,
			// looking like an indexer fault.
			name:    "a required credential that is missing",
			entries: []Entry{{Name: "p", Type: "torznab", Endpoint: "https://x.invalid"}},
			wantErr: []string{"p", "api_key is required"},
		},
		{
			name: "a capability that does not exist",
			entries: []Entry{{
				Name: "p", Type: "torznab", Endpoint: "https://x.invalid", APIKey: "k",
				Capabilities: []string{"indexr"},
			}},
			wantErr: []string{"p", "capabilities", "not a capability", "indexer"},
		},
		{
			// A fake has no default capabilities: what it stands in for is the
			// whole of what is being configured, so it must say.
			name:    "a fake with no capabilities",
			entries: []Entry{{Name: "p", Type: "fake"}},
			wantErr: []string{"p", "capabilities is required"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Validate(tc.entries)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal should mention %q, said: %v", want, err)
				}
			}
		})
	}
}

func TestValidateAccepts(t *testing.T) {
	cases := []struct {
		name  string
		entry Entry
		check func(*testing.T, Resolved)
	}{
		{
			name:  "an indexer with its defaults",
			entry: Entry{Name: "an-indexer", Type: "torznab", Endpoint: "https://x.invalid", APIKey: "k"},
			check: func(t *testing.T, r Resolved) {
				if len(r.Capabilities) != 1 || r.Capabilities[0] != CapabilityIndexer {
					t.Errorf("capabilities = %v", r.Capabilities)
				}
				if !r.Enabled {
					t.Error("a provider is enabled unless it says otherwise")
				}
			},
		},
		{
			// Transmission authenticates optionally: an operator running it on
			// a trusted network with auth off is an ordinary deployment, and
			// refusing to start would be Heyarr insisting on a policy the
			// operator already declined.
			name:  "a download client with no credential",
			entry: Entry{Name: "a-client", Type: "transmission", Endpoint: "http://x.invalid:9091"},
			check: func(t *testing.T, r Resolved) {
				if len(r.Capabilities) != 1 || r.Capabilities[0] != CapabilityDownload {
					t.Errorf("capabilities = %v", r.Capabilities)
				}
			},
		},
		{
			name: "explicit capabilities override the defaults",
			entry: Entry{
				Name: "does-both", Type: "fake",
				Capabilities: []string{"download", "indexer"},
			},
			check: func(t *testing.T, r Resolved) {
				// Canonical order, not the order they were written: what a
				// provider advertises must render identically every read.
				if len(r.Capabilities) != 2 ||
					r.Capabilities[0] != CapabilityIndexer ||
					r.Capabilities[1] != CapabilityDownload {
					t.Errorf("capabilities = %v; the canonical order is indexer, download", r.Capabilities)
				}
			},
		},
		{
			name: "a disabled provider still resolves",
			entry: Entry{
				Name: "off", Type: "torznab", Endpoint: "https://x.invalid",
				APIKey: "k", Enabled: ptr(false),
			},
			check: func(t *testing.T, r Resolved) {
				if r.Enabled {
					t.Error("enabled: false means disabled")
				}
			},
		},
		{
			name:  "a fake needs no endpoint",
			entry: Entry{Name: "fake", Type: "fake", Capabilities: []string{"indexer"}},
			check: func(t *testing.T, r Resolved) {
				if r.Endpoint != nil {
					t.Errorf("a fake reaches nothing, so it has no endpoint: %v", r.Endpoint)
				}
			},
		},
		{
			name: "a duplicate capability is collapsed rather than refused",
			entry: Entry{
				Name: "p", Type: "fake",
				Capabilities: []string{"indexer", "indexer"},
			},
			check: func(t *testing.T, r Resolved) {
				if len(r.Capabilities) != 1 {
					t.Errorf("capabilities = %v", r.Capabilities)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Validate([]Entry{tc.entry})
			if err != nil {
				t.Fatalf("expected this to validate: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("%d resolved", len(got))
			}
			tc.check(t, got[0])
		})
	}
}

// The inversion of ADR-0023's asymmetry, and the whole of ADR-0025.
//
// A service that is unreachable — because it is down, because the host does not
// exist, because nothing is listening — must NOT stop Heyarr from starting. A
// download client down at 03:00 must not stop the library being served at
// 03:01.
func TestUnreachableIsNotAStartupError(t *testing.T) {
	// Addresses that cannot possibly answer: a reserved-for-documentation
	// address, a port nothing listens on, and a host that does not resolve.
	// Nothing here is dialled, which is the point — validation is syntactic.
	for _, endpoint := range []string{
		"http://192.0.2.1:9696",
		"http://127.0.0.1:1",
		"https://this-host-does-not-exist.invalid",
	} {
		t.Run(endpoint, func(t *testing.T) {
			got, err := Validate([]Entry{{
				Name: "an-indexer", Type: "torznab", Endpoint: endpoint, APIKey: "k",
			}})
			if err != nil {
				t.Fatalf("an unreachable provider must start: %v", err)
			}
			if len(got) != 1 || !got[0].Enabled {
				t.Fatalf("resolved = %v", got)
			}
		})
	}
}

// An empty configuration is the supported, tested degrade path.
func TestNoProvidersIsValid(t *testing.T) {
	got, err := Validate(nil)
	if err != nil {
		t.Fatalf("a node with no providers is a supported configuration: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("resolved = %v", got)
	}
}

func TestBuildReportsUnimplementedKindsHonestly(t *testing.T) {
	resolved, err := Validate([]Entry{
		{Name: "an-indexer", Type: "torznab", Endpoint: "https://x.invalid", APIKey: "k"},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := Build(resolved, nil, fixedNow)
	if err != nil {
		t.Fatalf("configuring a provider whose client is not written yet must not fail: %v", err)
	}

	// It is registered, routed and advertised — the registry's shape is
	// complete before its members are.
	if !reg.Has(CapabilityIndexer) {
		t.Fatal("a configured indexer advertises the capability")
	}

	// And it reports the truth: unhealthy, because healthy would advertise
	// something it cannot deliver.
	statuses := reg.CheckAll(t.Context())
	if len(statuses) != 1 {
		t.Fatalf("%d statuses", len(statuses))
	}
	if statuses[0].Health.Healthy {
		t.Error("a provider whose client is not implemented is not healthy")
	}
	if !strings.Contains(statuses[0].Health.Detail, "not implemented") {
		t.Errorf("the detail should say why: %q", statuses[0].Health.Detail)
	}
}

// A disabled provider is registered and never routed — so "configured and
// switched off" and "not configured at all" stay tellable apart.
func TestADisabledProviderIsReportedAndNotRouted(t *testing.T) {
	resolved, err := Validate([]Entry{{
		Name: "off", Type: "torznab", Endpoint: "https://x.invalid",
		APIKey: "k", Enabled: ptr(false),
	}})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := Build(resolved, nil, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	if reg.Has(CapabilityIndexer) {
		t.Error("a disabled provider must not be routed to")
	}
	if len(reg.JobCapabilities()) != 0 {
		t.Errorf("a disabled provider must not make the node advertise: %v", reg.JobCapabilities())
	}
	// But it is REPORTED: "why is nothing searching" should be answerable from
	// one request rather than by re-reading the config file.
	statuses := reg.Statuses()
	if len(statuses) != 1 || statuses[0].Name != "off" {
		t.Fatalf("statuses = %v", statuses)
	}
	if len(statuses[0].Capabilities) != 0 {
		t.Errorf("a disabled provider reports no capabilities, got %v", statuses[0].Capabilities)
	}
}

func TestParseCapabilityAndKindNormalise(t *testing.T) {
	for _, raw := range []string{"INDEXER", "  indexer  ", "Indexer"} {
		got, err := ParseCapability(raw)
		if err != nil || got != CapabilityIndexer {
			t.Errorf("ParseCapability(%q) = (%v, %v)", raw, got, err)
		}
	}
	for _, raw := range []string{"TORZNAB", " torznab "} {
		got, err := ParseKind(raw)
		if err != nil || got != KindTorznab {
			t.Errorf("ParseKind(%q) = (%v, %v)", raw, got, err)
		}
	}
}

// The one deliberate crossing between the two capability vocabularies. It is a
// named method so the crossing is greppable rather than a string that happens
// to match.
func TestJobCapabilitySpellingMatches(t *testing.T) {
	for _, c := range Capabilities() {
		if c.JobCapability() != string(c) {
			t.Errorf("%s crosses to %q; the two vocabularies share a spelling", c, c.JobCapability())
		}
	}
}

// Offers exist so the acceptance demo can drive a search that actually selects
// something (M3-12). They are validated rather than trusted, because a fake
// that produced a shape a real provider is refused for would teach the demo a
// fiction.
func TestOffersAreOnlyMeaningfulForAFake(t *testing.T) {
	_, err := Validate([]Entry{{
		Name: "real", Type: string(KindTorznab), Endpoint: "http://indexer.invalid:9696",
		APIKey: Secret("k"),
		Offers: []Offer{{Title: "Arrival", Candidates: []OfferedCandidate{
			{ID: "x", Attributes: map[string]any{"resolution": 2160}},
		}}},
	}})
	if err == nil {
		t.Fatal("offers on a real provider must be refused, not silently ignored — " +
			"an ignored key is a config file that says something Heyarr does not do")
	}
	if !strings.Contains(err.Error(), "only meaningful for a fake") {
		t.Errorf("the refusal should say why; got: %v", err)
	}
	// The kind is the CONSTANT rather than a literal. When `prowlarr` became
	// `torznab` (ADR-0028) this test carried on passing locally and failed on
	// CI, because a literal made it assert ParseKind's refusal instead of the
	// one it exists for. A constant makes the next rename a compile error.
}

func TestOfferedCandidatesAreValidated(t *testing.T) {
	cases := []struct {
		name  string
		offer Offer
		want  string
	}{
		{
			"no title to answer for",
			Offer{Candidates: []OfferedCandidate{{ID: "x"}}},
			"no title to answer for",
		},
		{
			// The evaluator's tie-break. A fake producing one would be teaching
			// the demo a shape a real provider is refused for.
			"a candidate with no id",
			Offer{Title: "Arrival", Candidates: []OfferedCandidate{{}}},
			"no id",
		},
		{
			"an attribute that does not exist",
			Offer{Title: "Arrival", Candidates: []OfferedCandidate{
				{ID: "x", Attributes: map[string]any{"bitrate": 5000}},
			}},
			"no attribute called",
		},
		{
			"an attribute of the wrong kind",
			Offer{Title: "Arrival", Candidates: []OfferedCandidate{
				{ID: "x", Attributes: map[string]any{"resolution": "2160p"}},
			}},
			"is a int attribute",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Validate([]Entry{{
				Name: "fake", Type: "fake", Capabilities: []string{"indexer"},
				Offers: []Offer{tc.offer},
			}})
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal should mention %q; got: %v", tc.want, err)
			}
		})
	}
}

// An attribute left OUT stays out, so §63 reports it as undetermined rather
// than as a zero. That path is most of how a degraded node behaves and is
// otherwise unreachable without a real indexer that omits a field.
func TestAnOmittedAttributeStaysOmitted(t *testing.T) {
	resolved, err := Validate([]Entry{{
		Name: "fake", Type: "fake", Capabilities: []string{"indexer"},
		Offers: []Offer{{Title: "Arrival", Candidates: []OfferedCandidate{
			{ID: "x", Attributes: map[string]any{"resolution": 2160}},
		}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := resolved[0].Offers["arrival"]
	if len(got) != 1 {
		t.Fatalf("%d candidates", len(got))
	}
	if _, present := got[0].Attributes["video_codec"]; present {
		t.Error("an attribute nobody supplied must be absent, not defaulted")
	}
	if got[0].Provider != "fake" {
		t.Errorf("provider = %q; a candidate must say which provider offered it", got[0].Provider)
	}
}
