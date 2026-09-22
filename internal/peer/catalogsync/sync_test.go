package catalogsync

import (
	"context"
	"errors"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

// fakeStore is an in-memory op set with a controllable read error.
type fakeStore struct {
	ops       []string
	recorded  []string
	opsErr    error
	recordErr error
}

func (s *fakeStore) Ops(context.Context) ([]string, error) { return s.ops, s.opsErr }
func (s *fakeStore) RecordOps(_ context.Context, ops []string) error {
	if s.recordErr != nil {
		return s.recordErr
	}
	s.recorded = append(s.recorded, ops...)
	return nil
}

// fakeExchange records what it was offered and returns a scripted answer per peer.
type fakeExchange struct {
	offered map[string][]string
	answer  map[string][]string
	err     map[string]error
}

func (e *fakeExchange) Exchange(_ context.Context, t Target, ops []string) ([]string, error) {
	if e.offered == nil {
		e.offered = map[string][]string{}
	}
	e.offered[t.Peer.PeerID] = ops
	if err := e.err[t.Peer.PeerID]; err != nil {
		return nil, err
	}
	return e.answer[t.Peer.PeerID], nil
}

type fakeSiblings []Target

func (s fakeSiblings) Siblings(context.Context) ([]Target, error) { return s, nil }

func target(id string) Target { return Target{Peer: mtls.Peer{PeerID: id}, Endpoint: "https://" + id} }

// A pass offers this node's ops to each sibling and records what each answers —
// the delete a sibling made lands in this node's store.
func TestSyncAllOffersOursAndRecordsTheirs(t *testing.T) {
	store := &fakeStore{ops: []string{"our-op"}}
	ex := &fakeExchange{answer: map[string][]string{"b": {"our-op", "their-delete"}}}
	s := NewSyncer(store, ex, fakeSiblings{target("b")}, nil)

	synced, deferred, err := s.SyncAll(context.Background())
	if err != nil || synced != 1 || deferred != 0 {
		t.Fatalf("SyncAll = (%d,%d,%v), want (1,0,nil)", synced, deferred, err)
	}
	if got := ex.offered["b"]; len(got) != 1 || got[0] != "our-op" {
		t.Errorf("offered %v to b, want [our-op]", got)
	}
	// The sibling's delete op is now recorded locally.
	if len(store.recorded) != 2 {
		t.Fatalf("recorded %v, want the sibling's merged set", store.recorded)
	}
	found := false
	for _, op := range store.recorded {
		if op == "their-delete" {
			found = true
		}
	}
	if !found {
		t.Errorf("the sibling's delete op did not land locally: %v", store.recorded)
	}
}

// An unreachable sibling is deferred, not fatal: the pass still returns nil, and
// a reachable sibling in the same pass still converges.
func TestSyncAllDefersAnUnreachableSibling(t *testing.T) {
	store := &fakeStore{ops: nil}
	ex := &fakeExchange{
		err:    map[string]error{"down": errors.New("connection refused")},
		answer: map[string][]string{"up": {"a-delete"}},
	}
	s := NewSyncer(store, ex, fakeSiblings{target("down"), target("up")}, nil)

	synced, deferred, err := s.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("an unreachable sibling must not fail the pass: %v", err)
	}
	if synced != 1 || deferred != 1 {
		t.Fatalf("SyncAll = (%d synced, %d deferred), want (1,1)", synced, deferred)
	}
	if len(store.recorded) != 1 || store.recorded[0] != "a-delete" {
		t.Errorf("the reachable sibling should still converge, recorded %v", store.recorded)
	}
}

// A local read failure IS fatal to the pass — the cadence retries it — and
// nothing is offered on a pass that could not read what to offer.
func TestSyncAllErrorsOnALocalReadFailure(t *testing.T) {
	store := &fakeStore{opsErr: errors.New("db gone")}
	ex := &fakeExchange{}
	s := NewSyncer(store, ex, fakeSiblings{target("b")}, nil)

	if _, _, err := s.SyncAll(context.Background()); err == nil {
		t.Fatal("a local read failure should error the pass")
	}
	if len(ex.offered) != 0 {
		t.Errorf("nothing should be offered when the local read failed, offered %v", ex.offered)
	}
}

// No siblings is a clean no-op (a single-site node, or a pair not yet enrolled).
func TestSyncAllWithNoSiblingsIsANoOp(t *testing.T) {
	store := &fakeStore{ops: []string{"our-op"}}
	s := NewSyncer(store, &fakeExchange{}, fakeSiblings{}, nil)
	if synced, deferred, err := s.SyncAll(context.Background()); err != nil || synced != 0 || deferred != 0 {
		t.Fatalf("no siblings = (%d,%d,%v), want (0,0,nil)", synced, deferred, err)
	}
}
