package providers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
)

// Registry is the centralised provider registry (§59).
//
// One per instance. Content types do not maintain their own (§61), and a new
// provider kind is a new capability here rather than a second registry (§83).
//
// # What it holds
//
//	credentials    behind Secret, revealed only inside an implementation
//	capabilities   what each provider can do
//	routing        which providers answer a given capability, in a stable order
//	health         what the last check found, per provider
//	configuration  the operator's declared set
//
// # It is safe for concurrent use
//
// The health job writes while the API reads, and under `heyarr all` those are
// two goroutines in one process. A registry that needed the caller to
// synchronise would be one every caller synchronised differently.
type Registry struct {
	mu sync.RWMutex
	// order is the operator's configured order, which is the routing order.
	// Preserved rather than sorted by name: an operator who lists their fast
	// indexer first means it, and re-ordering their configuration alphabetically
	// would be Heyarr overriding a decision it was told.
	order  []string
	byName map[string]Provider
	health map[string]Health
	nowFn  func() time.Time
}

// ErrNoProvider is returned when nothing is configured for a capability.
//
// A distinct error because it is not a failure — it is ADR-0025's degrade path,
// and a caller needs to tell "there is no indexer here" from "the indexer
// broke". The first is a node doing exactly what it was configured to do.
var ErrNoProvider = errors.New("providers: no provider has that capability")

// New builds an empty registry.
func New(now func() time.Time) *Registry {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Registry{
		byName: map[string]Provider{},
		health: map[string]Health{},
		nowFn:  now,
	}
}

// Register adds a provider.
//
// Registering a name twice is refused rather than overwriting. Two providers
// sharing a name would make health, routing and a candidate's Provider field
// ambiguous — and the ambiguity would present as "sometimes my searches use the
// wrong indexer", which is not a debuggable sentence.
func (r *Registry) Register(p Provider) error {
	if p != nil && len(p.Capabilities()) == 0 {
		// A provider that can do nothing is configuration nobody meant to
		// write. Refusing it is cheaper than an operator wondering why their
		// indexer is never consulted.
		//
		// The one legitimate exception — a provider an operator DISABLED — goes
		// through RegisterInert, so that "declares nothing by mistake" and
		// "declares nothing on purpose" are two different call sites rather
		// than one flag.
		return fmt.Errorf("providers: %q declares no capabilities", strings.TrimSpace(p.Name()))
	}
	return r.RegisterInert(p)
}

// RegisterInert adds a provider that is allowed to declare no capabilities.
//
// It exists for exactly one case: a provider an operator has disabled. Such a
// provider must still be REPORTED — "why is nothing searching" should be
// answerable from GET /api/v1/providers rather than by re-reading the config
// file — while never being routed to.
//
// It is a separate method rather than a boolean on Register so that every
// capability-less registration is a visible, greppable decision. A flag would
// eventually be passed as true by something that had not thought about it.
func (r *Registry) RegisterInert(p Provider) error {
	if p == nil {
		return errors.New("providers: cannot register a nil provider")
	}
	name := strings.TrimSpace(p.Name())
	if name == "" {
		return errors.New("providers: a provider must have a name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byName[name]; exists {
		return fmt.Errorf("providers: a provider named %q is already registered", name)
	}
	r.byName[name] = p
	r.order = append(r.order, name)
	// Unknown, not unhealthy. Nothing has looked yet, and those are different
	// statements that lead to different actions.
	r.health[name] = Unknown()
	return nil
}

// Len is how many providers are configured.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.order)
}

// Names lists every provider in routing order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// Lookup returns one provider by name.
func (r *Registry) Lookup(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byName[name]
	return p, ok
}

// Route returns every provider with a capability, in configured order.
//
// It does NOT filter by health, and that is deliberate. Health is observed
// asynchronously and can be stale by definition; refusing to route to a
// provider that was unreachable ninety seconds ago would turn a blip into an
// outage, and a provider that is genuinely down fails the call anyway with a
// better message than "nothing was tried".
//
// Callers that want only-healthy can filter with Health(); nothing does yet,
// and inventing the preference before something needs it would be a policy
// nobody asked for.
func (r *Registry) Route(c Capability) []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []Provider
	for _, name := range r.order {
		p := r.byName[name]
		if hasCapability(p.Capabilities(), c) {
			out = append(out, p)
		}
	}
	return out
}

// Has reports whether anything can do this.
//
// It is what the worker asks to decide whether to advertise the corresponding
// job capability, so it is presence rather than health: a node with a
// configured-but-currently-unreachable indexer can still run search jobs, it
// will simply fail them with a reason. A node with none cannot run them at all,
// and those jobs must stay pending rather than fail (ADR-0025).
func (r *Registry) Has(c Capability) bool { return len(r.Route(c)) > 0 }

// JobCapabilities is what a worker built on this registry should advertise.
//
// Non-nil even when empty, for the same reason media.Toolchain.Capabilities is:
// "advertises nothing" is a deliberate, reportable state, and a nil slice logs
// and marshals as null, which reads as "never set". Someone reading this is
// usually reading it because work is not being claimed, which is exactly when
// that distinction matters.
func (r *Registry) JobCapabilities() []string {
	out := []string{}
	for _, c := range Capabilities() {
		if r.Has(c) {
			out = append(out, c.JobCapability())
		}
	}
	return out
}

// Indexers is every provider that can search, in routing order.
func (r *Registry) Indexers() []Indexer {
	var out []Indexer
	for _, p := range r.Route(CapabilityIndexer) {
		if idx, ok := p.(Indexer); ok {
			out = append(out, idx)
		}
	}
	return out
}

// Downloaders is every provider that can transfer, in routing order.
func (r *Registry) Downloaders() []Downloader {
	var out []Downloader
	for _, p := range r.Route(CapabilityDownload) {
		if dl, ok := p.(Downloader); ok {
			out = append(out, dl)
		}
	}
	return out
}

// Health returns what the last check found for one provider.
func (r *Registry) Health(name string) (Health, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.health[name]
	return h, ok
}

// SetHealth records what a check found.
func (r *Registry) SetHealth(name string, h Health) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, known := r.byName[name]; !known {
		return
	}
	r.health[name] = h
}

// CheckAll exercises every provider and records what it found.
//
// This is the health job's body (ADR-0025): reachability is checked
// continuously and is a job, because a service that is down at 03:00 must not
// have stopped Heyarr from starting at 20:00.
//
// It never returns an error for an unhealthy provider. A provider being down is
// a REPORT — the point of the pass is to record it — and failing the job would
// mean one unreachable indexer stops the download client from being checked.
func (r *Registry) CheckAll(ctx context.Context) []Status {
	providers := make([]Provider, 0, r.Len())
	r.mu.RLock()
	for _, name := range r.order {
		providers = append(providers, r.byName[name])
	}
	r.mu.RUnlock()

	out := make([]Status, 0, len(providers))
	for _, p := range providers {
		// Cancellation stops the pass rather than recording a false
		// "unreachable" for every provider after the first: a cancelled
		// context means the worker is shutting down, not that the world broke.
		if ctx.Err() != nil {
			break
		}
		h := p.Check(ctx)
		if h.CheckedAt.IsZero() {
			// An implementation that forgot to stamp its report would produce
			// health that reads as "never checked" forever, which is precisely
			// the state this pass exists to move things out of.
			h.CheckedAt = r.nowFn()
		}
		r.SetHealth(p.Name(), h)
		out = append(out, Status{
			Name:         p.Name(),
			Capabilities: sortCapabilities(p.Capabilities()),
			Health:       h,
		})
	}
	return out
}

// Status is one provider as reported by GET /api/v1/providers.
//
// It carries NO credential and no endpoint-with-credentials-in-it. What an
// operator needs is which provider, what it can do, and whether it is working.
type Status struct {
	Name         string
	Capabilities []Capability
	Health       Health
}

// Statuses reports every provider, in routing order.
func (r *Registry) Statuses() []Status {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Status, 0, len(r.order))
	for _, name := range r.order {
		p := r.byName[name]
		out = append(out, Status{
			Name:         name,
			Capabilities: sortCapabilities(p.Capabilities()),
			Health:       r.health[name],
		})
	}
	return out
}

// SearchResult is what a fan-out across every indexer found.
//
// Candidates and Failures are both present because a partial answer is the
// normal case with several indexers: one being down must not discard what the
// others returned, and it must not be silently invisible either. §60 keeps
// operational visibility among the things Heyarr retains.
type SearchResult struct {
	// Candidates is everything that came back, in a deterministic order.
	Candidates []acquisition.ReleaseCandidate
	// Failures is the providers that could not answer, by name, with why.
	Failures []Failure
	// Consulted is how many providers were asked, so "no candidates" and "no
	// indexers" are tellable apart at the call site.
	Consulted int
}

// Failure is one provider that could not answer.
type Failure struct {
	Provider string
	Detail   string
}

// Search asks every indexer, in routing order, and aggregates the answers.
//
// # Why the registry does the fan-out
//
// §59 lists routing as the registry's job. The alternative is every caller
// iterating Indexers() itself, which is the same loop written in several places
// with the deduplication done differently in each — and the deduplication is
// the part that matters, because two indexers proxying the same tracker return
// the same release twice and §63 would then score it twice.
//
// # A provider that fails does not fail the search
//
// It becomes a Failure and the others are still asked. An indexer being
// unreachable is ordinary; discarding three working indexers' results because a
// fourth timed out would make the whole feature as reliable as its worst
// member.
func (r *Registry) Search(ctx context.Context, q Query) (SearchResult, error) {
	if err := q.Validate(); err != nil {
		return SearchResult{}, err
	}
	indexers := r.Indexers()
	if len(indexers) == 0 {
		// Not an error the caller should treat as broken — see ErrNoProvider.
		return SearchResult{}, fmt.Errorf("%w: %s", ErrNoProvider, CapabilityIndexer)
	}

	result := SearchResult{Consulted: len(indexers)}
	seen := map[string]bool{}
	for _, idx := range indexers {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		found, err := idx.Search(ctx, q)
		if err != nil {
			result.Failures = append(result.Failures, Failure{
				Provider: idx.Name(),
				// %v rather than %w: this is a report for an operator, and a
				// Secret in an implementation's error renders redacted through
				// String(). Wrapping would make the chain inspectable, which
				// is not what a status line is for.
				Detail: fmt.Sprintf("%v", err),
			})
			continue
		}
		for _, c := range found {
			// Deduplicate across providers. Two indexers proxying the same
			// tracker return the same release, and §63 scoring it twice would
			// put a duplicate at the top of its own ranking.
			key := c.Provider + "\x00" + c.ID
			if c.ID == "" {
				// A candidate with no id cannot be deduplicated and cannot be
				// tie-broken deterministically (M3-04). Dropping it is worse
				// than reporting it, so it is a failure against the provider
				// that produced it.
				result.Failures = append(result.Failures, Failure{
					Provider: idx.Name(),
					Detail:   "returned a candidate with no id, which cannot be ranked deterministically",
				})
				continue
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			result.Candidates = append(result.Candidates, c)
		}
	}

	// A stable order out of the registry, so that M3-04's ranking is fed the
	// same input every run. The evaluator breaks ties on candidate id, but it
	// can only do that if the id it receives does not depend on which indexer
	// answered first.
	sort.SliceStable(result.Candidates, func(i, j int) bool {
		a, b := result.Candidates[i], result.Candidates[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.ID < b.ID
	})
	sort.SliceStable(result.Failures, func(i, j int) bool {
		return result.Failures[i].Provider < result.Failures[j].Provider
	})
	return result, nil
}

func hasCapability(caps []Capability, want Capability) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}
