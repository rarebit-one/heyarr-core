package providers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// The registry (§59).
//
// NOTHING in this file opens a listening socket, and that is an assertion about
// the DESIGN rather than a convenience. If the provider interface ever leaked a
// transport, a test here would need one — so the absence of `httptest` in this
// package is the evidence that values-in-values-out held.

var fixedNow = func() time.Time { return time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC) }

func candidate(id, provider string, resolution int64) acquisition.ReleaseCandidate {
	return acquisition.ReleaseCandidate{
		ID: id, Title: id, Provider: provider,
		Attributes: acquisition.Attributes{
			policy.AttrResolution: policy.Num(resolution),
		},
	}
}

func TestRegisterRefusesAmbiguityAndNonsense(t *testing.T) {
	cases := []struct {
		name    string
		build   func(*Registry) error
		wantErr string
	}{
		{
			name:    "a nil provider",
			build:   func(r *Registry) error { return r.Register(nil) },
			wantErr: "nil provider",
		},
		{
			name: "a provider with no name",
			build: func(r *Registry) error {
				return r.Register(NewFake("", CapabilityIndexer))
			},
			wantErr: "must have a name",
		},
		{
			// A provider that can do nothing is configuration nobody meant to
			// write, and it would present as "my indexer is never consulted".
			name: "a provider that declares nothing",
			build: func(r *Registry) error {
				return r.Register(NewFake("empty"))
			},
			wantErr: "declares no capabilities",
		},
		{
			// Two providers sharing a name make health, routing and a
			// candidate's Provider field ambiguous.
			name: "the same name twice",
			build: func(r *Registry) error {
				if err := r.Register(NewFake("dup", CapabilityIndexer)); err != nil {
					return err
				}
				return r.Register(NewFake("dup", CapabilityDownload))
			},
			wantErr: "already registered",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.build(New(fixedNow))
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("the refusal should mention %q, said: %v", tc.wantErr, err)
			}
		})
	}
}

// ADR-0025's whole claim, at the registry: a node with nothing configured is a
// legitimate state, not a broken one.
func TestAnEmptyRegistryIsALegitimateState(t *testing.T) {
	r := New(fixedNow)

	if r.Len() != 0 {
		t.Fatalf("Len = %d", r.Len())
	}
	for _, c := range Capabilities() {
		if r.Has(c) {
			t.Errorf("an empty registry claims %s", c)
		}
		if got := r.Route(c); len(got) != 0 {
			t.Errorf("routing %s returned %d providers", c, len(got))
		}
	}

	// Non-nil even when empty: "advertises nothing" is a deliberate,
	// reportable state, and null reads as "never set" — which is exactly the
	// wrong signal for somebody wondering why work is not being claimed.
	caps := r.JobCapabilities()
	if caps == nil {
		t.Error("JobCapabilities must be [] rather than null when nothing is configured")
	}
	if len(caps) != 0 {
		t.Errorf("JobCapabilities = %v", caps)
	}

	if got := r.Statuses(); len(got) != 0 {
		t.Errorf("Statuses returned %d", len(got))
	}
	// A search with no indexer is ErrNoProvider, not a generic failure: the
	// caller needs to tell "there is no indexer here" from "the indexer broke".
	_, err := r.Search(context.Background(), Query{Title: "anything"})
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("expected ErrNoProvider, got %v", err)
	}
}

// The capability set is OPEN, and this is the test that proves the registry has
// not become indexer-shaped. Nothing implements `metadata` in this milestone.
func TestAMetadataProviderRegistersRoutesAndReports(t *testing.T) {
	r := New(fixedNow)
	if err := r.Register(NewFake("a-metadata-service", CapabilityMetadata)); err != nil {
		t.Fatal(err)
	}

	if !r.Has(CapabilityMetadata) {
		t.Fatal("a metadata provider is configured and the registry does not know it")
	}
	routed := r.Route(CapabilityMetadata)
	if len(routed) != 1 || routed[0].Name() != "a-metadata-service" {
		t.Fatalf("routing metadata gave %v", routed)
	}

	// It advertises the corresponding job capability, so a metadata job would
	// route — even though nothing enqueues one yet.
	caps := r.JobCapabilities()
	if len(caps) != 1 || caps[0] != "metadata" {
		t.Fatalf("JobCapabilities = %v, want [metadata]", caps)
	}

	// And it is reported, which is the half an operator sees.
	statuses := r.Statuses()
	if len(statuses) != 1 {
		t.Fatalf("%d statuses", len(statuses))
	}
	if len(statuses[0].Capabilities) != 1 || statuses[0].Capabilities[0] != CapabilityMetadata {
		t.Errorf("reported capabilities = %v", statuses[0].Capabilities)
	}

	// It is NOT an indexer, so nothing searches it. A registry that routed
	// every provider to every capability would pass everything above.
	if r.Has(CapabilityIndexer) {
		t.Error("a metadata provider must not answer indexer routing")
	}
}

// Routing preserves the operator's order: somebody who lists their fast indexer
// first means it.
func TestRoutingPreservesConfiguredOrder(t *testing.T) {
	r := New(fixedNow)
	for _, name := range []string{"zulu", "alpha", "mike"} {
		if err := r.Register(NewFake(name, CapabilityIndexer)); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, p := range r.Route(CapabilityIndexer) {
		got = append(got, p.Name())
	}
	if strings.Join(got, ",") != "zulu,alpha,mike" {
		t.Errorf("routing order = %v; configuration order is the routing order", got)
	}
}

// A provider that fails must not discard what the others returned. An indexer
// being unreachable is ordinary; making the feature as reliable as its worst
// member is not.
func TestOneFailingIndexerDoesNotFailTheSearch(t *testing.T) {
	r := New(fixedNow)
	good := NewFake("good", CapabilityIndexer).
		Offer("Arrival", candidate("g1", "good", 2160))
	bad := NewFake("bad", CapabilityIndexer).FailWith(errors.New("connection refused"))

	for _, p := range []*Fake{bad, good} {
		if err := r.Register(p); err != nil {
			t.Fatal(err)
		}
	}

	result, err := r.Search(context.Background(), Query{Title: "Arrival"})
	if err != nil {
		t.Fatalf("one failing indexer must not fail the search: %v", err)
	}
	if len(result.Candidates) != 1 || result.Candidates[0].ID != "g1" {
		t.Fatalf("candidates = %v", result.Candidates)
	}
	// And the failure is REPORTED, not swallowed. §60 keeps operational
	// visibility among the things Heyarr retains.
	if len(result.Failures) != 1 {
		t.Fatalf("failures = %v", result.Failures)
	}
	if result.Failures[0].Provider != "bad" {
		t.Errorf("the failure should name the provider, got %v", result.Failures[0])
	}
	if !strings.Contains(result.Failures[0].Detail, "connection refused") {
		t.Errorf("the failure should say what happened, got %q", result.Failures[0].Detail)
	}
	if result.Consulted != 2 {
		t.Errorf("Consulted = %d; both were asked", result.Consulted)
	}
}

// Two indexers proxying the same tracker return the same release. Scoring it
// twice would put a duplicate at the top of §63's own ranking.
func TestSearchDeduplicatesAcrossProviders(t *testing.T) {
	r := New(fixedNow)
	// Both offer a candidate with the SAME provider and id, which is what a
	// shared upstream looks like.
	shared := candidate("same-release", "upstream", 2160)
	for _, name := range []string{"one", "two"} {
		if err := r.Register(NewFake(name, CapabilityIndexer).Offer("Arrival", shared)); err != nil {
			t.Fatal(err)
		}
	}

	result, err := r.Search(context.Background(), Query{Title: "Arrival"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("%d candidates for one release seen twice: %v",
			len(result.Candidates), result.Candidates)
	}

	// A release genuinely offered by two DIFFERENT providers is two candidates:
	// they may differ in what they carry, and §63 should see both.
	r2 := New(fixedNow)
	for _, name := range []string{"one", "two"} {
		if err := r2.Register(NewFake(name, CapabilityIndexer).
			Offer("Arrival", candidate("same-id", name, 2160))); err != nil {
			t.Fatal(err)
		}
	}
	result2, err := r2.Search(context.Background(), Query{Title: "Arrival"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result2.Candidates) != 2 {
		t.Errorf("%d candidates; one id from two providers is two candidates", len(result2.Candidates))
	}
}

// Determinism. §63 breaks ties on candidate id, and it can only do that if the
// input does not depend on which indexer answered first.
func TestSearchResultsAreDeterministic(t *testing.T) {
	build := func(order []string) []acquisition.ReleaseCandidate {
		r := New(fixedNow)
		for _, name := range order {
			f := NewFake(name, CapabilityIndexer).
				Offer("Arrival",
					candidate(name+"-b", name, 1080),
					candidate(name+"-a", name, 2160))
			if err := r.Register(f); err != nil {
				t.Fatal(err)
			}
		}
		result, err := r.Search(context.Background(), Query{Title: "Arrival"})
		if err != nil {
			t.Fatal(err)
		}
		return result.Candidates
	}

	first := build([]string{"zulu", "alpha"})
	for range 20 {
		again := build([]string{"alpha", "zulu"})
		if len(again) != len(first) {
			t.Fatalf("%d candidates then %d", len(first), len(again))
		}
		for i := range again {
			if again[i].ID != first[i].ID {
				t.Fatalf("registration order changed the result at %d: %s then %s",
					i, first[i].ID, again[i].ID)
			}
		}
	}
}

// A candidate with no id cannot be deduplicated or tie-broken. Dropping it
// silently would make a provider bug invisible; it is a reported failure.
func TestACandidateWithNoIDIsReportedNotDropped(t *testing.T) {
	r := New(fixedNow)
	f := NewFake("sloppy", CapabilityIndexer).
		Offer("Arrival",
			acquisition.ReleaseCandidate{ID: "", Title: "no id", Provider: "sloppy"},
			candidate("fine", "sloppy", 1080))
	if err := r.Register(f); err != nil {
		t.Fatal(err)
	}

	result, err := r.Search(context.Background(), Query{Title: "Arrival"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 || result.Candidates[0].ID != "fine" {
		t.Errorf("candidates = %v", result.Candidates)
	}
	if len(result.Failures) != 1 || !strings.Contains(result.Failures[0].Detail, "no id") {
		t.Errorf("the id-less candidate should be reported, got %v", result.Failures)
	}
}

func TestSearchRefusesAnEmptyQuery(t *testing.T) {
	r := New(fixedNow)
	if err := r.Register(NewFake("an-indexer", CapabilityIndexer)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Search(context.Background(), Query{}); err == nil {
		t.Fatal("a search with no title cannot mean anything")
	}
}

// Health starts UNKNOWN, not unhealthy. "Nobody has looked" and "we looked and
// it is broken" lead to different actions.
func TestHealthStartsUnknownAndIsRecordedByCheckAll(t *testing.T) {
	r := New(fixedNow)
	f := NewFake("an-indexer", CapabilityIndexer)
	if err := r.Register(f); err != nil {
		t.Fatal(err)
	}

	before, ok := r.Health("an-indexer")
	if !ok {
		t.Fatal("a registered provider has health")
	}
	if before.Checked() {
		t.Error("nothing has looked yet")
	}
	if before.Healthy {
		t.Error("unknown is not healthy")
	}

	statuses := r.CheckAll(context.Background())
	if len(statuses) != 1 {
		t.Fatalf("%d statuses", len(statuses))
	}
	after, _ := r.Health("an-indexer")
	if !after.Healthy || !after.Checked() {
		t.Fatalf("after a check: %+v", after)
	}
}

// A cancelled context stops the pass rather than recording a false
// "unreachable" for everything after the first.
func TestCheckAllStopsOnCancellation(t *testing.T) {
	r := New(fixedNow)
	for _, name := range []string{"one", "two", "three"} {
		if err := r.Register(NewFake(name, CapabilityIndexer)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := r.CheckAll(ctx); len(got) != 0 {
		t.Errorf("a cancelled pass checked %d providers", len(got))
	}
	for _, name := range []string{"one", "two", "three"} {
		h, _ := r.Health(name)
		if h.Checked() {
			t.Errorf("%s was recorded as checked during a cancelled pass", name)
		}
	}
}

// Health is observed asynchronously and is stale by definition. Refusing to
// route to a provider that was down ninety seconds ago would turn a blip into
// an outage.
func TestRoutingIgnoresHealth(t *testing.T) {
	r := New(fixedNow)
	if err := r.Register(NewFake("an-indexer", CapabilityIndexer)); err != nil {
		t.Fatal(err)
	}
	r.SetHealth("an-indexer", Unhealthy("connection refused", fixedNow()))

	if !r.Has(CapabilityIndexer) {
		t.Error("an unhealthy provider is still configured")
	}
	if len(r.Route(CapabilityIndexer)) != 1 {
		t.Error("an unhealthy provider is still routed to; it fails the call with a reason")
	}
	// And it still advertises the job capability: the node CAN run search
	// jobs, it will simply fail them with a message. A node with no indexer at
	// all is the case where jobs must stay pending.
	if len(r.JobCapabilities()) != 1 {
		t.Errorf("JobCapabilities = %v", r.JobCapabilities())
	}
}

func TestSetHealthIgnoresUnknownProviders(t *testing.T) {
	r := New(fixedNow)
	r.SetHealth("never-registered", Healthy("1", fixedNow()))
	if _, ok := r.Health("never-registered"); ok {
		t.Error("health was recorded for a provider that is not registered")
	}
}

// One provider, several capabilities. Kind and capability are different
// questions, and a service that both indexed and downloaded must be
// representable.
func TestOneProviderCanHaveSeveralCapabilities(t *testing.T) {
	r := New(fixedNow)
	if err := r.Register(NewFake("does-both", CapabilityIndexer, CapabilityDownload)); err != nil {
		t.Fatal(err)
	}
	if !r.Has(CapabilityIndexer) || !r.Has(CapabilityDownload) {
		t.Fatal("a provider with two capabilities answers both")
	}
	caps := r.JobCapabilities()
	if strings.Join(caps, ",") != "indexer,download" {
		t.Errorf("JobCapabilities = %v; the order is the canonical one", caps)
	}
}
