package resources

import (
	"net/http"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The provider registry over HTTP (§59, M3-07).
//
// # What a degraded node has to be able to say
//
// ADR-0025 makes a node with no indexer and no download client a supported,
// tested configuration: it scans, ingests, catalogues, verifies, serves ranges
// and plays, and search and acquire jobs sit pending and visible. The cost of
// that design is that "why is nothing being acquired" has an entirely
// legitimate answer which is invisible unless something reports it.
//
// This is that report. One request, and an operator knows what is configured,
// what each thing can do, and whether it is working.
//
// # No credential leaves here
//
// Not the API key, not an endpoint with credentials embedded in it, not a
// redacted-but-length-preserving hint. What an operator needs is which
// provider, what it can do, and whether it works — and providers.Secret makes
// the leak structurally difficult rather than a matter of remembering.

// ProviderStatus is one provider, as reported.
type ProviderStatus struct {
	Name string `json:"name"`
	// Capabilities is what this provider can do for us — `indexer`,
	// `download`, `metadata`.
	//
	// EMPTY when a provider is disabled in configuration, which is how
	// "configured and switched off" is distinguished from "not configured at
	// all": the first appears here with nothing it can do, the second does not
	// appear.
	Capabilities []string `json:"capabilities"`
	// Healthy is whether the last check found it usable.
	Healthy bool `json:"healthy"`
	// Detail says what was observed, in a few words.
	Detail string `json:"detail"`
	// Version is the API version the service reported, when it said one.
	//
	// This is what replaces version pinning for a service Heyarr does not
	// install (ADR-0026): not controlling the version does not mean ignoring
	// it. It is what turns "acquisitions stopped after I upgraded the service"
	// into one request rather than an afternoon.
	Version string `json:"version,omitempty"`
	// CheckedAt is when the last check ran. Absent means NEVER CHECKED, which
	// is distinct from unhealthy — "nobody has looked" and "we looked and it is
	// broken" lead to different actions.
	CheckedAt *time.Time `json:"checked_at,omitempty"`
}

// ProvidersResponse is the GET /api/v1/providers body.
type ProvidersResponse struct {
	Providers []ProviderStatus `json:"providers"`
	// Capabilities is what this node can therefore do, as a set.
	//
	// It is not derivable from the list above without knowing that a disabled
	// provider contributes nothing, so it is stated rather than left to a
	// client to work out — and getting it wrong client-side would produce a
	// dashboard that says "indexing available" for a provider somebody
	// switched off.
	Capabilities []string `json:"capabilities"`
}

// listProviders is GET /api/v1/providers.
//
// Health is read from the DATABASE rather than from the registry's in-memory
// copy, because under ADR-0002 the worker that ran the health check may be
// another machine entirely. The registry supplies what is CONFIGURED here; the
// database supplies what was OBSERVED anywhere.
//
// A provider with no recorded health renders as never-checked rather than
// unhealthy. That is honest on a node whose worker has not run a pass yet, and
// it is the same distinction §56's satisfaction axes make.
func (a *API) listProviders(w http.ResponseWriter, r *http.Request) {
	statuses := a.providers.Statuses()

	observed := map[string]providers.Health{}
	if a.catalog != nil {
		found, err := a.catalog.ProviderHealth(r.Context())
		if err != nil {
			a.fail(w, r, "provider", err)
			return
		}
		observed = found
	}

	out := ProvidersResponse{
		Providers:    make([]ProviderStatus, 0, len(statuses)),
		Capabilities: a.providers.JobCapabilities(),
	}
	for _, s := range statuses {
		health := s.Health
		if recorded, ok := observed[s.Name]; ok {
			// What another process saw wins over what this one remembers: the
			// database is the shared answer, and the registry's copy is only
			// meaningful in the process that did the checking.
			health = recorded
		}

		caps := make([]string, 0, len(s.Capabilities))
		for _, c := range s.Capabilities {
			caps = append(caps, string(c))
		}

		status := ProviderStatus{
			Name:         s.Name,
			Capabilities: caps,
			Healthy:      health.Healthy,
			Detail:       health.Detail,
			Version:      health.Version,
		}
		if health.Checked() {
			at := health.CheckedAt.UTC()
			status.CheckedAt = &at
		}
		out.Providers = append(out.Providers, status)
	}
	a.write(w, r, http.StatusOK, out)
}
