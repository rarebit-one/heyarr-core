// Every response in this file is drained and closed by the helper that made
// the request, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by peerSend
package peerapi_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// The byte-carrying hop, from the SOURCE's side (§21, §28, ADR-0013, M4-09).
//
// internal/worker's transfer fabric asserts what a destination does with what
// arrives. This file asserts the other end: that the route exists on the peer
// listener, serves the same contract the client API serves, refuses a peer that
// is not a member, and distinguishes "this peer does not hold that blob" from
// "this peer serves no bytes at all" — a distinction a puller acts on, since
// the first means try another source and the second means there is nothing here
// to try again for.

// serveWithBlobs is serve() with a content store behind the blob route.
func serveWithBlobs(t *testing.T, self *peerNode, members mtls.Membership, store cas.Store) *listener {
	t.Helper()
	logs := &syncBuffer{}
	var handler peerapi.BlobServer
	if store != nil {
		built, err := blobs.New(blobs.Options{Store: store})
		if err != nil {
			t.Fatal(err)
		}
		handler = built
	}
	srv, err := peerapi.New(peerapi.Options{
		Addr:       "127.0.0.1:0",
		Material:   self.material,
		Members:    members,
		SelfPeerID: self.peerID,
		Blobs:      handler,
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

func (l *listener) blobURL(hash string) string {
	return "https://" + l.addr + peerapi.BlobContentPath(hash)
}

// sourceStore is a CAS holding one blob, and the digest that names it.
func sourceStore(t *testing.T, content []byte) (*cas.FS, hashing.Hash) {
	t.Helper()
	store, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	desc, err := store.Put(t.Context(), bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return store, desc.Hash
}

// The happy path, and it asserts the BYTES rather than the status. A surface
// that answered 200 with an empty body would pass a status-only test and be
// useless to every consumer of it.
func TestPeerSurfaceServesBlobContentToAMember(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	puller := newPeerNode(t, "peer-destination", "destination")
	root := newTrustRoot(source.member(), puller.member())

	content := bytes.Repeat([]byte("bytes that cross a network"), 400)
	store, hash := sourceStore(t, content)
	l := serveWithBlobs(t, source, root, store)

	client := dialler(t, puller, mtls.PinnedKey(source.member()))
	status, body, _, err := peerSend(t, client, http.MethodGet, l.blobURL(hash.String()), "")
	if err != nil {
		t.Fatalf("reading a blob from a peer: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if body != string(content) {
		t.Fatal("the peer surface served bytes that are not the blob")
	}
	// Hashing what came back is the only check that cannot be satisfied by a
	// server returning something plausible.
	got, _, err := hashing.HashReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if got != hash {
		t.Fatalf("the served bytes hash to %s, want %s", got, hash)
	}
}

// §28 makes ranges part of the contract every peer advertises, and ADR-0013
// makes this the same handler the client API serves. If the mount had lost
// them — by wrapping the handler, or by copying a simpler one — this is what
// would notice.
func TestPeerSurfaceServesRangesAndHead(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	puller := newPeerNode(t, "peer-destination", "destination")
	root := newTrustRoot(source.member(), puller.member())

	content := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	store, hash := sourceStore(t, content)
	l := serveWithBlobs(t, source, root, store)
	client := dialler(t, puller, mtls.PinnedKey(source.member()))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, l.blobURL(hash.String()), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=4-9")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("a ranged read answered %d, want 206", resp.StatusCode)
	}
	buf := make([]byte, 16)
	n, _ := resp.Body.Read(buf)
	if string(buf[:n]) != "456789" {
		t.Fatalf("the range served %q, want %q", string(buf[:n]), "456789")
	}
	if resp.Header.Get("ETag") != `"blake3-`+hash.Hex()+`"` {
		t.Fatalf("ETag = %q, want the strong content validator", resp.Header.Get("ETag"))
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatal("the peer surface does not advertise range support (§28)")
	}

	status, body, _, err := peerSend(t, client, http.MethodHead, l.blobURL(hash.String()), "")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("HEAD answered %d, want 200", status)
	}
	if body != "" {
		t.Fatal("HEAD returned a body")
	}
}

// 404 and 503 are different answers to different questions, and a puller acts
// on the difference.
func TestPeerSurfaceDistinguishesAnAbsentBlobFromNoStoreAtAll(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	puller := newPeerNode(t, "peer-destination", "destination")
	root := newTrustRoot(source.member(), puller.member())
	absent := "blake3:" + strings.Repeat("ab", 32)

	withStore, _ := sourceStore(t, []byte("something else"))
	held := serveWithBlobs(t, source, root, withStore)
	client := dialler(t, puller, mtls.PinnedKey(source.member()))
	status, _, _, err := peerSend(t, client, http.MethodGet, held.blobURL(absent), "")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusNotFound {
		t.Fatalf("a blob this peer does not hold answered %d, want 404 — a puller reads that as "+
			"'try another source'", status)
	}

	// A malformed digest is a third thing again: not a hash at all, and no
	// amount of retrying will make it one.
	status, _, _, err = peerSend(t, client, http.MethodGet, held.blobURL("not-a-digest"), "")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("a malformed digest answered %d, want 400", status)
	}

	// And a node with no store answers 503: it is not serving bytes at all.
	bare := serveWithBlobs(t, source, root, nil)
	bareClient := dialler(t, puller, mtls.PinnedKey(source.member()))
	status, _, _, err = peerSend(t, bareClient, http.MethodGet, bare.blobURL(absent), "")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("a node with no content store answered %d, want 503 — collapsing it into 404 would "+
			"have a destination hunting for a blob on a node that serves none", status)
	}
}

// Membership is the authorisation for a peer-to-peer blob read (ADR-0030), and
// revocation is the deletion of the record (ADR-0012) — on the connection the
// peer is already holding open.
func TestPeerSurfaceRefusesBlobContentToARevokedPeer(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	puller := newPeerNode(t, "peer-destination", "destination")
	root := newTrustRoot(source.member(), puller.member())

	content := []byte("bytes a member may read")
	store, hash := sourceStore(t, content)
	l := serveWithBlobs(t, source, root, store)
	client := dialler(t, puller, mtls.PinnedKey(source.member()))

	// It works first. Without this the refusal below would pass against a
	// surface that serves nobody.
	status, body, _, err := peerSend(t, client, http.MethodGet, l.blobURL(hash.String()), "")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || body != string(content) {
		t.Fatalf("the member's read answered %d with %d bytes", status, len(body))
	}

	root.remove(puller.pub)

	status, _, reused, err := peerSend(t, client, http.MethodGet, l.blobURL(hash.String()), "")
	if err != nil {
		// The connection was severed rather than answered, which is also a
		// refusal — membership is consulted per connection as well as per
		// request.
		return
	}
	if status != http.StatusForbidden {
		t.Fatalf("a revoked peer read a blob and got %d, want 403 (reused connection: %t)", status, reused)
	}
}
