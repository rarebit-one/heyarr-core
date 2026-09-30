package providers

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
)

// gatedIndexer is an indexer whose answer waits on a channel, so a test can
// hold one indexer "in flight" and observe what the others do meanwhile —
// without a socket or a real clock driving the outcome.
type gatedIndexer struct {
	name    string
	release chan struct{}
	started chan struct{}
	once    sync.Once
	found   []acquisition.ReleaseCandidate
	err     error
}

func newGated(name string, found []acquisition.ReleaseCandidate, err error) *gatedIndexer {
	return &gatedIndexer{
		name: name, found: found, err: err,
		release: make(chan struct{}), started: make(chan struct{}),
	}
}

func (g *gatedIndexer) Name() string                   { return g.name }
func (g *gatedIndexer) Capabilities() []Capability     { return []Capability{CapabilityIndexer} }
func (g *gatedIndexer) Check(_ context.Context) Health { return Healthy("gated", fixedNow()) }
func (g *gatedIndexer) open()                          { close(g.release) }
func (g *gatedIndexer) waitStarted(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-g.started:
	case <-time.After(within):
		t.Fatalf("%s was never asked while another indexer was still in flight", g.name)
	}
}

func (g *gatedIndexer) Search(ctx context.Context, _ Query) ([]acquisition.ReleaseCandidate, error) {
	g.once.Do(func() { close(g.started) })
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.found, g.err
}

// A dead indexer's retries must not delay the healthy ones. Asked one after
// another, every search paid each unreachable indexer's full backoff before the
// next was even tried.
func TestASlowIndexerDoesNotHoldUpTheOthers(t *testing.T) {
	r := New(fixedNow)
	slow := newGated("slow", nil, errors.New("connection refused"))
	fast := newGated("fast", []acquisition.ReleaseCandidate{candidate("f1", "fast", 1080)}, nil)
	// The slow one is FIRST in routing order: sequentially, fast would not
	// be asked until slow gave up.
	for _, p := range []Provider{slow, fast} {
		if err := r.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	close(fast.release) // fast answers the moment it is asked

	done := make(chan SearchResult, 1)
	go func() {
		res, err := r.Search(context.Background(), Query{Title: "Arrival"})
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()

	slow.waitStarted(t, 5*time.Second)
	fast.waitStarted(t, 5*time.Second)
	select {
	case <-done:
		t.Fatal("the search returned before the slow indexer answered; its failure would be lost")
	default:
	}
	slow.open()

	res := <-done
	if len(res.Candidates) != 1 || res.Candidates[0].ID != "f1" {
		t.Errorf("candidates = %v", res.Candidates)
	}
	// Errors are still surfaced, attributed to the indexer that failed.
	if len(res.Failures) != 1 || res.Failures[0].Provider != "slow" ||
		res.Failures[0].Detail != "connection refused" {
		t.Errorf("failures = %v", res.Failures)
	}
	if res.Consulted != 2 {
		t.Errorf("Consulted = %d", res.Consulted)
	}
}

// The merge must be exactly what asking in routing order produced, however the
// answers arrive. Dedup is first-seen-wins, so a shared release that differs
// between two indexers must come from the one earlier in routing order even
// when the later one answers first.
func TestConcurrentSearchMergesInRoutingOrder(t *testing.T) {
	shared := func(title string) acquisition.ReleaseCandidate {
		c := candidate("shared", "upstream", 2160)
		c.Title = title
		return c
	}
	build := func() (*Registry, []*gatedIndexer) {
		r := New(fixedNow)
		idx := []*gatedIndexer{
			newGated("first", []acquisition.ReleaseCandidate{
				shared("from-first"), candidate("a2", "first", 1080), {ID: "", Provider: "first"},
			}, nil),
			newGated("second", []acquisition.ReleaseCandidate{
				shared("from-second"), candidate("b1", "second", 720),
			}, nil),
			newGated("third", nil, errors.New("timeout")),
			newGated("fourth", []acquisition.ReleaseCandidate{candidate("d1", "fourth", 480)}, nil),
			newGated("fifth", nil, errors.New("refused")),
		}
		for _, g := range idx {
			if err := r.Register(g); err != nil {
				t.Fatal(err)
			}
		}
		return r, idx
	}

	// Reference: the answers released one at a time, in routing order.
	search := func(release func([]*gatedIndexer)) SearchResult {
		r, idx := build()
		done := make(chan SearchResult, 1)
		go func() {
			res, err := r.Search(context.Background(), Query{Title: "Arrival"})
			if err != nil {
				t.Error(err)
			}
			done <- res
		}()
		release(idx)
		return <-done
	}
	inOrder := search(func(idx []*gatedIndexer) {
		for _, g := range idx {
			g.open()
		}
	})
	// Reverse: the later indexers answer first. Only the first four are in
	// flight at once (the bound), so the fifth starts once one of them is
	// released — releasing in reverse still exercises out-of-order arrival.
	reversed := search(func(idx []*gatedIndexer) {
		for i := len(idx) - 2; i >= 0; i-- {
			idx[i].waitStarted(t, 5*time.Second)
		}
		for i := len(idx) - 2; i >= 0; i-- {
			idx[i].open()
			if i == len(idx)-2 {
				idx[len(idx)-1].waitStarted(t, 5*time.Second)
				idx[len(idx)-1].open()
			}
		}
	})

	if !reflect.DeepEqual(inOrder, reversed) {
		t.Fatalf("arrival order changed the result:\nin order: %+v\nreversed: %+v", inOrder, reversed)
	}
	for _, c := range inOrder.Candidates {
		if c.ID == "shared" && c.Title != "from-first" {
			t.Errorf("the duplicate kept came from %q; routing order says first-seen wins", c.Title)
		}
	}
	wantFailures := []string{"fifth", "first", "third"}
	if len(inOrder.Failures) != len(wantFailures) {
		t.Fatalf("failures = %v", inOrder.Failures)
	}
	for i, f := range inOrder.Failures {
		if f.Provider != wantFailures[i] {
			t.Errorf("failure %d = %v, want provider %s", i, f, wantFailures[i])
		}
	}
}

// Bounded: a node with many indexers does not open them all at once.
func TestConcurrentSearchIsBounded(t *testing.T) {
	r := New(fixedNow)
	var idx []*gatedIndexer
	for i := range searchConcurrency + 2 {
		g := newGated(fmt.Sprintf("idx-%d", i), nil, nil)
		idx = append(idx, g)
		if err := r.Register(g); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := r.Search(context.Background(), Query{Title: "Arrival"}); err != nil {
			t.Error(err)
		}
	}()
	for _, g := range idx[:searchConcurrency] {
		g.waitStarted(t, 5*time.Second)
	}
	for _, g := range idx[searchConcurrency:] {
		select {
		case <-g.started:
			t.Fatalf("%s was asked while %d others were already in flight", g.name, searchConcurrency)
		case <-time.After(50 * time.Millisecond):
		}
	}
	for _, g := range idx {
		g.open()
	}
	<-done
}

// Cancellation still reaches the indexers in flight and is returned, not
// presented as a complete search.
func TestConcurrentSearchHonoursCancellation(t *testing.T) {
	r := New(fixedNow)
	stuck := newGated("stuck", nil, nil)
	if err := r.Register(stuck); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.Search(ctx, Query{Title: "Arrival"})
		done <- err
	}()
	stuck.waitStarted(t, 5*time.Second)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled search did not return")
	}
}
