package placementsync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/peer/inventory"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

func hashOf(first byte) string {
	return "blake3:" + strings.Repeat(string(first), 64)
}

// TestRangesPartitionTheHashSpace: every digest falls in exactly one range, so
// a cycle that walks Ranges() asks about every blob once.
func TestRangesPartitionTheHashSpace(t *testing.T) {
	t.Parallel()
	ranges := Ranges()
	if len(ranges) != 16 {
		t.Fatalf("len(Ranges()) = %d, want 16", len(ranges))
	}
	for _, c := range "0123456789abcdef" {
		h := hashOf(byte(c))
		n := 0
		for _, r := range ranges {
			if err := r.Validate(); err != nil {
				t.Fatalf("range %s: %v", r, err)
			}
			if r.Contains(h) {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%s is in %d ranges, want exactly 1", h, n)
		}
	}
}

func TestRangeValidate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		r  Range
		ok bool
	}{
		{Range{Lo: "0", Hi: "1"}, true},
		{Range{Lo: "f"}, true},
		{Range{Lo: "", Hi: ""}, true},
		{Range{Lo: "1", Hi: "1"}, false},
		{Range{Lo: "2", Hi: "1"}, false},
		{Range{Lo: "A"}, false},
		{Range{Lo: "0", Hi: "g"}, false},
	} {
		if err := tc.r.Validate(); (err == nil) != tc.ok {
			t.Errorf("Range%+v.Validate() = %v, want ok=%v", tc.r, err, tc.ok)
		}
	}
}

func TestDigestIsOrderIndependentAndNamesTheEmptySet(t *testing.T) {
	t.Parallel()
	a := []Entry{{BlobHash: hashOf('a'), Size: 1}, {BlobHash: hashOf('b'), Size: 2}}
	b := []Entry{a[1], a[0]}
	if Digest(a) != Digest(b) {
		t.Fatal("the digest depends on order")
	}
	if Digest(nil) == "" || Digest(nil) == Digest(a) {
		t.Fatalf("the empty set's digest %q must be non-empty and distinct", Digest(nil))
	}
	c := []Entry{{BlobHash: hashOf('a'), Size: 1}, {BlobHash: hashOf('b'), Size: 3}}
	if Digest(a) == Digest(c) {
		t.Fatal("a size change does not change the digest")
	}
}

func TestAskValidateRefusesAClaimOutsideItsRange(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	ok := Ask{
		Range: Range{Lo: "a", Hi: "b"}, Digest: Digest(nil), ObservedAt: now, Claim: true,
		Held: []inventory.Entry{{BlobHash: hashOf('a'), State: inventory.StatePresent, BytesPresent: 1}},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a valid ask: %v", err)
	}
	outside := ok
	outside.Held = []inventory.Entry{{BlobHash: hashOf('c'), State: inventory.StatePresent}}
	if err := outside.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a held claim outside the range: err = %v, want ErrInvalid", err)
	}
	unclaimed := ok
	unclaimed.Claim = false
	if err := unclaimed.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("held entries without a claim: err = %v, want ErrInvalid", err)
	}
	missing := ok
	missing.Held = []inventory.Entry{{BlobHash: hashOf('a'), State: inventory.StateMissing}}
	if err := missing.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a claim listing a missing blob: err = %v, want ErrInvalid", err)
	}
	noDigest := ok
	noDigest.Digest = ""
	if err := noDigest.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an ask with no digest: err = %v, want ErrInvalid", err)
	}
}

func TestOfferValidate(t *testing.T) {
	t.Parallel()
	r := Range{Lo: "a", Hi: "b"}
	in := []Entry{{BlobHash: hashOf('a'), Size: 4}}
	for _, tc := range []struct {
		name string
		o    Offer
		ok   bool
	}{
		{"whole set", Offer{Range: r, Digest: Digest(in), Entries: in}, true},
		{"unchanged", Offer{Range: r, Digest: Digest(in), Unchanged: true}, true},
		{"unchanged with entries", Offer{Range: r, Digest: Digest(in), Unchanged: true, Entries: in}, false},
		{"another range", Offer{Range: Range{Lo: "b", Hi: "c"}, Digest: Digest(in), Entries: in}, false},
		{"outside the range", Offer{
			Range: r, Digest: Digest([]Entry{{BlobHash: hashOf('c')}}),
			Entries: []Entry{{BlobHash: hashOf('c')}},
		}, false},
		{"digest lies", Offer{Range: r, Digest: Digest(nil), Entries: in}, false},
		{"twice", Offer{Range: r, Digest: Digest(append(in, in...)), Entries: append(in, in...)}, false},
		{"not a digest", Offer{
			Range: r, Digest: Digest([]Entry{{BlobHash: "blake3:zz"}}),
			Entries: []Entry{{BlobHash: "blake3:zz"}},
		}, false},
	} {
		if err := tc.o.Validate(r); (err == nil) != tc.ok {
			t.Errorf("%s: Validate = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// fakeStore is LocalStore over maps: what was taken per source, and what this
// node holds.
type fakeStore struct {
	taken    map[string]map[string]Entry
	holds    map[string]Entry
	replaced int
	fail     error
}

func newFakeStore() *fakeStore {
	return &fakeStore{taken: map[string]map[string]Entry{}, holds: map[string]Entry{}}
}

func (f *fakeStore) TakenPlacements(_ context.Context, source string, r Range) ([]Entry, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	var out []Entry
	for h, e := range f.taken[source] {
		if r.Contains(h) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeStore) HeldClaim(_ context.Context, r Range) ([]Entry, error) {
	var out []Entry
	for h, e := range f.holds {
		if r.Contains(h) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeStore) ConfirmOffered(context.Context, string, Range) error { return nil }

func (f *fakeStore) ReplacePlacements(_ context.Context, source string, r Range, entries []Entry) (int, int, error) {
	f.replaced++
	if f.taken[source] == nil {
		f.taken[source] = map[string]Entry{}
	}
	want := map[string]Entry{}
	added, removed := 0, 0
	for _, e := range entries {
		want[e.BlobHash] = e
		if _, ok := f.taken[source][e.BlobHash]; !ok {
			added++
		}
		f.taken[source][e.BlobHash] = e
	}
	for h := range f.taken[source] {
		if _, ok := want[h]; !ok && r.Contains(h) {
			delete(f.taken[source], h)
			removed++
		}
	}
	return added, removed, nil
}

// fakeSource answers as a source would: its offered set per asker, and the held
// claims it was sent.
type fakeSource struct {
	offered map[string][]Entry // peer → offered entries
	asks    []Ask
	down    map[string]bool
}

func (f *fakeSource) Ask(_ context.Context, t Target, a Ask) (Offer, error) {
	if f.down[t.Peer.PeerID] {
		return Offer{}, errors.New("connection refused")
	}
	f.asks = append(f.asks, a)
	var in []Entry
	for _, e := range f.offered[t.Peer.PeerID] {
		if a.Contains(e.BlobHash) {
			in = append(in, e)
		}
	}
	o := Offer{Range: a.Range, Digest: Digest(in)}
	if o.Digest == a.Digest {
		o.Unchanged = true
		return o, nil
	}
	o.Entries = in
	return o, nil
}

func targetsOf(ids ...string) Targets {
	return func(context.Context) ([]Target, error) {
		var out []Target
		for _, id := range ids {
			out = append(out, Target{Peer: mtls.Peer{PeerID: id}, Endpoint: "https://" + id})
		}
		return out, nil
	}
}

// TestPullerTakesThenConverges: the first cycle takes what the sibling offers;
// the second, with nothing changed, writes nothing and costs one digest per
// range; a withdrawal is taken away; a pulled blob's claim is sent once.
func TestPullerTakesThenConverges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newFakeStore()
	a, b := hashOf('a'), hashOf('b')
	src := &fakeSource{offered: map[string][]Entry{"site-a": {{BlobHash: a, Size: 10, Held: true}, {BlobHash: b, Size: 20, Held: true}}}}
	p := NewPuller(store, src, targetsOf("site-a"), func() time.Time { return time.Unix(1_700_000_000, 0) }, nil)

	sum, err := p.Cycle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 16 || sum.Added != 2 || sum.Changed != 2 {
		t.Fatalf("first cycle = %+v, want 16 asked, 2 added in 2 ranges", sum)
	}

	store.replaced = 0
	sum, err = p.Cycle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if store.replaced != 0 || sum.Changed != 0 {
		t.Fatalf("an unchanged sibling rewrote %d ranges (%+v)", store.replaced, sum)
	}

	// The blob arrives here: its range resends a complete claim, once.
	store.holds[a] = Entry{BlobHash: a, Size: 10, Held: true}
	src.asks = nil
	if _, err := p.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	var claims []inventory.Entry
	claimed := 0
	for _, ask := range src.asks {
		if ask.Claim {
			claimed++
		}
		claims = append(claims, ask.Held...)
	}
	if claimed != 1 || len(claims) != 1 || claims[0].BlobHash != a || claims[0].State != inventory.StatePresent {
		t.Fatalf("claims sent = %d with %+v, want one claim listing %s present", claimed, claims, a)
	}
	src.asks = nil
	if _, err := p.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	for _, ask := range src.asks {
		if ask.Claim {
			t.Fatalf("an unchanged claim was resent: %+v", ask.Held)
		}
	}

	// The sibling loses its copy of a but still wants it kept: the entry stays,
	// with Held false, and is not withdrawn here.
	src.offered["site-a"][0].Held = false
	sum, err = p.Cycle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Removed != 0 {
		t.Fatalf("a source losing its copy withdrew the placement (%+v)", sum)
	}
	if e, ok := store.taken["site-a"][a]; !ok || e.Held {
		t.Fatalf("taken[a] = %+v, %v, want kept with the source's hold false", e, ok)
	}

	// The sibling withdraws b.
	src.offered["site-a"] = src.offered["site-a"][:1]
	sum, err = p.Cycle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Removed != 1 {
		t.Fatalf("withdrawal cycle = %+v, want 1 removed", sum)
	}
	if _, still := store.taken["site-a"][b]; still {
		t.Fatal("a withdrawn placement was kept")
	}
}

// TestPullerCarriesOnPastAnUnreachableSibling (ADR-0038): a sibling that cannot
// be asked is counted, and the next one is still asked.
func TestPullerCarriesOnPastAnUnreachableSibling(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	src := &fakeSource{
		offered: map[string][]Entry{"site-c": {{BlobHash: hashOf('c'), Size: 1, Held: true}}},
		down:    map[string]bool{"site-b": true},
	}
	p := NewPuller(store, src, targetsOf("site-b", "site-c"), nil, nil)
	sum, err := p.Cycle(context.Background())
	if err != nil {
		t.Fatalf("an unreachable sibling failed the cycle: %v", err)
	}
	if sum.Unreachable != 1 || sum.Added != 1 {
		t.Fatalf("cycle = %+v, want 1 unreachable and 1 added from the other sibling", sum)
	}
}

// TestPullerStopsOnALocalFailure: this node's own store failing is the one
// thing that fails the cycle; it is not blamed on a sibling.
func TestPullerStopsOnALocalFailure(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.fail = errors.New("disk I/O error")
	p := NewPuller(store, &fakeSource{}, targetsOf("site-b"), nil, nil)
	if _, err := p.Cycle(context.Background()); err == nil {
		t.Fatal("a local store failure did not fail the cycle")
	}
}

type lyingSource struct{}

func (lyingSource) Ask(_ context.Context, _ Target, a Ask) (Offer, error) {
	return Offer{Range: a.Range, Digest: Digest([]Entry{{BlobHash: hashOf('a'), Size: 1}}), Unchanged: true}, nil
}

// TestAnUnchangedAnswerMustMatchTheAskersDigest: "unchanged" is a claim the
// source's set is the one this node took. A digest that contradicts it is
// refused, not trusted, and counted against that sibling.
func TestAnUnchangedAnswerMustMatchTheAskersDigest(t *testing.T) {
	t.Parallel()
	p := NewPuller(newFakeStore(), lyingSource{}, targetsOf("site-b"), nil, nil)
	sum, err := p.Cycle(context.Background())
	if err != nil {
		t.Fatalf("a sibling's bad answer failed the cycle: %v", err)
	}
	if sum.Unreachable != 1 || sum.Asked != 0 {
		t.Fatalf("cycle = %+v, want the lying sibling refused and counted", sum)
	}
}
