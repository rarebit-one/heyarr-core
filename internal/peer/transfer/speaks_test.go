package transfer_test

import (
	"testing"

	domaintransfer "github.com/rarebit-one/heyarr-core/internal/domain/transfer"
	"github.com/rarebit-one/heyarr-core/internal/peer/transfer"
)

// What a source SPEAKS, asked rather than assumed (§27, ADR-0042, #266).
//
// Before this, every candidate was built as a piece peer and transfer.WebSeed
// was constructed nowhere outside a test — §27's web seed was unreachable from
// any running binary. These tests are the ones that fail if the classification
// is removed and the transport goes back to assuming.

// 🔴 A node serving the piece routes is a piece peer.
func TestASourceServingPieceRoutesIsAPiecePeer(t *testing.T) {
	content := pieceFixture(t, 4)
	src := newNode(t, "peer-pieces", "pieces")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	holder := newPieceHolder(t, content, 0, 1, 2, 3)
	s := startPieceSource(t, src, root, holder)
	d := newDestination(t, dst)

	got := d.puller.KindOf(t.Context(), sourceFor(src, s.addr))
	if got.Kind != domaintransfer.KindPeer {
		t.Errorf("KindOf = %q, want %q — this node answers the availability and piece routes",
			got.Kind, domaintransfer.KindPeer)
	}
}

// 🔴 A node serving CONTENT and no piece routes is a web seed.
//
// This is §27 exactly: a member reachable over HTTP that serves byte ranges of
// blobs it holds whole and takes no part in swarms.
func TestASourceServingOnlyContentIsAWebSeed(t *testing.T) {
	content := pieceFixture(t, 4)
	seed := newNode(t, "peer-seed", "seed")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(seed.member(), dst.member())

	s := startSource(t, seed, root, content)
	d := newDestination(t, dst)

	got := d.puller.KindOf(t.Context(), sourceFor(seed, s.addr))
	if got.Kind != domaintransfer.KindWebSeed {
		t.Errorf("KindOf = %q, want %q — this node serves ranges and no pieces",
			got.Kind, domaintransfer.KindWebSeed)
	}
}

// 🔴 A blob completes using ONLY a web seed that was DISCOVERED rather than
// declared.
//
// The sharpest assertion here, and the one #266 asked for. Every other web-seed
// test hands PullPieces a `transfer.WebSeed(...)` the test built itself, which
// proves the transport can drive one and says nothing about anything being able
// to reach that code. This one starts from a bare source, asks it what it
// speaks, and uses the answer — which is the path a running binary takes.
func TestABlobCompletesFromAWebSeedDiscoveredByAsking(t *testing.T) {
	content := pieceFixture(t, 8)
	blob := digestOfBytes(t, content)

	seed := newNode(t, "peer-seed", "seed")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(seed.member(), dst.member())

	s := startSource(t, seed, root, content)
	d := newDestination(t, dst)

	candidate := d.puller.KindOf(t.Context(), sourceFor(seed, s.addr))
	if candidate.Kind != domaintransfer.KindWebSeed {
		t.Fatalf("the source was classified %q, so this test would not be about a web seed",
			candidate.Kind)
	}

	out, err := d.puller.PullPieces(t.Context(), blob, int64(len(content)),
		[]transfer.Candidate{candidate})
	if err != nil {
		t.Fatalf("completing a blob from a discovered web seed: %v", err)
	}
	if out.Fetched != 8 {
		t.Errorf("fetched %d pieces, want 8", out.Fetched)
	}
	if out.FromPeer[seed.peerID] != 8 {
		t.Errorf("the web seed is credited with %d pieces, want 8: %v",
			out.FromPeer[seed.peerID], out.FromPeer)
	}
	assertPublished(t, d, blob, content)
}

// A source that cannot be asked keeps the piece contract.
//
// Downgrading on a network error is the precise mistake ADR-0042 names: a peer
// whose route is BROKEN would become indistinguishable from one that never
// served pieces, and the transport would quietly take the slower contract
// instead of reporting a fault.
func TestASourceThatCannotBeAskedKeepsThePieceContract(t *testing.T) {
	gone := newNode(t, "peer-gone", "gone")
	dst := newNode(t, "peer-destination", "destination")
	_ = newTrustRoot(gone.member(), dst.member())

	d := newDestination(t, dst)
	got := d.puller.KindOf(t.Context(), sourceFor(gone, unreachableAddr(t)))
	if got.Kind != domaintransfer.KindPeer {
		t.Errorf("KindOf = %q for an unreachable source, want %q — a failure to ask is not "+
			"evidence that a peer does not speak piece exchange", got.Kind, domaintransfer.KindPeer)
	}
}
