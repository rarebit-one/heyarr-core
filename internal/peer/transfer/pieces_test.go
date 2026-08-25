package transfer_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/peer/transfer"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/pieces"
)

// ---------------------------------------------------------------------------
// the fixture

// pieceHolder is a peer that holds SOME pieces of a blob and will serve only
// those.
//
// It refuses a piece it does not advertise, rather than serving it anyway,
// because a fixture whose sources can always answer cannot tell a puller that
// reads availability from one that ignores it and asks everybody for
// everything.
type pieceHolder struct {
	geometry pieces.Geometry
	content  []byte
	claim    pieces.Availability

	// served records every piece this holder answered.
	served []int
	// order, when shared between holders, records every fetch across ALL of
	// them as "<peer>:<index>". A per-holder record cannot express "this piece
	// was fetched BEFORE that one from a different peer", which is exactly what
	// a rarest-first assertion is about.
	order *[]string
	// id is this holder's peer id, for the shared order.
	id string
	// corrupt, when set, is applied to a piece's bytes before serving —
	// a peer that answers 200 with bytes it cannot back.
	corrupt func(index int, b []byte) []byte
	// shortenPiece, when set, is an index this holder serves at the wrong
	// length.
	shortenPiece int
}

func newPieceHolder(t *testing.T, content []byte, indices ...int) *pieceHolder {
	t.Helper()
	g, err := pieces.For(int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	claim := pieces.NewAvailability(g.Count())
	for _, i := range indices {
		claim.Add(i)
	}
	return &pieceHolder{geometry: g, content: content, claim: claim, shortenPiece: -1}
}

func (h *pieceHolder) PieceAvailability(
	_ context.Context, _ hashing.Hash,
) (string, bool, error) {
	return pieces.Encode(h.geometry, h.claim), true, nil
}

func (h *pieceHolder) ReadPiece(
	_ context.Context, _ hashing.Hash, index int,
) ([]byte, error) {
	if !h.claim.Has(index) {
		return nil, peerapi.ErrNoSuchPiece
	}
	h.served = append(h.served, index)
	if h.order != nil {
		*h.order = append(*h.order, fmt.Sprintf("%s:%d", h.id, index))
	}
	off, length, err := h.geometry.Range(index)
	if err != nil {
		return nil, peerapi.ErrNoSuchPiece
	}
	out := append([]byte(nil), h.content[off:off+length]...)
	if h.corrupt != nil {
		out = h.corrupt(index, out)
	}
	if index == h.shortenPiece {
		out = out[:len(out)-1]
	}
	return out, nil
}

// startPieceSource brings up a peer serving the piece routes and nothing else.
func startPieceSource(
	t *testing.T, self *node, members mtls.Membership, holder *pieceHolder,
) *sourceNode {
	t.Helper()
	srv, err := peerapi.New(peerapi.Options{
		Addr:       "127.0.0.1:0",
		Material:   self.material,
		Members:    members,
		SelfPeerID: self.peerID,
		Pieces:     holder,
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutting a piece source down: %v", err)
		}
	})
	return &sourceNode{self: self, addr: srv.Addr()}
}

func sourceFor(n *node, addr string) replication.Source {
	return replication.Source{
		PeerID:    n.peerID,
		Name:      n.name,
		Endpoint:  "https://" + addr,
		PublicKey: n.member().PublicKey,
		Health:    "healthy",
	}
}

// pieceFixture is content whose bytes are not uniform, so a piece written at
// the wrong offset produces a different blob rather than an identical one.
func pieceFixture(t *testing.T, wantPieces int) []byte {
	t.Helper()
	size := wantPieces * pieces.MinPieceLength
	// A fixed seed: the bytes must differ from each other and be the same on
	// every run and every platform.
	r := rand.New(rand.NewSource(0x5EED))
	b := make([]byte, size)
	if _, err := r.Read(b); err != nil {
		t.Fatal(err)
	}
	g, err := pieces.For(int64(size))
	if err != nil {
		t.Fatal(err)
	}
	if g.Count() != wantPieces {
		t.Fatalf("the fixture divides into %d pieces, not the %d this test reasons about",
			g.Count(), wantPieces)
	}
	return b
}

func digestOfBytes(t *testing.T, b []byte) hashing.Hash {
	t.Helper()
	h := hashing.New()
	if _, err := h.Write(b); err != nil {
		t.Fatal(err)
	}
	return h.Sum()
}

// ---------------------------------------------------------------------------
// the assertion this whole milestone exists for

// 🔴 A blob is assembled from two peers that EACH HOLD HALF, and neither could
// have served it alone.
//
// This is §23. Before it, a peer holding part of a blob was indistinguishable
// from one holding none, so two peers both still fetching the same blob had
// nothing to say to each other and the only available shape was
// Internet → A, then A → B.
//
// The disjointness is the point and is asserted: if either holder is ever asked
// for a piece it does not hold it refuses, so a pull that ignored availability
// could not complete.
func TestABlobIsAssembledFromTwoPeersThatEachHoldHalf(t *testing.T) {
	content := pieceFixture(t, 8)
	blob := digestOfBytes(t, content)

	left := newNode(t, "peer-left", "left")
	right := newNode(t, "peer-right", "right")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(left.member(), right.member(), dst.member())

	// Disjoint halves, interleaved rather than split down the middle: a
	// contiguous split would let a puller that fetched a prefix from one peer
	// and a suffix from the other look correct while ignoring both bitsets.
	leftHolder := newPieceHolder(t, content, 0, 2, 4, 6)
	rightHolder := newPieceHolder(t, content, 1, 3, 5, 7)
	l := startPieceSource(t, left, root, leftHolder)
	r := startPieceSource(t, right, root, rightHolder)

	d := newDestination(t, dst)
	out, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]replication.Source{sourceFor(left, l.addr), sourceFor(right, r.addr)})
	if err != nil {
		t.Fatalf("assembling from two half-holders: %v", err)
	}

	if out.Fetched != 8 {
		t.Errorf("fetched %d pieces, want 8", out.Fetched)
	}
	if len(out.FromPeer) != 2 {
		t.Errorf("the blob came from %d peers, want 2 — one peer could not have served it",
			len(out.FromPeer))
	}
	if out.FromPeer[left.peerID] != 4 || out.FromPeer[right.peerID] != 4 {
		t.Errorf("pieces came %d from left and %d from right, want 4 and 4",
			out.FromPeer[left.peerID], out.FromPeer[right.peerID])
	}

	// The bytes, not just the outcome.
	got, _, err := d.store.Open(t.Context(), blob)
	if err != nil {
		t.Fatalf("the assembled blob is not in the store: %v", err)
	}
	defer func() { _ = got.Close() }()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Error("the assembled blob is not the fixture's bytes")
	}
}

// A source that divides the blob differently is not a source for it.
//
// Two peers computing different piece lengths are talking about different byte
// ranges under the same index, and bytes fetched that way fail verification for
// a reason nobody could diagnose. The disagreement is caught at the survey, so
// nothing is ever fetched from that peer.
func TestAPeerThatDividesTheBlobDifferentlyIsNotUsed(t *testing.T) {
	content := pieceFixture(t, 8)
	blob := digestOfBytes(t, content)

	odd := newNode(t, "peer-odd", "odd")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(odd.member(), dst.member())

	// A peer holding a DIFFERENT, LARGER blob, described honestly under its own
	// geometry. It can serve pieces 0-7 perfectly well, at exactly the length
	// this node expects — For scales the piece length only past a thousand
	// pieces, so both divisions use the same one. The ONLY thing wrong is that
	// the bytes belong to another blob.
	//
	// That is what makes this test capable of failing. An earlier version gave
	// the odd peer a size it could not actually serve, so the pull failed
	// because the fixture broke rather than because the geometry check ran —
	// and deleting the check left the test passing. Sabotage caught it.
	other := pieceFixture(t, 32)
	holder := newPieceHolder(t, other, 0, 1, 2, 3, 4, 5, 6, 7)
	if holder.geometry.PieceLength != mustGeometry(t, int64(len(content))).PieceLength {
		t.Fatal("the fixtures divide into different piece LENGTHS, so the length check would " +
			"catch this peer and the geometry check would not be what is being tested")
	}
	o := startPieceSource(t, odd, root, holder)

	d := newDestination(t, dst)
	_, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]replication.Source{sourceFor(odd, o.addr)})
	if !errors.Is(err, transfer.ErrNoPieceSource) {
		t.Fatalf("err = %v, want ErrNoPieceSource — a peer with a different geometry is not a "+
			"source, and must be dropped at the survey rather than caught at Publish", err)
	}
	if len(holder.served) != 0 {
		t.Errorf("%d pieces were fetched from a peer that divides the blob differently",
			len(holder.served))
	}
}

// mustGeometry is For, for a fixture that must divide.
func mustGeometry(t *testing.T, size int64) pieces.Geometry {
	t.Helper()
	g, err := pieces.For(size)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// A piece served at the wrong length is refused rather than written.
//
// This is the one write that could corrupt a range belonging to a DIFFERENT
// piece, so the length is checked before WriteAt and not after.
func TestAPieceOfTheWrongLengthIsNotWritten(t *testing.T) {
	content := pieceFixture(t, 4)
	blob := digestOfBytes(t, content)

	bad := newNode(t, "peer-bad", "bad")
	good := newNode(t, "peer-good", "good")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(bad.member(), good.member(), dst.member())

	badHolder := newPieceHolder(t, content, 0, 1, 2, 3)
	badHolder.shortenPiece = 2
	goodHolder := newPieceHolder(t, content, 0, 1, 2, 3)
	b := startPieceSource(t, bad, root, badHolder)
	g := startPieceSource(t, good, root, goodHolder)

	d := newDestination(t, dst)
	out, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]replication.Source{sourceFor(bad, b.addr), sourceFor(good, g.addr)})
	if err != nil {
		t.Fatalf("a short piece from one peer should be survivable with another: %v", err)
	}
	if err := d.store.Verify(t.Context(), blob); err != nil {
		t.Errorf("the assembled blob does not verify: %v", err)
	}
	if out.FromPeer[bad.peerID] > 3 {
		t.Errorf("the short-serving peer contributed %d pieces, so the short one was counted",
			out.FromPeer[bad.peerID])
	}
}

// 🔴 A peer that serves plausible bytes it cannot back fails the WHOLE-object
// check, and everything is discarded.
//
// There are no piece hashes and adding them would not help — a peer publishing
// a hash for its own bytes verifies its own garbage (ADR-0043). The blob's
// BLAKE3 digest is the only statement not made by the peer serving the bytes,
// so this is where a lying peer is caught, and it is caught at the end.
//
// The staging file and the bitset must BOTH go: resuming from a record that
// produced a bad blob would reproduce it.
func TestBytesAPeerCannotBackFailTheWholeObjectCheckAndDiscardEverything(t *testing.T) {
	content := pieceFixture(t, 4)
	blob := digestOfBytes(t, content)

	liar := newNode(t, "peer-liar", "liar")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(liar.member(), dst.member())

	holder := newPieceHolder(t, content, 0, 1, 2, 3)
	// Right length, wrong bytes, in the middle: everything local looks fine
	// until the whole object is hashed.
	holder.corrupt = func(index int, b []byte) []byte {
		if index == 2 {
			b[len(b)/2] ^= 0xFF
		}
		return b
	}
	l := startPieceSource(t, liar, root, holder)

	d := newDestination(t, dst)
	_, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]replication.Source{sourceFor(liar, l.addr)})
	if err == nil {
		t.Fatal("a blob assembled from bytes the peer could not back was accepted")
	}
	// The message must say what actually happened: not which peer, because it
	// cannot know, but that it cannot know.
	if !bytes.Contains([]byte(err.Error()), []byte("no way to say which")) {
		t.Errorf("the failure does not admit it cannot attribute the bad piece: %v", err)
	}

	if held, _ := d.store.Has(t.Context(), blob); held {
		t.Error("the store holds a blob that did not verify")
	}
	if encoded, _ := d.store.LoadPieceProgress(blob); encoded != "" {
		t.Error("the bitset survived a failed verification, so a resume would reproduce the bad blob")
	}
}

// A pull resumes from the bitset a previous attempt left, and does not refetch
// what landed.
func TestAPiecePullResumesFromItsBitset(t *testing.T) {
	content := pieceFixture(t, 8)
	blob := digestOfBytes(t, content)

	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	holder := newPieceHolder(t, content, 0, 1, 2, 3, 4, 5, 6, 7)
	s := startPieceSource(t, src, root, holder)
	d := newDestination(t, dst)

	// Stage the first three pieces by hand and record them, which is what an
	// interrupted attempt leaves behind.
	g, err := pieces.For(int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	partial, err := d.store.OpenPartial(t.Context(), blob)
	if err != nil {
		t.Fatal(err)
	}
	have := pieces.NewAvailability(g.Count())
	for _, i := range []int{0, 1, 2} {
		off, length, rerr := g.Range(i)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if _, werr := partial.WriteAt(content[off:off+length], off); werr != nil {
			t.Fatal(werr)
		}
		have.Add(i)
	}
	if err := partial.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.store.SavePieceProgress(blob, pieces.Encode(g, have)); err != nil {
		t.Fatal(err)
	}

	out, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]replication.Source{sourceFor(src, s.addr)})
	if err != nil {
		t.Fatalf("resuming: %v", err)
	}
	if out.Resumed != 3 {
		t.Errorf("resumed %d pieces, want 3", out.Resumed)
	}
	if out.Fetched != 5 {
		t.Errorf("fetched %d pieces, want 5 — the three already staged were refetched", out.Fetched)
	}
	for _, i := range holder.served {
		if i < 3 {
			t.Errorf("piece %d was refetched despite being recorded as held", i)
		}
	}
	if err := d.store.Verify(t.Context(), blob); err != nil {
		t.Errorf("the resumed blob does not verify: %v", err)
	}
}

// A bitset describing a different geometry is ignored rather than
// reinterpreted, because its bits mean different byte ranges.
func TestABitsetFromADifferentGeometryIsIgnored(t *testing.T) {
	content := pieceFixture(t, 4)
	blob := digestOfBytes(t, content)

	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	holder := newPieceHolder(t, content, 0, 1, 2, 3)
	s := startPieceSource(t, src, root, holder)
	d := newDestination(t, dst)

	// A record for a blob four times the size: same bits, different meaning.
	other, err := pieces.For(int64(len(content)) * 4)
	if err != nil {
		t.Fatal(err)
	}
	stale := pieces.NewAvailability(other.Count())
	stale.Add(0)
	stale.Add(1)
	if err := d.store.SavePieceProgress(blob, pieces.Encode(other, stale)); err != nil {
		t.Fatal(err)
	}

	out, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]replication.Source{sourceFor(src, s.addr)})
	if err != nil {
		t.Fatalf("pulling with a stale record: %v", err)
	}
	if out.Resumed != 0 {
		t.Errorf("resumed %d pieces from a record describing a different division", out.Resumed)
	}
	if out.Fetched != 4 {
		t.Errorf("fetched %d pieces, want all 4", out.Fetched)
	}
	if err := d.store.Verify(t.Context(), blob); err != nil {
		t.Errorf("the blob does not verify: %v", err)
	}
}

// The rarest piece is fetched first: the piece one peer holds is the piece that
// disappears if that peer goes away.
func TestTheRarestPieceIsFetchedFirst(t *testing.T) {
	content := pieceFixture(t, 4)
	blob := digestOfBytes(t, content)

	only := newNode(t, "peer-only", "only")
	common := newNode(t, "peer-common", "common")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(only.member(), common.member(), dst.member())

	// Piece 3 is held by ONE peer; 0, 1 and 2 by both. Piece 3 is also LAST in
	// index order, so a puller that simply walked the indices ascending would
	// fetch it last and fail this.
	onlyHolder := newPieceHolder(t, content, 0, 1, 2, 3)
	commonHolder := newPieceHolder(t, content, 0, 1, 2)

	// A SHARED order across both peers. Asserting on one holder's own record
	// cannot see this: the sole holder is the only peer that can serve piece 3,
	// so piece 3 is its first fetch whatever order the puller chose. Reversing
	// rarest to commonest left that assertion passing, which sabotage caught.
	var order []string
	onlyHolder.order, onlyHolder.id = &order, only.peerID
	commonHolder.order, commonHolder.id = &order, common.peerID

	o := startPieceSource(t, only, root, onlyHolder)
	c := startPieceSource(t, common, root, commonHolder)

	d := newDestination(t, dst)
	if _, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]replication.Source{sourceFor(only, o.addr), sourceFor(common, c.addr)}); err != nil {
		t.Fatalf("pulling: %v", err)
	}
	if len(order) != 4 {
		t.Fatalf("%d pieces were fetched, want 4: %v", len(order), order)
	}
	if want := fmt.Sprintf("%s:3", only.peerID); order[0] != want {
		t.Errorf("the first fetch was %q, want %q — the piece only one peer holds is the piece "+
			"that disappears if that peer goes away, so it goes first. Full order: %v",
			order[0], want, order)
	}
}

// A blob the store already holds is success, not a conflict, and nothing is
// fetched.
func TestPullingABlobAlreadyHeldFetchesNothing(t *testing.T) {
	content := pieceFixture(t, 4)

	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	holder := newPieceHolder(t, content, 0, 1, 2, 3)
	s := startPieceSource(t, src, root, holder)
	d := newDestination(t, dst)

	desc, err := d.store.Put(t.Context(), bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}

	out, err := d.puller.PullPieces(t.Context(), desc.Hash, desc.Size,
		[]replication.Source{sourceFor(src, s.addr)})
	if err != nil {
		t.Fatalf("pulling a blob already held: %v", err)
	}
	if !out.Deduplicated {
		t.Error("a blob already held was not reported as deduplicated")
	}
	if len(holder.served) != 0 {
		t.Errorf("%d pieces were fetched for a blob already held", len(holder.served))
	}
}

// With no source holding anything this node needs, the answer is "not yet",
// not "failed" — in a swarm where peers are both still fetching, that is
// ordinary and temporary.
func TestNoSourceHoldingWhatIsNeededIsNotYetRatherThanFailed(t *testing.T) {
	content := pieceFixture(t, 4)
	blob := digestOfBytes(t, content)

	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	holder := newPieceHolder(t, content) // holds nothing yet
	s := startPieceSource(t, src, root, holder)

	d := newDestination(t, dst)
	_, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]replication.Source{sourceFor(src, s.addr)})
	if !errors.Is(err, transfer.ErrNoPieceSource) {
		t.Errorf("err = %v, want ErrNoPieceSource", err)
	}
}

// 🔴 A bitset that claims pieces the staging file does not actually have fails
// CLOSED, and takes the record down with it.
//
// This is ADR-0043's "the bitset is a hint" at its sharpest. The record and the
// file can disagree — a crash between the two, a truncated file, a record
// restored from somewhere it should not have been — and the record is the half
// that is cheap to fake. Believing it means assembling a blob out of pieces
// that were never fetched.
//
// Nothing detects this directly, and that is the point: the whole-object check
// catches it the same way it catches a lying peer, because the digest is the
// only statement about these bytes that the local record did not make. What
// this asserts is that the failure is CLEAN — the bad staging file and the
// record that produced it both go, so the next attempt does not resume into the
// same wrong blob forever.
func TestABitsetClaimingPiecesTheFileLacksFailsClosedAndClearsItself(t *testing.T) {
	content := pieceFixture(t, 4)
	blob := digestOfBytes(t, content)

	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	// The source holds nothing, so nothing can rescue a bad resume: if the
	// record were believed, the pull would go straight to Publish.
	holder := newPieceHolder(t, content)
	s := startPieceSource(t, src, root, holder)
	d := newDestination(t, dst)

	g := mustGeometry(t, int64(len(content)))
	// A record claiming the whole blob, over a staging file that has only
	// piece 0 in it.
	partial, err := d.store.OpenPartial(t.Context(), blob)
	if err != nil {
		t.Fatal(err)
	}
	_, length, err := g.Range(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := partial.WriteAt(content[:length], 0); err != nil {
		t.Fatal(err)
	}
	if err := partial.Close(); err != nil {
		t.Fatal(err)
	}
	all := pieces.NewAvailability(g.Count())
	for i := range g.Count() {
		all.Add(i)
	}
	if err := d.store.SavePieceProgress(blob, pieces.Encode(g, all)); err != nil {
		t.Fatal(err)
	}

	_, err = d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]replication.Source{sourceFor(src, s.addr)})
	if err == nil {
		t.Fatal("a blob was published from a record that claimed pieces the file did not have")
	}
	if held, _ := d.store.Has(t.Context(), blob); held {
		t.Error("the store holds a blob assembled from pieces that were never fetched")
	}
	// The record must NOT survive. If it did, every future attempt would resume
	// into the same wrong blob and fail identically, forever.
	if encoded, _ := d.store.LoadPieceProgress(blob); encoded != "" {
		t.Error("the bitset survived, so the next attempt resumes into the same wrong blob")
	}
}
