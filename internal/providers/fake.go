package providers

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
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
	// added is every source handed to Add, in order — see Added().
	added []secret.Value
	// feeds is what Enumerate returns, per feed ref — so one fake can stand in
	// for a metadata service across a library of followed sources, which is what
	// the demo and the follow-pipeline integration test need (M12).
	feeds map[string][]followed.FeedItem
	// enumerations counts Enumerate calls, so a test can assert the poll loop
	// actually reached the adapter rather than inferring it from projected wants.
	enumerations int
	// servesTypes, when non-nil, restricts which followed types this fake claims
	// to serve (see ServingTypes, ServesType). Nil means it serves ANY type,
	// which keeps the many single-fake follow tests routing to it unchanged.
	servesTypes map[followed.Type]bool
	now         func() time.Time
}

// NewFake builds an inert provider with the given capabilities.
func NewFake(name string, caps ...Capability) *Fake {
	return &Fake{
		name:       name,
		caps:       sortCapabilities(caps),
		candidates: map[string][]acquisition.ReleaseCandidate{},
		feeds:      map[string][]followed.FeedItem{},
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

// Add implements Downloader. It records what it was asked to fetch and returns
// a transfer describing it, so a test can assert that a grab reached a client
// AND what it handed over — the second half being the part that would
// otherwise pass while sending an empty source.
func (f *Fake) Add(_ context.Context, source secret.Value) (Transfer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Inside the lock: FailWith writes this field under the same mutex, and
	// reading it outside would be a data race that -race finds only sometimes.
	if f.failWith != nil {
		return Transfer{}, f.failWith
	}
	f.added = append(f.added, source)
	return Transfer{
		ID:   "fake:" + strconv.Itoa(len(f.added)),
		Name: "a fake transfer",
	}, nil
}

// Added is every source this fake was handed, in order.
func (f *Fake) Added() []secret.Value {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]secret.Value(nil), f.added...)
}

// OfferFeed makes a set of feed items the answer Enumerate gives for a ref, so
// a fake metadata provider can drive the follow pipeline without a network —
// the same reason Offer exists for searches (M12).
func (f *Fake) OfferFeed(ref string, items ...followed.FeedItem) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.feeds[normaliseRef(ref)] = items
	return f
}

// ServingTypes restricts the followed types this fake serves, for the routing
// tests that configure more than one metadata fake. Unset (the default), a fake
// serves ANY type, so a single-fake follow test routes to it without ceremony.
func (f *Fake) ServingTypes(types ...followed.Type) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.servesTypes = map[followed.Type]bool{}
	for _, t := range types {
		f.servesTypes[t] = true
	}
	return f
}

// ServesType implements FeedProvider. A fake with no configured types serves any
// (the common single-adapter case); one configured with ServingTypes serves only
// those, so a test can prove the poll routes to the RIGHT adapter.
func (f *Fake) ServesType(t followed.Type) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.servesTypes == nil {
		return true
	}
	return f.servesTypes[t]
}

// Enumerations is how many times Enumerate was called.
func (f *Fake) Enumerations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enumerations
}

// Enumerate implements FeedProvider. It returns whatever OfferFeed staged for
// the ref, so the poll loop and projection are exercisable end to end without a
// metadata service. FailWith makes it fail, so the beat's hold-off and the
// worker's error path are reachable too.
func (f *Fake) Enumerate(_ context.Context, ref string) ([]followed.FeedItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enumerations++
	if f.failWith != nil {
		return nil, f.failWith
	}
	found := f.feeds[normaliseRef(ref)]
	return append([]followed.FeedItem(nil), found...), nil
}

func normaliseTitle(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func normaliseRef(s string) string {
	return strings.TrimSpace(s)
}

// Compile-time proof that a Fake really is substitutable for the real thing. If
// the interface grows a method, this breaks here rather than in the demo.
var (
	_ Provider     = (*Fake)(nil)
	_ Indexer      = (*Fake)(nil)
	_ Downloader   = (*Fake)(nil)
	_ FeedProvider = (*Fake)(nil)
)
