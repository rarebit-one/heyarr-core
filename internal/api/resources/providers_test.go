// Every HTTP response in this file is closed by the t.Cleanup that the harness
// registers, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// GET /api/v1/providers (§59, M3-07).
//
// The endpoint exists because ADR-0025 makes "no indexer, no download client" a
// supported configuration, and the cost of that design is that "why is nothing
// being acquired" has a legitimate answer which is invisible unless something
// reports it.

type wireProviders struct {
	Providers []struct {
		Name         string   `json:"name"`
		Capabilities []string `json:"capabilities"`
		Healthy      bool     `json:"healthy"`
		Detail       string   `json:"detail"`
		Version      string   `json:"version"`
		CheckedAt    *string  `json:"checked_at"`
	} `json:"providers"`
	Capabilities []string `json:"capabilities"`
}

func readProviders(t *testing.T, h *harness) wireProviders {
	t.Helper()
	resp := h.get("/api/v1/providers")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	var got wireProviders
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// ADR-0025's whole claim at the edge: a node with nothing configured answers,
// and answers honestly. Not a 404, not a 500 — an empty set.
func TestANodeWithNoProvidersReportsAnEmptySet(t *testing.T) {
	h := newHarness(t)
	got := readProviders(t, h)

	if len(got.Providers) != 0 {
		t.Errorf("providers = %v", got.Providers)
	}
	if got.Capabilities == nil {
		t.Error("capabilities must be [] rather than null — null reads as \"never set\", " +
			"which is the wrong signal for somebody wondering why nothing is claimed")
	}
	if len(got.Capabilities) != 0 {
		t.Errorf("capabilities = %v", got.Capabilities)
	}

	// And the rest of the API is unaffected. This is the assertion that proves
	// the claim rather than merely stating it.
	if resp := h.get("/api/v1/works"); resp.StatusCode != http.StatusOK {
		t.Errorf("a node with no providers must still serve its catalog, got %d", resp.StatusCode)
	}
}

func TestConfiguredProvidersAreReported(t *testing.T) {
	reg := providers.New(nil)
	for _, p := range []*providers.Fake{
		providers.NewFake("an-indexer", providers.CapabilityIndexer),
		providers.NewFake("a-client", providers.CapabilityDownload),
	} {
		if err := reg.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	h := newHarness(t, withProviders(reg))

	got := readProviders(t, h)
	if len(got.Providers) != 2 {
		t.Fatalf("providers = %v", got.Providers)
	}
	// Configured order is routing order, and it is what is reported.
	if got.Providers[0].Name != "an-indexer" || got.Providers[1].Name != "a-client" {
		t.Errorf("order = %v; configuration order is routing order", got.Providers)
	}
	if strings.Join(got.Capabilities, ",") != "indexer,download" {
		t.Errorf("capabilities = %v", got.Capabilities)
	}
}

// "Nobody has looked" is distinct from "we looked and it is broken", and the
// distinction has to survive to the wire — they lead to different actions.
func TestNeverCheckedIsDistinctFromUnhealthy(t *testing.T) {
	reg := providers.New(nil)
	if err := reg.Register(providers.NewFake("an-indexer", providers.CapabilityIndexer)); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, withProviders(reg))

	got := readProviders(t, h)
	if len(got.Providers) != 1 {
		t.Fatalf("providers = %v", got.Providers)
	}
	if got.Providers[0].CheckedAt != nil {
		t.Errorf("checked_at = %v; nothing has checked yet", *got.Providers[0].CheckedAt)
	}
	if got.Providers[0].Healthy {
		t.Error("unknown is not healthy")
	}
	if !strings.Contains(got.Providers[0].Detail, "not checked") {
		t.Errorf("detail = %q; it should say nothing has looked", got.Providers[0].Detail)
	}
}

// A disabled provider is REPORTED with no capabilities, so "configured and
// switched off" and "not configured at all" stay tellable apart. Without this,
// "why is nothing searching" means re-reading the config file.
func TestADisabledProviderIsReportedWithNoCapabilities(t *testing.T) {
	resolved, err := providers.Validate([]providers.Entry{
		{Name: "an-indexer", Type: "torznab", Endpoint: "https://x.invalid", APIKey: "k"},
		{
			Name: "switched-off", Type: "torznab", Endpoint: "https://y.invalid",
			APIKey: "k", Enabled: boolPtr(false),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Build(resolved, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, withProviders(reg))

	got := readProviders(t, h)
	if len(got.Providers) != 2 {
		t.Fatalf("a disabled provider must still be reported: %v", got.Providers)
	}

	byName := map[string][]string{}
	for _, p := range got.Providers {
		byName[p.Name] = p.Capabilities
	}
	if len(byName["switched-off"]) != 0 {
		t.Errorf("a disabled provider reports no capabilities, got %v", byName["switched-off"])
	}
	if len(byName["an-indexer"]) != 1 {
		t.Errorf("the enabled one still reports its capability, got %v", byName["an-indexer"])
	}
	// And the node's own capability set counts only the enabled one.
	if strings.Join(got.Capabilities, ",") != "indexer" {
		t.Errorf("capabilities = %v; a disabled provider contributes nothing", got.Capabilities)
	}
}

// The credential must not reach the response. Asserted by searching the body
// for the plaintext, not by reading the struct definition.
func TestNoCredentialReachesTheResponse(t *testing.T) {
	const secret = "sk-live-DO-NOT-LEAK-8e91c4"
	resolved, err := providers.Validate([]providers.Entry{{
		Name: "an-indexer", Type: "torznab",
		Endpoint: "https://indexer.invalid", APIKey: providers.Secret(secret),
	}})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Build(resolved, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, withProviders(reg))

	resp := h.get("/api/v1/providers")
	body := string(h.body(resp))
	if strings.Contains(body, secret) {
		t.Fatalf("the credential reached the API response:\n%s", body)
	}
	// And the response is genuinely useful — a leak test that passed against an
	// empty body would prove nothing.
	if !strings.Contains(body, "an-indexer") {
		t.Errorf("expected the provider to be reported at all:\n%s", body)
	}
}

// The metadata capability exists with nothing implementing it, and it reaches
// the wire. This is what stops the registry becoming indexer-shaped.
func TestAMetadataProviderIsReported(t *testing.T) {
	reg := providers.New(nil)
	if err := reg.Register(
		providers.NewFake("a-metadata-service", providers.CapabilityMetadata)); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, withProviders(reg))

	got := readProviders(t, h)
	if len(got.Providers) != 1 {
		t.Fatalf("providers = %v", got.Providers)
	}
	if strings.Join(got.Providers[0].Capabilities, ",") != "metadata" {
		t.Errorf("capabilities = %v", got.Providers[0].Capabilities)
	}
	if strings.Join(got.Capabilities, ",") != "metadata" {
		t.Errorf("node capabilities = %v", got.Capabilities)
	}
}

func boolPtr(b bool) *bool { return &b }
