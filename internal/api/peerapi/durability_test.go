package peerapi_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/durability"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// The three answers ADR-0018's placement precondition acts on, against a real
// peer surface over real mutually authenticated TLS (M4-12).
//
// This is the mechanism behind the collector's three refusals. The collector's
// own tests drive a fake, which is right for asserting the decision; this
// asserts that the fake's three cases are the three cases a real peer produces,
// because a verifier that collapsed any two of them would remove a refusal
// while every unit test kept passing.

// verifierFor builds the durability verifier as one node asking another.
func verifierFor(t *testing.T, self *peerNode) *durability.Verifier {
	t.Helper()
	v, err := durability.New(durability.Options{
		Material: self.material,
		// The controller half is not what this file is about; it is asserted
		// against a real database in internal/worker.
		Controller: durability.ControllerFunc(func(context.Context) error { return nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// peerAt describes a peer to the verifier the way the catalog would.
func peerAt(n *peerNode, addr string) integrity.Peer {
	return integrity.Peer{
		PeerID: n.peerID, Name: n.name, Endpoint: "https://" + addr,
		PublicKey: n.pub, Health: "reachable",
	}
}

func TestDurabilityVerifierConfirmsAPeerThatHoldsTheBytes(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	asker := newPeerNode(t, "peer-asker", "asker")
	root := newTrustRoot(source.member(), asker.member())
	store, hash := sourceStore(t, bytes.Repeat([]byte("durable bytes"), 100))
	l := serveWithBlobs(t, source, root, store)

	if err := verifierFor(t, asker).Holds(t.Context(), peerAt(source, l.addr), hash); err != nil {
		t.Fatalf("a peer that holds the bytes was not confirmed: %v", err)
	}
}

// The lying row's other end. The peer answers, and answers that it does not
// hold them — which is a different action from silence, and the only one that
// justifies correcting a `replicas` row.
func TestDurabilityVerifierReportsAPeerThatDoesNotHoldTheBytes(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	asker := newPeerNode(t, "peer-asker", "asker")
	root := newTrustRoot(source.member(), asker.member())
	// A store that holds something else entirely, so the peer is serving
	// normally and simply does not have this blob.
	store, _ := sourceStore(t, []byte("different content"))
	l := serveWithBlobs(t, source, root, store)

	absent := hashing.MustParse(
		"blake3:0000000000000000000000000000000000000000000000000000000000000000")
	err := verifierFor(t, asker).Holds(t.Context(), peerAt(source, l.addr), absent)
	if !errors.Is(err, integrity.ErrPeerLacksBlob) {
		t.Fatalf("err = %v, want %v — a 404 is the peer contradicting the row, and it must not "+
			"read as unreachability", err, integrity.ErrPeerLacksBlob)
	}
}

// Silence. Nothing answered, so nothing was established — and it must not be
// mistaken for the peer denying it, which would corrupt a correct row to
// `missing`.
func TestDurabilityVerifierReportsAPeerThatAnswersNothing(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	asker := newPeerNode(t, "peer-asker", "asker")
	root := newTrustRoot(source.member(), asker.member())
	store, hash := sourceStore(t, []byte("bytes on a peer about to go away"))
	l := serveWithBlobs(t, source, root, store)
	// Take the peer down, keeping its address, which is what an operator sees
	// as "site B is down" rather than "site B was removed".
	if err := l.srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	err := verifierFor(t, asker).Holds(t.Context(), peerAt(source, l.addr), hash)
	if !errors.Is(err, integrity.ErrPeerUnreachable) {
		t.Fatalf("err = %v, want %v", err, integrity.ErrPeerUnreachable)
	}
	if errors.Is(err, integrity.ErrPeerLacksBlob) {
		t.Fatal("silence read as a denial, which would corrupt a correct replicas row to missing")
	}
}

// A peer with no pinned key is refused before a socket exists.
//
// Membership is the only trust root in the inter-peer path (ADR-0012), and
// believing whatever answered an unpinned dial would be trust on first use — in
// the service of deciding it is safe to delete the last local copy.
func TestDurabilityVerifierRefusesAPeerWithNoPinnedKey(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	asker := newPeerNode(t, "peer-asker", "asker")
	root := newTrustRoot(source.member(), asker.member())
	store, hash := sourceStore(t, []byte("bytes behind an unpinned peer"))
	l := serveWithBlobs(t, source, root, store)

	p := peerAt(source, l.addr)
	p.PublicKey = nil
	err := verifierFor(t, asker).Holds(t.Context(), p, hash)
	if err == nil {
		t.Fatal("an unpinned peer was accepted as evidence that a blob is safe to delete")
	}
	if errors.Is(err, integrity.ErrPeerLacksBlob) {
		t.Error("a wiring refusal read as the peer denying the blob")
	}
}

// A peer that is up and serves no bytes at all establishes nothing either way:
// it has not stayed silent and it has not denied the blob.
func TestDurabilityVerifierRefusesAPeerServingNoBytes(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	asker := newPeerNode(t, "peer-asker", "asker")
	root := newTrustRoot(source.member(), asker.member())
	var noStore cas.Store
	l := serveWithBlobs(t, source, root, noStore)
	_, hash := sourceStore(t, []byte("bytes this peer does not serve"))

	err := verifierFor(t, asker).Holds(t.Context(), peerAt(source, l.addr), hash)
	if err == nil {
		t.Fatal("a peer serving no content confirmed a blob")
	}
	if errors.Is(err, integrity.ErrPeerLacksBlob) {
		t.Error("503 read as a denial; the row would be corrected on the strength of a peer that " +
			"was never asked about it")
	}
}
