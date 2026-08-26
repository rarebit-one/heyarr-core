package transfer_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/transfer"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/pieces"
)

// assertPublished checks the BYTES, not the outcome.
//
// An outcome is the transfer's account of itself. The published blob is the
// only thing that matters, and invariant 1 says a destination verifies it
// rather than believing a report about it.
func assertPublished(t *testing.T, d *destination, blob hashing.Hash, want []byte) {
	t.Helper()
	got, _, err := d.store.Open(t.Context(), blob)
	if err != nil {
		t.Fatalf("the assembled blob is not in the store: %v", err)
	}
	defer func() { _ = got.Close() }()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Error("the assembled blob is not the fixture's bytes")
	}
}

// unreachableAddr is an address nothing is listening on.
//
// A port that was bound and released, rather than one picked out of the air:
// the kernel has just told us it was free, so a dial to it is refused
// immediately instead of hanging until a firewall's timeout — which is the
// difference between a test that asserts something and a test that waits.
func unreachableAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// concurrencyProbe records whether two sources were ever serving AT THE SAME
// MOMENT, and makes the answer deterministic rather than lucky.
//
// # Why an overlap and not a timing
//
// Every other observable outcome of this change is identical for a sequential
// driver: the blob completes, both sources are attributed, the digest verifies.
// "It went faster" is a measurement, it is a property of the machine, and it is
// exactly the kind of assertion M5 learned not to write. The one thing that is
// true of sources used TOGETHER and false of sources used IN TURN is that two
// of them were inside a request at the same instant.
//
// # Why it blocks rather than samples
//
// Sampling an overlap is a race with the scheduler and would flake. Instead
// each source that arrives waits until `want` of them have arrived. A driver
// that fetches from several sources at once releases them all immediately; a
// driver that fetches from one source at a time never gets a second arrival, so
// the wait runs to its deadline and the test fails on a number rather than
// hanging.
type concurrencyProbe struct {
	mu   sync.Mutex
	cond *sync.Cond
	want int
	// in is how many sources are inside a request right now.
	in int
	// peak is the most that were ever inside one at the same time.
	peak int
	// deadline bounds the wait, so a sequential driver fails rather than hangs.
	deadline time.Duration
	expired  bool
}

func newConcurrencyProbe(want int) *concurrencyProbe {
	p := &concurrencyProbe{want: want, deadline: 5 * time.Second}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *concurrencyProbe) enter() {
	p.mu.Lock()
	p.in++
	if p.in > p.peak {
		p.peak = p.in
	}
	if p.in >= p.want {
		p.cond.Broadcast()
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	// The timer is outside the lock and wakes the waiters through the same
	// condition variable, so nothing here can wait forever.
	timer := time.AfterFunc(p.deadline, func() {
		p.mu.Lock()
		p.expired = true
		p.cond.Broadcast()
		p.mu.Unlock()
	})
	defer timer.Stop()

	p.mu.Lock()
	for p.in < p.want && !p.expired {
		p.cond.Wait()
	}
	p.mu.Unlock()
}

func (p *concurrencyProbe) leave() {
	p.mu.Lock()
	p.in--
	p.mu.Unlock()
}

func (p *concurrencyProbe) peakConcurrency() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak
}

// ---------------------------------------------------------------------------
// the assertion M6-02 exists for

// 🔴 Two sources are used TOGETHER, not in turn.
//
// This is §23's "while also", and it is the one claim a sequential driver
// cannot satisfy. Both sources hold everything, so a driver that used them in
// turn would still complete, would still verify, and — because the first source
// would run out of unclaimed pieces only when the blob was done — would attribute
// every piece to one of them. Completion proves nothing here. The overlap does.
func TestTwoSourcesAreUsedTogetherAndNotInTurn(t *testing.T) {
	content := pieceFixture(t, 8)
	blob := digestOfBytes(t, content)

	a := newNode(t, "peer-a", "a")
	b := newNode(t, "peer-b", "b")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(a.member(), b.member(), dst.member())

	probe := newConcurrencyProbe(2)
	ah := newPieceHolder(t, content, 0, 1, 2, 3, 4, 5, 6, 7)
	bh := newPieceHolder(t, content, 0, 1, 2, 3, 4, 5, 6, 7)
	ah.probe, bh.probe = probe, probe

	as := startPieceSource(t, a, root, ah)
	bs := startPieceSource(t, b, root, bh)

	d := newDestination(t, dst)
	out, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]transfer.Candidate{
			transfer.Peer(sourceFor(a, as.addr)),
			transfer.Peer(sourceFor(b, bs.addr)),
		})
	if err != nil {
		t.Fatalf("pulling from two sources: %v", err)
	}

	if peak := probe.peakConcurrency(); peak < 2 {
		t.Errorf("at most %d source was ever inside a request at once, want at least 2 — "+
			"the sources were used in turn, which is the shape M6 exists to replace", peak)
	}
	// Attribution, not merely completion. "It finished" passes for a sequential
	// implementation; "both of them served something" does not.
	if out.FromPeer[a.peerID] == 0 || out.FromPeer[b.peerID] == 0 {
		t.Errorf("the outcome attributes %v, and both sources must have served something",
			out.FromPeer)
	}
	if got := out.FromPeer[a.peerID] + out.FromPeer[b.peerID]; got != 8 {
		t.Errorf("%d pieces attributed across both sources, want 8: %v", got, out.FromPeer)
	}
	assertPublished(t, d, blob, content)
}

// 🔴 An unreachable participant is not fatal, and the outcome NAMES it.
//
// ADR-0038 says an unreachable peer is having an ordinary day, and ADR-0041 says
// a session makes progress with whoever it has. The failure mode this guards is
// the ordinary shape of concurrent code: a WaitGroup over all sources, an error
// channel that ends the session on the first failure, or a completion check that
// counts participants — each of which turns an ordinary day into an outage.
func TestADeadSourceIsNamedRatherThanFatal(t *testing.T) {
	content := pieceFixture(t, 4)
	blob := digestOfBytes(t, content)

	live := newNode(t, "peer-live", "live")
	dead := newNode(t, "peer-dead", "dead")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(live.member(), dead.member(), dst.member())

	liveHolder := newPieceHolder(t, content, 0, 1, 2, 3)
	ls := startPieceSource(t, live, root, liveHolder)

	// A source at an address nothing answers on. Not a peer that refuses — one
	// that is not there at all, which is what a site losing its link looks like.
	deadSource := sourceFor(dead, unreachableAddr(t))

	d := newDestination(t, dst)
	out, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]transfer.Candidate{
			transfer.Peer(sourceFor(live, ls.addr)),
			transfer.Peer(deadSource),
		})
	if err != nil {
		t.Fatalf("a session with one dead source failed: %v — an unreachable participant is "+
			"an ordinary day (ADR-0038), not a verdict", err)
	}
	if !slices.Contains(out.Unreachable, dead.peerID) {
		t.Errorf("the outcome does not name the dead source: %v", out.Unreachable)
	}
	if slices.Contains(out.Unreachable, live.peerID) {
		t.Errorf("the outcome names a source that served every piece as unreachable: %v",
			out.Unreachable)
	}
	if out.FromPeer[live.peerID] != 4 {
		t.Errorf("the live source served %d pieces, want 4: %v",
			out.FromPeer[live.peerID], out.FromPeer)
	}
	assertPublished(t, d, blob, content)
}

// 🔴 A source that dies MID-TRANSFER has its outstanding work redistributed.
//
// Skipping a source that was never there is easy. The case that matters is a
// source that answered, was given work on the strength of that, and then
// stopped — because its pieces are already assigned and nothing else will fetch
// them unless they are explicitly handed back.
func TestASourceThatDiesMidTransferIsRedistributed(t *testing.T) {
	content := pieceFixture(t, 8)
	blob := digestOfBytes(t, content)

	quitter := newNode(t, "peer-quitter", "quitter")
	steady := newNode(t, "peer-steady", "steady")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(quitter.member(), steady.member(), dst.member())

	// Both claim everything, so every piece the quitter abandons is one the
	// steady peer can still serve — which is the redistribution being asserted.
	quitterHolder := newPieceHolder(t, content, 0, 1, 2, 3, 4, 5, 6, 7)
	quitterHolder.failFrom = 2 // serves two, then refuses everything
	steadyHolder := newPieceHolder(t, content, 0, 1, 2, 3, 4, 5, 6, 7)

	qs := startPieceSource(t, quitter, root, quitterHolder)
	ss := startPieceSource(t, steady, root, steadyHolder)

	d := newDestination(t, dst)
	out, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]transfer.Candidate{
			transfer.Peer(sourceFor(quitter, qs.addr)),
			transfer.Peer(sourceFor(steady, ss.addr)),
		})
	if err != nil {
		t.Fatalf("a source dying mid-transfer failed the session: %v", err)
	}
	if out.FromPeer[quitter.peerID] != 2 {
		t.Errorf("the quitter is credited with %d pieces, want the 2 it served before it stopped: %v",
			out.FromPeer[quitter.peerID], out.FromPeer)
	}
	if out.FromPeer[steady.peerID] != 6 {
		t.Errorf("the steady source is credited with %d pieces, want the other 6 — the quitter's "+
			"outstanding work must be picked up, not abandoned: %v",
			out.FromPeer[steady.peerID], out.FromPeer)
	}
	// It contributed, so it is not called unreachable. A peer that had a bad
	// moment and a peer that was never there are different facts.
	if slices.Contains(out.Unreachable, quitter.peerID) {
		t.Errorf("a source that served two pieces is named unreachable: %v", out.Unreachable)
	}
	assertPublished(t, d, blob, content)
}

// membershipSet is a mutable roster a test can revoke a peer from mid-transfer.
//
// It stands in for the catalog rows Options.Members reads: removing a peer is
// the deletion of its membership record (ADR-0012), and snapshot is what the
// running session re-reads. It is mutex-guarded because a source's request
// handler revokes on one goroutine while the destination's session reads on
// another, and `-race` is right to want that serialised.
type membershipSet struct {
	mu      sync.Mutex
	members map[string]bool
}

func newMembershipSet(peerIDs ...string) *membershipSet {
	m := &membershipSet{members: map[string]bool{}}
	for _, id := range peerIDs {
		m.members[id] = true
	}
	return m
}

func (m *membershipSet) revoke(peerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.members, peerID)
}

func (m *membershipSet) snapshot(context.Context) (map[string]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]bool, len(m.members))
	for id, ok := range m.members {
		out[id] = ok
	}
	return out, nil
}

// 🔴 A peer revoked mid-transfer stops being fetched from, and its outstanding
// work is picked up by the sources that remain (#290, §26, ADR-0012, ADR-0038).
//
// The session reads membership ONCE at survey time and then re-surveys those
// candidates as it runs — but until #290 it never re-read WHO they are, so a
// peer whose membership record was deleted mid-transfer went on being asked for
// pieces, and went on answering, until the transfer ended. #265 asked for this
// and it was not delivered: a revoked peer that keeps receiving until the end is
// a revocation that did not happen.
func TestARevokedPeerStopsBeingFetchedFromMidTransfer(t *testing.T) {
	content := pieceFixture(t, 8)
	blob := digestOfBytes(t, content)

	revoked := newNode(t, "peer-revoked", "revoked")
	steady := newNode(t, "peer-steady", "steady")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(revoked.member(), steady.member(), dst.member())

	// The revoked peer holds the whole blob and is the only source at the
	// survey; the steady peer starts empty and gains everything the instant the
	// revocation happens. So the pieces the revoked peer had not yet served are
	// there for the steady peer to pick up — the redistribution #280 built for a
	// source that dies mid-transfer, reached through the revocation path rather
	// than a second one.
	revokedHolder := newPieceHolder(t, content, 0, 1, 2, 3, 4, 5, 6, 7)
	steadyHolder := newPieceHolder(t, content)

	roster := newMembershipSet(revoked.peerID, steady.peerID)
	revokedHolder.onServed = func(int) {
		if len(revokedHolder.servedPieces()) == 2 {
			roster.revoke(revoked.peerID)
			steadyHolder.gain(0, 1, 2, 3, 4, 5, 6, 7)
		}
	}

	rs := startPieceSource(t, revoked, root, revokedHolder)
	ss := startPieceSource(t, steady, root, steadyHolder)

	store, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	puller, err := transfer.New(transfer.Options{
		Material: dst.material, Store: store, Members: roster.snapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	d := &destination{puller: puller, store: store}

	out, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]transfer.Candidate{
			transfer.Peer(sourceFor(revoked, rs.addr)),
			transfer.Peer(sourceFor(steady, ss.addr)),
		})
	if err != nil {
		t.Fatalf("a revocation mid-transfer failed the session: %v", err)
	}
	// It served two before it was revoked and not one more. The membership
	// check is at the TOP of the fetch loop, so the piece in flight when the
	// record vanished lands and is kept, and the NEXT is never asked for. A
	// revoked peer that keeps receiving until the transfer ends is the exact bug
	// #290 names, and this is the number that fails when the check is removed.
	if got := out.FromPeer[revoked.peerID]; got != 2 {
		t.Errorf("the revoked peer served %d pieces, want the 2 it served before revocation — "+
			"a revoked peer must stop being fetched from within one generation: %v",
			got, out.FromPeer)
	}
	// The other six are the revoked peer's outstanding work, picked up rather
	// than abandoned.
	if got := out.FromPeer[steady.peerID]; got != 6 {
		t.Errorf("the steady peer served %d pieces, want the other 6 — the revoked peer's "+
			"outstanding work must be redistributed, not abandoned: %v", got, out.FromPeer)
	}
	// It contributed, so it is not named unreachable: a peer that was revoked
	// after serving two pieces and a peer that was never there are different
	// facts (ADR-0041).
	if slices.Contains(out.Unreachable, revoked.peerID) {
		t.Errorf("a peer that served two pieces before revocation is named unreachable: %v",
			out.Unreachable)
	}
	// Bytes already received from the revoked peer are KEPT: the whole object is
	// verified at Publish against a digest that did not come from it (invariant
	// 1, ADR-0043), so its two pieces are part of the blob rather than discarded.
	assertPublished(t, d, blob, content)
}

// An unenrolled peer that dials is refused at the piece handshake, not with a
// status code (#290's reverse direction, ADR-0012).
//
// This node must stop FETCHING from a revoked peer — the test above — and it
// must also stop SERVING to one, which is mTLS's job at the connection rather
// than the session's. The session for a stranger never comes into being:
// asserting it here is the cheap insurance #290 asks for that nothing opened a
// second door.
func TestAnUnenrolledPeerIsRefusedAtThePieceHandshake(t *testing.T) {
	content := pieceFixture(t, 4)
	blob := digestOfBytes(t, content)

	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	// The source's listener does not know the destination: an unenrolled dialer.
	sourceRoot := newTrustRoot(src.member())

	holder := newPieceHolder(t, content, 0, 1, 2, 3)
	s := startPieceSource(t, src, sourceRoot, holder)

	d := newDestination(t, dst)
	out, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]transfer.Candidate{transfer.Peer(sourceFor(src, s.addr))})
	if !errors.Is(err, transfer.ErrNoPieceSource) {
		t.Fatalf("a stranger read pieces from a source that does not pin it: err=%v", err)
	}
	// It could not even be surveyed, so it is named unreachable rather than
	// credited — the handshake failed before any piece route answered.
	if !slices.Contains(out.Unreachable, src.peerID) {
		t.Errorf("the refused source is not named unreachable: %v", out.Unreachable)
	}
	if served := holder.servedPieces(); len(served) != 0 {
		t.Errorf("a source that refused the handshake still served %d pieces: %v",
			len(served), served)
	}
}

// 🔴 A peer that acquires pieces DURING the session becomes a source of them.
//
// This is the difference between a swarm and two independent pulls, and it is
// invisible in any test where the sources start full. Two peers that begin at
// the same time both hold nothing at the survey; if the survey is the only
// question ever asked, they never exchange anything and §23's diagram is
// decoration.
func TestAPeerThatFillsUpDuringTheSessionIsAskedAgain(t *testing.T) {
	content := pieceFixture(t, 6)
	blob := digestOfBytes(t, content)

	late := newNode(t, "peer-late", "late")
	slow := newNode(t, "peer-slow", "slow")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(late.member(), slow.member(), dst.member())

	// The late peer starts holding NOTHING, so the opening survey finds it
	// useless. It gains the last two pieces once the slow peer has served one.
	lateHolder := newPieceHolder(t, content)
	slowHolder := newPieceHolder(t, content, 0, 1, 2, 3)
	slowHolder.onServed = func(int) { lateHolder.gain(4, 5) }

	ls := startPieceSource(t, late, root, lateHolder)
	ss := startPieceSource(t, slow, root, slowHolder)

	d := newDestination(t, dst)
	out, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]transfer.Candidate{
			transfer.Peer(sourceFor(late, ls.addr)),
			transfer.Peer(sourceFor(slow, ss.addr)),
		})
	if err != nil {
		t.Fatalf("pulling with a peer that fills up mid-session: %v", err)
	}
	if out.FromPeer[late.peerID] != 2 {
		t.Errorf("the peer that acquired pieces mid-session served %d of them, want 2 — a "+
			"one-shot survey never asks again, and two peers starting together would then "+
			"never exchange anything at all: %v", out.FromPeer[late.peerID], out.FromPeer)
	}
	assertPublished(t, d, blob, content)
}

// Completion is the digest, and never the participants.
//
// A session completes while a participant is still unreachable, and no code
// path waits for one. The dead source here holds pieces nobody else does —
// except it does not, because it is dead — so the assertion is that the session
// ends on the bitset rather than on the source set.
func TestCompletionIsTheDigestAndNotTheParticipants(t *testing.T) {
	content := pieceFixture(t, 4)
	blob := digestOfBytes(t, content)

	live := newNode(t, "peer-live", "live")
	gone := newNode(t, "peer-gone", "gone")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(live.member(), gone.member(), dst.member())

	liveHolder := newPieceHolder(t, content, 0, 1, 2, 3)
	ls := startPieceSource(t, live, root, liveHolder)

	done := make(chan error, 1)
	d := newDestination(t, dst)
	go func() {
		_, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
			[]transfer.Candidate{
				transfer.Peer(sourceFor(live, ls.addr)),
				transfer.Peer(sourceFor(gone, unreachableAddr(t))),
			})
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the session did not complete: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the session never completed with an unreachable participant present — " +
			"something is waiting for a peer rather than for the digest")
	}
	assertPublished(t, d, blob, content)
}

// Nobody holding anything is still ErrNoPieceSource, and the sources that could
// not answer are named rather than swallowed.
func TestASessionWithNothingToFetchNamesWhoCouldNotAnswer(t *testing.T) {
	content := pieceFixture(t, 4)
	blob := digestOfBytes(t, content)

	gone := newNode(t, "peer-gone", "gone")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(gone.member(), dst.member())

	d := newDestination(t, dst)
	out, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]transfer.Candidate{transfer.Peer(sourceFor(gone, unreachableAddr(t)))})
	if !errors.Is(err, transfer.ErrNoPieceSource) {
		t.Fatalf("pulling with no usable source returned %v, want ErrNoPieceSource", err)
	}
	if !slices.Contains(out.Unreachable, gone.peerID) {
		t.Errorf("the outcome does not name the source that never answered: %v", out.Unreachable)
	}
	_ = root
}

// ---------------------------------------------------------------------------
// ADR-0043's ordering, asserted rather than asserted-about

// failingWriteStore is a content store whose staging file refuses ONE offset.
//
// It exists to make a claim testable that was otherwise only stated: the bitset
// is written AFTER the bytes. Everything else about a piece transfer looks
// identical under either order, because the whole-object hash catches a wrong
// blob either way — the difference only shows when a write FAILS, and then it
// shows as a record claiming a piece that is not there.
type failingWriteStore struct {
	*cas.FS
	refuseOffset int64
}

func (s *failingWriteStore) OpenPartial(
	ctx context.Context, expected hashing.Hash,
) (cas.Partial, error) {
	p, err := s.FS.OpenPartial(ctx, expected)
	if err != nil {
		return nil, err
	}
	return &failingWritePartial{Partial: p, refuseOffset: s.refuseOffset}, nil
}

type failingWritePartial struct {
	cas.Partial
	refuseOffset int64
}

func (p *failingWritePartial) WriteAt(b []byte, off int64) (int, error) {
	if off == p.refuseOffset {
		return 0, errors.New("staging file refused this write")
	}
	return p.Partial.WriteAt(b, off)
}

// 🔴 A piece whose bytes did not land is NOT recorded as landed.
//
// ADR-0043 permits sparse writes precisely because a hole is distinguishable
// from received data — the bitset says the piece never arrived. That property
// is the whole safety argument, and it holds only while the record is written
// after the bytes. Reversed, the record is a claim about bytes that may not
// exist, and this node would go on to SERVE that range to somebody else, which
// is the one failure mode in this design that hurts a machine other than this
// one.
func TestAPieceWhoseBytesDidNotLandIsNotRecordedAsLanded(t *testing.T) {
	content := pieceFixture(t, 4)
	blob := digestOfBytes(t, content)

	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	holder := newPieceHolder(t, content, 0, 1, 2, 3)
	s := startPieceSource(t, src, root, holder)

	base, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g, err := pieces.For(int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	refusedIndex := 1
	off, _, err := g.Range(refusedIndex)
	if err != nil {
		t.Fatal(err)
	}
	store := &failingWriteStore{FS: base, refuseOffset: off}
	puller, err := transfer.New(transfer.Options{Material: dst.material, Store: store})
	if err != nil {
		t.Fatal(err)
	}

	// A staging file this node cannot write ends the session: no other source
	// can fix it and every retry fails identically. Asking somebody else, or
	// coming back to it after a re-survey, is how one refused offset turns into
	// a piece fetched until the machine runs out of ephemeral ports — which is
	// what it did before this was distinguished from a source's refusal.
	if _, err := puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]transfer.Candidate{transfer.Peer(sourceFor(src, s.addr))}); err == nil {
		t.Fatal("a transfer whose staging file refused a write reported success")
	}
	if served := len(holder.servedPieces()); served > 4 {
		t.Errorf("the source was asked for %d pieces for a four-piece blob — a piece this node "+
			"could not store was asked for again and again", served)
	}

	encoded, err := store.LoadPieceProgress(blob)
	if err != nil {
		t.Fatalf("reading back the progress record: %v", err)
	}
	if encoded == "" {
		t.Skip("no progress record was kept, so there is nothing here that could lie")
	}
	_, have, err := pieces.Decode(encoded)
	if err != nil {
		t.Fatalf("the progress record does not decode: %v", err)
	}
	if have.Has(refusedIndex) {
		t.Errorf("the record claims piece %d, whose bytes were refused — the bitset was written "+
			"before the bytes, and this node would now serve a range it does not hold (ADR-0043)",
			refusedIndex)
	}
	// The piece before it did land, so the record is a record and not simply
	// empty — an assertion that nothing is claimed would pass for a session
	// that recorded nothing at all.
	if !have.Has(0) {
		t.Error("the record does not claim piece 0, whose bytes did land, so this test would " +
			"pass against a transfer that recorded nothing")
	}
}
