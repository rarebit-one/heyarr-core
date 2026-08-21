package providers

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
)

// Fake is a provider that talks to nothing.
//
// # Why this is production code rather than a test fixture
//
// ADR-0026: a real indexer proxies real trackers with real credentials and can
// never run in CI. The milestone's central claim — that Heyarr decides what
// should exist and explains why, WITHOUT a real indexer being present — is
// therefore only demonstrable against something that behaves exactly like a
// provider and needs no service.
//
// Putting that in `_test.go` would mean the acceptance demo could not use it,
// and the demo is the thing that proves the claim on a real machine over a real
// socket. Putting it here means the demo exercises the same registration,
// routing, health and capability paths as production — rather than a parallel
// set that agrees with production only until one of them changes.
//
// It is inert by construction: no endpoint, no credential, no I/O, and a health
// check that cannot fail because there is nothing that could be unwell.
type Fake struct {
	name string
	caps []Capability

	mu sync.Mutex
	// candidates is what Search returns, per lower-cased title. A map rather
	// than a single list so one fake can stand in for a whole library's worth
	// of searches, which is what the demo needs.
	candidates map[string][]acquisition.ReleaseCandidate
	// searches counts calls, so a test can assert routing actually routed
	// rather than inferring it from a result that might have come from
	// anywhere.
	searches int
	// failWith, when set, makes Search fail — so the "one indexer is down and
	// the others still answer" path is exercisable without a network.
	failWith error
	now      func() time.Time
}

// NewFake builds an inert provider with the given capabilities.
func NewFake(name string, caps ...Capability) *Fake {
	return &Fake{
		name:       name,
		caps:       sortCapabilities(caps),
		candidates: map[string][]acquisition.ReleaseCandidate{},
		now:        func() time.Time { return time.Now().UTC() },
	}
}

// Name implements Provider.
func (f *Fake) Name() string { return f.name }

// Capabilities implements Provider.
func (f *Fake) Capabilities() []Capability { return append([]Capability(nil), f.caps...) }

// Check implements Provider. A fake reaches nothing, so it is always healthy —
// there is no failure mode to report.
func (f *Fake) Check(_ context.Context) Health {
	return Healthy("fake", f.now())
}

// Offer makes a set of candidates the answer to a title.
func (f *Fake) Offer(title string, candidates ...acquisition.ReleaseCandidate) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.candidates[normaliseTitle(title)] = candidates
	return f
}

// FailWith makes every subsequent Search fail, so the partial-answer path is
// testable.
func (f *Fake) FailWith(err error) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWith = err
	return f
}

// Searches is how many times Search was called.
func (f *Fake) Searches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.searches
}

// Search implements Indexer.
func (f *Fake) Search(_ context.Context, q Query) ([]acquisition.ReleaseCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searches++
	if f.failWith != nil {
		return nil, f.failWith
	}
	found := f.candidates[normaliseTitle(q.Title)]
	out := make([]acquisition.ReleaseCandidate, 0, len(found))
	for _, c := range found {
		if c.Provider == "" {
			// A candidate must say which provider offered it, because the
			// registry deduplicates on (provider, id) and §63 reports it. A
			// fake that forgot would produce a duplicate-looking result the
			// moment two fakes offered the same release.
			c.Provider = f.name
		}
		out = append(out, c)
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

// Transfers implements Downloader. A fake moves nothing.
func (f *Fake) Transfers(_ context.Context) ([]Transfer, error) { return nil, nil }

func normaliseTitle(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// Compile-time proof that a Fake really is substitutable for the real thing. If
// the interface grows a method, this breaks here rather than in the demo.
var (
	_ Provider   = (*Fake)(nil)
	_ Indexer    = (*Fake)(nil)
	_ Downloader = (*Fake)(nil)
)
