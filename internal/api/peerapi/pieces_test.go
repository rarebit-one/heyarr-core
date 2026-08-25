// Every response in this file is drained and closed by the helper that made
// the request, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by peerSend
package peerapi_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

// The piece availability route, from the SOURCE's side (§23, ADR-0042).
//
// The subject here is the same shape as the manifest route's: what this route
// must not do. It must not compute anything to answer, it must tell "holds
// none of it yet" apart from "never heard of it", and it must not be reachable
// by a peer whose membership record is gone.

// sourcePieces is a node's answer about what it holds, and its accounting.
//
// The counter is the point. "The route answered" is satisfied by a route that
// read the whole blob first, and only a counter tells those apart — the same
// reasoning the manifest fixture uses, and it exists because a GET that read
// 20 GB to answer would be a remote denial of service.
type sourcePieces struct {
	mu sync.Mutex
	// available is what this node reports per blob.
	available map[string]string
	// known is every blob this node has anything to say about.
	known map[string]bool
	// reads counts calls, so a test can assert the route asked exactly once.
	reads int
	// computed counts calls to a method that WOULD be expensive. The honest
	// build never reaches it; the fixture offers it so the absence assertion
	// below is capable of failing rather than being a sentence.
	computed int
}

func newSourcePieces() *sourcePieces {
	return &sourcePieces{available: map[string]string{}, known: map[string]bool{}}
}

func (s *sourcePieces) PieceAvailability(
	_ context.Context, blob hashing.Hash,
) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if !s.known[blob.String()] {
		return "", false, nil
	}
	got, cached := s.available[blob.String()]
	if !cached {
		// The expensive path: this node knows the blob and has no ready
		// answer, so it would have to read the content to produce one.
		//
		// Reachable on purpose. An absence assertion whose subject cannot
		// happen is a sentence rather than an assertion, so the fixture keeps
		// a way to be expensive and counts it — and every test below arranges
		// a cached answer, so the honest route never gets here.
		s.computeExpensivelyLocked()
		return "", true, nil
	}
	return got, true, nil
}

// computeExpensivelyLocked stands in for reading the whole blob to answer,
// which is what this route must never cause. The caller holds s.mu.
func (s *sourcePieces) computeExpensivelyLocked() { s.computed++ }

func (s *sourcePieces) counts() (reads, computed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads, s.computed
}

// serveWithPieces is serve() with a piece source behind the route.
func serveWithPieces(
	t *testing.T, self *peerNode, members mtls.Membership, src peerapi.PieceSource,
) *listener {
	t.Helper()
	logs := &syncBuffer{}
	srv, err := peerapi.New(peerapi.Options{
		Addr:       "127.0.0.1:0",
		Material:   self.material,
		Members:    members,
		SelfPeerID: self.peerID,
		Pieces:     src,
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
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
			t.Errorf("shutting the peer surface down: %v", err)
		}
	})
	return &listener{srv: srv, self: self, addr: srv.Addr(), logs: logs}
}

func (l *listener) piecesURL(hash string) string {
	return "https://" + l.addr + peerapi.PieceAvailabilityPath(hash)
}

// THE assertion this route exists for: a peer can say it holds SOME of a blob.
//
// Every other route answers about a blob held whole. Without this one, a peer
// holding part of a blob is indistinguishable from one holding none, and two
// peers both still fetching have nothing to say to each other.
func TestAPeerCanReportHoldingPartOfABlob(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	dest := newPeerNode(t, "peer-destination", "destination")
	root := newTrustRoot(source.member(), dest.member())

	src := newSourcePieces()
	blob := hashing.MustParse("blake3:" + strings.Repeat("ab", 32))
	src.known[blob.String()] = true
	src.available[blob.String()] = "12:0f00"

	l := serveWithPieces(t, source, root, src)
	client := dialler(t, dest, mtls.PinnedKey(source.member()))
	status, body, _, err := peerSend(t, client, http.MethodGet, l.piecesURL(blob.String()), "")
	if err != nil {
		t.Fatalf("asking a peer what it holds: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}

	var got peerapi.PieceAvailabilityResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.BlobHash != blob.String() {
		t.Errorf("blob_hash = %q, want %q", got.BlobHash, blob.String())
	}
	if got.Available != "12:0f00" {
		t.Errorf("available = %q, want the bitset the node reported", got.Available)
	}
}

// 🔴 Answering COMPUTES NOTHING.
//
// The geometry is derived from the size, so answering never reads content. A
// GET that read a 20 GB blob to answer would be a remote denial of service —
// the same objection ADR-0034 makes to manifest-on-demand.
//
// The fixture deliberately CAN compute, and counts it. An absence assertion
// whose subject cannot exist is a sentence, not an assertion.
func TestAnsweringReadsNoContent(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	dest := newPeerNode(t, "peer-destination", "destination")
	root := newTrustRoot(source.member(), dest.member())

	src := newSourcePieces()
	blob := hashing.MustParse("blake3:" + strings.Repeat("cd", 32))
	src.known[blob.String()] = true
	src.available[blob.String()] = "8:ff"

	l := serveWithPieces(t, source, root, src)
	client := dialler(t, dest, mtls.PinnedKey(source.member()))
	status, body, _, err := peerSend(t, client, http.MethodGet, l.piecesURL(blob.String()), "")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}

	reads, computed := src.counts()
	if computed != 0 {
		t.Errorf("answering read content %d times — this route is a remote denial of "+
			"service if it can be made to", computed)
	}
	if reads != 1 {
		t.Errorf("the route asked the source %d times, want exactly 1", reads)
	}
}

// "Fetching, nothing yet" and "never heard of it" are DIFFERENT ANSWERS.
//
// A session choosing sources acts differently on each: the first is a peer
// worth asking again shortly, the second is one to stop asking. Collapsing
// them is the same mistake as reporting an unreachable indexer as one that
// found nothing (#239).
func TestHoldingNothingYetIsNotTheSameAsNeverHavingHeardOfIt(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	dest := newPeerNode(t, "peer-destination", "destination")
	root := newTrustRoot(source.member(), dest.member())

	src := newSourcePieces()
	fetching := hashing.MustParse("blake3:" + strings.Repeat("11", 32))
	src.known[fetching.String()] = true
	src.available[fetching.String()] = "" // known, nothing landed
	unknown := hashing.MustParse("blake3:" + strings.Repeat("22", 32))

	l := serveWithPieces(t, source, root, src)
	client := dialler(t, dest, mtls.PinnedKey(source.member()))

	status, body, _, err := peerSend(t, client, http.MethodGet, l.piecesURL(fetching.String()), "")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Errorf("a blob being fetched with nothing landed answered %d, want 200: %s", status, body)
	}

	status, _, _, err = peerSend(t, client, http.MethodGet, l.piecesURL(unknown.String()), "")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusNotFound {
		t.Errorf("a blob this node never heard of answered %d, want 404", status)
	}
}

// A node with no store behind its surface answers 503, not 404.
//
// The distinction a destination acts on: 404 means "that is a hash and this
// peer has none of it", so try another source; 503 means this peer is not
// serving bytes at all, so there is nothing here to try again for.
func TestANodeWithNoStoreAnswersUnavailableRatherThanNotFound(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	dest := newPeerNode(t, "peer-destination", "destination")
	root := newTrustRoot(source.member(), dest.member())

	l := serveWithPieces(t, source, root, nil)
	client := dialler(t, dest, mtls.PinnedKey(source.member()))
	blob := hashing.MustParse("blake3:" + strings.Repeat("33", 32))

	status, _, _, err := peerSend(t, client, http.MethodGet, l.piecesURL(blob.String()), "")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 — a node with no store is not a node "+
			"that is missing this blob", status)
	}
}

// A path that is not a digest is refused before anything is looked up.
func TestANonDigestPathIsRefused(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	dest := newPeerNode(t, "peer-destination", "destination")
	root := newTrustRoot(source.member(), dest.member())

	src := newSourcePieces()
	l := serveWithPieces(t, source, root, src)
	client := dialler(t, dest, mtls.PinnedKey(source.member()))

	status, _, _, err := peerSend(t, client, http.MethodGet,
		"https://"+l.addr+peerapi.PieceAvailabilityPath("not-a-digest"), "")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if reads, _ := src.counts(); reads != 0 {
		t.Errorf("the source was asked %d times about a path that is not a digest", reads)
	}
}
