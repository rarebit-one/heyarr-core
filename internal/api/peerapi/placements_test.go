// Every response in this file is drained and closed by the helper that made
// the request, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by peerSend
package peerapi_test

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/peer/placementsync"
)

// ADR-0114's route. The property worth asserting is the same as the inventory
// route's: the request names no peer, so the ONLY identity the source can act
// on is the certificate's, and the test checks the id the source was CALLED
// with, not just that a 200 came back.

type recordingOffers struct {
	mu      sync.Mutex
	askedBy []string
	offer   []placementsync.Entry
}

func (r *recordingOffers) OfferPlacements(
	_ context.Context, asker string, ask placementsync.Ask,
) (placementsync.Offer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.askedBy = append(r.askedBy, asker)
	var in []placementsync.Entry
	for _, e := range r.offer {
		if ask.Contains(e.BlobHash) {
			in = append(in, e)
		}
	}
	return placementsync.Offer{Range: ask.Range, Digest: placementsync.Digest(in), Entries: in}, nil
}

func serveWithPlacements(t *testing.T, self *peerNode, members mtls.Membership, src peerapi.PlacementSource) *listener {
	t.Helper()
	logs := &syncBuffer{}
	srv, err := peerapi.New(peerapi.Options{
		Addr: "127.0.0.1:0", Material: self.material, Members: members, SelfPeerID: self.peerID,
		Placements: src,
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
		_ = srv.Shutdown(ctx)
	})
	return &listener{srv: srv, self: self, addr: srv.Addr(), logs: logs}
}

// TestASiblingAsksAndIsAnsweredAsTheCertificatesPeer drives the real client
// against the real route: the asker learns the offer, and the source acted for
// the peer the certificate proved.
func TestASiblingAsksAndIsAnsweredAsTheCertificatesPeer(t *testing.T) {
	t.Parallel()
	source := newPeerNode(t, "site-a-id", "site-a")
	asker := newPeerNode(t, "site-b-id", "site-b")
	root := newTrustRoot(source.member(), asker.member())
	offered := "blake3:" + "a1" + "00000000000000000000000000000000000000000000000000000000000000"
	src := &recordingOffers{offer: []placementsync.Entry{{BlobHash: offered, Size: 42}}}
	l := serveWithPlacements(t, source, root, src)

	client := placementsync.NewClient(asker.material, nil)
	target := placementsync.Target{Peer: source.member(), Endpoint: "https://" + l.addr}
	r := placementsync.Range{Lo: "a", Hi: "b"}
	offer, err := client.Ask(context.Background(), target, placementsync.Ask{
		Range: r, Digest: placementsync.Digest(nil), ObservedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if err := offer.Validate(r); err != nil {
		t.Fatalf("the answer is not valid: %v", err)
	}
	if len(offer.Entries) != 1 || offer.Entries[0].BlobHash != offered || offer.Entries[0].Size != 42 {
		t.Fatalf("offer = %+v, want the one offered blob", offer)
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if len(src.askedBy) != 1 || src.askedBy[0] != asker.peerID {
		t.Fatalf("the source acted for %v, want [%s] (the certificate's peer)", src.askedBy, asker.peerID)
	}
}

func TestAPlacementQuestionIsRefusedWhenMalformedOrUnserved(t *testing.T) {
	t.Parallel()
	source := newPeerNode(t, "site-a-id", "site-a")
	asker := newPeerNode(t, "site-b-id", "site-b")
	root := newTrustRoot(source.member(), asker.member())
	url := func(l *listener) string { return "https://" + l.addr + placementsync.Path }

	served := serveWithPlacements(t, source, root, &recordingOffers{})
	c := dialler(t, asker, root)
	for _, body := range []string{
		`{"lo":"a","hi":"b","digest":"","observed_at":"2026-10-18T00:00:00Z"}`,
		`{"lo":"b","hi":"a","digest":"sha256:00","observed_at":"2026-10-18T00:00:00Z"}`,
		`{"lo":"a","hi":"b","digest":"sha256:00","observed_at":"2026-10-18T00:00:00Z","peer_id":"site-c-id"}`,
	} {
		status, out, _, err := peerSend(t, c, http.MethodPost, url(served), body)
		if err != nil {
			t.Fatal(err)
		}
		if status != http.StatusBadRequest {
			t.Errorf("body %s answered %d, want 400\n%s", body, status, out)
		}
	}

	unserved := serve(t, source, root) // no placement source: the media profile
	status, out, _, err := peerSend(t, c, http.MethodPost, url(unserved),
		`{"lo":"a","hi":"b","digest":"sha256:00","observed_at":"2026-10-18T00:00:00Z"}`)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("a node with no placement source answered %d, want 503\n%s", status, out)
	}
}
