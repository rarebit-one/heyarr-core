package siblingfanout_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/peer/siblingfanout"
)

const opHash = "blake3:op-a"

// fakeFetcher answers per peer id: an answer, an error, or blocking until the
// ask is cancelled. It counts asks, which is what tells a cache from a re-dial.
type fakeFetcher struct {
	mu      sync.Mutex
	answers map[string]peerapi.FanOutAnswer
	errs    map[string]error
	block   map[string]bool
	asks    map[string]int
}

func (f *fakeFetcher) Fetch(ctx context.Context, t siblingfanout.Target, op string) (peerapi.FanOutAnswer, error) {
	f.mu.Lock()
	f.asks[t.Peer.PeerID]++
	ans, err, block := f.answers[t.Peer.PeerID], f.errs[t.Peer.PeerID], f.block[t.Peer.PeerID]
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return peerapi.FanOutAnswer{}, ctx.Err()
	}
	return ans, err
}

func (f *fakeFetcher) count(peer string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asks[peer]
}

func targets(ids ...string) siblingfanout.Targets {
	return func(context.Context) ([]siblingfanout.Target, error) {
		var out []siblingfanout.Target
		for _, id := range ids {
			out = append(out, siblingfanout.Target{Peer: mtls.Peer{PeerID: id, Name: "name-" + id}})
		}
		return out, nil
	}
}

// TestOutcomesPerSibling: each sibling's answer is reported in Targets order —
// its outcome for the op, pending when it has none, unknown when it could not
// be asked or did not answer the question — and none of that is an error.
func TestOutcomesPerSibling(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	f := &fakeFetcher{
		answers: map[string]peerapi.FanOutAnswer{
			"applied": {Site: "site-b", Applications: []peerapi.FanOutApplication{
				{OpHash: "blake3:another", Outcome: "held"},
				{OpHash: opHash, Outcome: "applied", Detail: "from site site-a",
					AppliedAt: "2026-11-21T08:00:00Z", State: "downloading"},
			}},
			"pending": {Site: "site-c", Applications: []peerapi.FanOutApplication{}},
		},
		errs: map[string]error{
			"down":    errors.New("dial tcp: connection refused"),
			"refuses": &siblingfanout.RefusedError{Status: 404, Body: "no peer route"},
		},
		block: map[string]bool{"hangs": true},
		asks:  map[string]int{},
	}
	r := siblingfanout.NewReader(siblingfanout.Options{
		Fetch: f, Targets: targets("applied", "pending", "down", "refuses", "hangs"),
		Timeout: 50 * time.Millisecond, Now: func() time.Time { return now },
	})
	got, err := r.Outcomes(context.Background(), opHash)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d outcomes, want one per sibling: %+v", len(got), got)
	}
	want := []struct {
		peer, site, outcome, state string
		reachable                  bool
		detail                     string
	}{
		{"applied", "site-b", "applied", "downloading", true, "from site site-a"},
		{"pending", "site-c", siblingfanout.OutcomePending, "", true, ""},
		{"down", "", siblingfanout.OutcomeUnknown, "", false, "unreachable: "},
		{"refuses", "", siblingfanout.OutcomeUnknown, "", true, "did not answer the question"},
		{"hangs", "", siblingfanout.OutcomeUnknown, "", false, "unreachable: "},
	}
	for i, w := range want {
		o := got[i]
		if o.PeerID != w.peer || o.Name != "name-"+w.peer || o.Site != w.site || o.Outcome != w.outcome ||
			o.State != w.state || o.Reachable != w.reachable || !strings.Contains(o.Detail, w.detail) ||
			!o.AskedAt.Equal(now) {
			t.Errorf("sibling %s = %+v", w.peer, o)
		}
	}
}

// TestOutcomesAreCachedForTheTTL: a second read inside the TTL asks nobody —
// including the sibling that was down, so a dead sibling costs one timeout per
// TTL, not one per read — and a read after it asks again.
func TestOutcomesAreCachedForTheTTL(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	f := &fakeFetcher{
		answers: map[string]peerapi.FanOutAnswer{"up": {Site: "site-b"}},
		errs:    map[string]error{"down": errors.New("no route to host")},
		asks:    map[string]int{},
	}
	r := siblingfanout.NewReader(siblingfanout.Options{
		Fetch: f, Targets: targets("up", "down"), TTL: time.Minute, Now: func() time.Time { return clock() },
	})
	read := func() []siblingfanout.Outcome {
		t.Helper()
		got, err := r.Outcomes(context.Background(), opHash)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	read()
	now = now.Add(59 * time.Second)
	if got := read(); got[1].Reachable || got[1].Outcome != siblingfanout.OutcomeUnknown {
		t.Errorf("the cached unreachable answer = %+v", got[1])
	}
	if f.count("up") != 1 || f.count("down") != 1 {
		t.Fatalf("inside the TTL: asked up %d, down %d times, want 1 each", f.count("up"), f.count("down"))
	}
	if r.Outcomes(context.Background(), "blake3:op-b"); f.count("up") != 2 {
		t.Errorf("another op is its own cache entry: asked up %d times, want 2", f.count("up"))
	}
	now = now.Add(time.Second)
	read()
	if f.count("up") != 3 || f.count("down") != 3 {
		t.Errorf("after the TTL: asked up %d, down %d times, want 3 each", f.count("up"), f.count("down"))
	}
}

// TestACancelledReadIsNotCached: a status read that ends before a sibling
// answers says nothing about the sibling, so the next read asks again.
func TestACancelledReadIsNotCached(t *testing.T) {
	t.Parallel()
	f := &fakeFetcher{block: map[string]bool{"slow": true}, asks: map[string]int{}}
	r := siblingfanout.NewReader(siblingfanout.Options{Fetch: f, Targets: targets("slow"), Timeout: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := r.Outcomes(ctx, opHash)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Outcome != siblingfanout.OutcomeUnknown {
		t.Errorf("a cancelled ask = %+v, want unknown", got[0])
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := r.Outcomes(ctx2, opHash); err != nil {
		t.Fatal(err)
	}
	if f.count("slow") != 2 {
		t.Errorf("asked %d times, want the cancelled answer not cached", f.count("slow"))
	}
}

// fanOutSource is a sibling's FanOutSource: it records which signer asked.
type fanOutSource struct {
	mu      sync.Mutex
	signers []string
}

func (s *fanOutSource) FanOutFrom(_ context.Context, signer, op string, _ int) (peerapi.FanOutAnswer, error) {
	s.mu.Lock()
	s.signers = append(s.signers, signer)
	s.mu.Unlock()
	return peerapi.FanOutAnswer{Site: "site-b", Applications: []peerapi.FanOutApplication{
		{OpHash: op, Outcome: "deferred", AppliedAt: "2026-11-21T08:00:00Z"},
	}}, nil
}

func node(t *testing.T, id string) (mtls.Peer, *mtls.Material) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := mtls.NewMaterial(mtls.MaterialOptions{PrivateKey: priv, PeerID: id, Lifetime: time.Hour, RenewBefore: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return mtls.Peer{PeerID: id, Name: id, PublicKey: pub}, m
}

// TestClientAsksOverThePinnedLink: the real client reaches a sibling's peer
// surface over mTLS, the sibling answers for the ops the CERTIFICATE's key
// signed, and a sibling nobody is listening for is an unknown, unreachable
// outcome next to it rather than a failed read.
func TestClientAsksOverThePinnedLink(t *testing.T) {
	t.Parallel()
	origin, originMat := node(t, "origin")
	sibling, sibMat := node(t, "sibling")
	src := &fanOutSource{}
	srv, err := peerapi.New(peerapi.Options{
		Addr: "127.0.0.1:0", Material: sibMat, Members: mtls.PinnedKey(origin),
		SelfPeerID: sibling.PeerID, FanOut: src,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	// A port that was listening a moment ago and is not now: nothing answers.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := ln.Addr().String()
	_ = ln.Close()
	gone, _ := node(t, "gone")

	r := siblingfanout.NewReader(siblingfanout.Options{
		Fetch: siblingfanout.NewClient(originMat, nil),
		Targets: func(context.Context) ([]siblingfanout.Target, error) {
			return []siblingfanout.Target{
				{Peer: sibling, Endpoint: "https://" + srv.Addr()},
				{Peer: gone, Endpoint: "https://" + deadAddr},
			}, nil
		},
		Timeout: 5 * time.Second,
	})
	got, err := r.Outcomes(context.Background(), opHash)
	if err != nil {
		t.Fatal(err)
	}
	if o := got[0]; !o.Reachable || o.Site != "site-b" || o.Outcome != "deferred" || o.AppliedAt == "" {
		t.Errorf("the reachable sibling = %+v", o)
	}
	if o := got[1]; o.Reachable || o.Outcome != siblingfanout.OutcomeUnknown || !strings.HasPrefix(o.Detail, "unreachable: ") {
		t.Errorf("the unreachable sibling = %+v", o)
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if len(src.signers) != 1 || src.signers[0] != identity.FormatPublicKey(origin.PublicKey) {
		t.Errorf("the sibling answered for signers %v, want the asking certificate's key", src.signers)
	}
}
