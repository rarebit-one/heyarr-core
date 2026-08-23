package transfer_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/peer/transfer"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// M5-05's acceptance, from the DESTINATION's side (§20, ADR-0030, ADR-0034).
//
// internal/api/peerapi asserts what the source answers. This file asserts what
// the destination DOES about it — which is the half that matters, because the
// three answers the source keeps distinct are only worth keeping distinct if
// something acts differently on each.
//
// Nothing here tells a source anything about this node's inventory. The whole
// request is a blob digest read out of this node's own catalog, and every
// decision below is taken here, after the answer arrived.

// ---------------------------------------------------------------------------
// fixtures

type node struct {
	peerID   string
	name     string
	pub      ed25519.PublicKey
	material *mtls.Material
}

func newNode(t *testing.T, peerID, name string) *node {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	material, err := mtls.NewMaterial(mtls.MaterialOptions{
		PrivateKey: priv, PeerID: peerID, Lifetime: time.Hour, RenewBefore: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &node{peerID: peerID, name: name, pub: pub, material: material}
}

func (n *node) member() mtls.Peer {
	return mtls.Peer{PeerID: n.peerID, Name: n.name, PublicKey: n.pub}
}

// trustRoot is the membership table both ends consult.
type trustRoot struct {
	mu    sync.Mutex
	byKey map[string]mtls.Peer
}

func newTrustRoot(members ...mtls.Peer) *trustRoot {
	r := &trustRoot{byKey: map[string]mtls.Peer{}}
	for _, m := range members {
		r.byKey[string(m.PublicKey)] = m
	}
	return r
}

func (r *trustRoot) Lookup(_ context.Context, pub []byte) (mtls.Peer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byKey[string(pub)]
	if !ok {
		return mtls.Peer{}, mtls.ErrNotAMember
	}
	return p, nil
}

// sourceNode is one running source: its peer surface, its content store and
// the manifests it holds.
type sourceNode struct {
	self      *node
	addr      string
	store     *cas.FS
	manifests *fakeManifests
}

// source renders it as the replication candidate a destination would read out
// of its own catalog.
func (s *sourceNode) source() replication.Source {
	return replication.Source{
		PeerID: s.self.peerID, Name: s.self.name,
		Endpoint: "https://" + s.addr, PublicKey: s.self.pub,
	}
}

// fakeManifests is a source's manifest store.
//
// It can be told to serve a manifest that does not check out, which is the
// only way to exercise the destination's rejection path: the honest source
// validates on the way in, so a corrupt manifest on the wire is a source whose
// table was tampered with, a source running a buggy build, or something on the
// path. All three are the destination's problem and all three look like this.
type fakeManifests struct {
	mu     sync.Mutex
	stored map[string]manifests.Manifest
	held   map[string]bool
	// served, when set for a blob, is what goes on the wire INSTEAD of stored.
	served map[string]manifests.Manifest
}

func newFakeManifests() *fakeManifests {
	return &fakeManifests{
		stored: map[string]manifests.Manifest{},
		held:   map[string]bool{},
		served: map[string]manifests.Manifest{},
	}
}

func (f *fakeManifests) hold(blob hashing.Hash) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held[blob.String()] = true
}

func (f *fakeManifests) store(m manifests.Manifest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held[m.BlobHash.String()] = true
	f.stored[m.BlobHash.String()] = m
}

func (f *fakeManifests) serveInstead(blob hashing.Hash, m manifests.Manifest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held[blob.String()] = true
	f.served[blob.String()] = m
}

func (f *fakeManifests) ChunkManifest(
	_ context.Context, blob hashing.Hash,
) (manifests.Manifest, manifests.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := blob.String()
	if !f.held[key] {
		return manifests.Manifest{}, "", fmt.Errorf("%w: %s", peerapi.ErrNoSuchBlob, blob)
	}
	if m, ok := f.served[key]; ok {
		return m, manifests.StatePresent, nil
	}
	if m, ok := f.stored[key]; ok {
		return m, manifests.StatePresent, nil
	}
	return manifests.Manifest{}, manifests.StateUndecided, nil
}

// startSource runs a peer surface holding the given content, with a manifest
// route behind it.
func startSource(t *testing.T, self *node, members mtls.Membership, content []byte) *sourceNode {
	t.Helper()
	store, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if content != nil {
		if _, err := store.Put(t.Context(), bytes.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	blobHandler, err := blobs.New(blobs.Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeManifests()
	srv, err := peerapi.New(peerapi.Options{
		Addr:       "127.0.0.1:0",
		Material:   self.material,
		Members:    members,
		SelfPeerID: self.peerID,
		Blobs:      blobHandler,
		Manifests:  fake,
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
			t.Errorf("shutting a source down: %v", err)
		}
	})
	return &sourceNode{self: self, addr: srv.Addr(), store: store, manifests: fake}
}

// destination is this node: a puller and the store it lands bytes in.
type destination struct {
	puller *transfer.Puller
	store  *cas.FS
}

func newDestination(t *testing.T, self *node) *destination {
	t.Helper()
	store, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := transfer.New(transfer.Options{Material: self.material, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	return &destination{puller: p, store: store}
}

// manifestFor builds a manifest whose index order and digest order differ, so
// an ordering fault cannot pass by coincidence.
func manifestFor(t *testing.T, blob hashing.Hash, lengths ...int64) manifests.Manifest {
	t.Helper()
	chunks := make([]chunking.Chunk, 0, len(lengths))
	var off int64
	for i, n := range lengths {
		h := hashing.New()
		_, _ = h.Write(bytes.Repeat([]byte{byte('a' + i)}, int(n)))
		chunks = append(chunks, chunking.Chunk{Offset: off, Length: n, Digest: h.Sum()})
		off += n
	}
	ascending, descending := true, true
	for i := 1; i < len(chunks); i++ {
		if chunks[i].Digest.Hex() <= chunks[i-1].Digest.Hex() {
			ascending = false
		}
		if chunks[i].Digest.Hex() >= chunks[i-1].Digest.Hex() {
			descending = false
		}
	}
	if ascending || descending {
		t.Fatal("the fixture's digests are ordered, so index order and digest order coincide and " +
			"a sorted response would satisfy every ordering assertion below")
	}
	m, err := manifests.Build(blob, chunking.DefaultConfig(), chunks,
		time.Date(2026, 8, 24, 9, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// ---------------------------------------------------------------------------
// the happy path

// A destination fetches a manifest over mTLS and gets the same chunk sequence
// the source stored — in order, by digest.
func TestADestinationReadsTheSourcesChunkSequenceInOrder(t *testing.T) {
	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	content := bytes.Repeat([]byte("bytes described by a manifest"), 500)
	source := startSource(t, src, root, content)
	whole, _, err := hashing.HashReader(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	stored := manifestFor(t, whole, 4096, 65536, 12288, 300, 8192)
	source.manifests.store(stored)

	dest := newDestination(t, dst)
	got, err := dest.puller.FetchManifest(t.Context(), source.source(), whole)
	if err != nil {
		t.Fatalf("reading a manifest from a source: %v", err)
	}

	if !got.Digest.Equal(stored.Digest) {
		t.Errorf("manifest digest = %s, the source stored %s", got.Digest, stored.Digest)
	}
	if got.Digest.Equal(got.BlobHash) {
		t.Error("the manifest digest equals the blob digest — one is being used as the other, " +
			"and ADR-0034 says nothing may resolve a manifest digest to bytes")
	}
	if len(got.Chunks) != len(stored.Chunks) {
		t.Fatalf("%d chunks arrived, the source stored %d", len(got.Chunks), len(stored.Chunks))
	}
	// THE assertion: same digests, same positions.
	for i := range stored.Chunks {
		if !got.Chunks[i].Digest.Equal(stored.Chunks[i].Digest) {
			t.Errorf("chunk %d is %s, the source stored %s at that index",
				i, got.Chunks[i].Digest, stored.Chunks[i].Digest)
		}
		if got.Chunks[i].Offset != stored.Chunks[i].Offset ||
			got.Chunks[i].Length != stored.Chunks[i].Length {
			t.Errorf("chunk %d spans %d+%d, the source stored %d+%d", i,
				got.Chunks[i].Offset, got.Chunks[i].Length,
				stored.Chunks[i].Offset, stored.Chunks[i].Length)
		}
	}
}

// ---------------------------------------------------------------------------
// the two 404s lead to two different actions

// replicate is what a destination does with one ordered list of candidates: it
// asks each for a description and acts on the answer.
//
// It is written out here rather than asserted at the error level because the
// acceptance condition is about ACTIONS. "The bodies differ" is satisfied by
// two error strings nobody branches on; this branches, and the outcome says
// which branch ran.
func replicate(
	ctx context.Context, d *destination, expected hashing.Hash, candidates ...replication.Source,
) (outcome transfer.Outcome, chunked bool, err error) {
	var lastErr error
	for _, src := range candidates {
		m, mErr := d.puller.FetchManifest(ctx, src, expected)
		switch {
		case mErr == nil:
			// A description arrived. A chunk-level transfer is M5-06 and
			// beyond; what matters here is that the destination decided, on
			// its own, that this source can be used chunk-wise.
			out, pErr := d.puller.Pull(ctx, src, expected)
			return out, m.ChunkCount() > 0, pErr
		case errors.Is(mErr, transfer.ErrSourceHasNoManifest):
			// The answer, not a failure. This source holds the bytes and has
			// no description of them: pull whole, from THIS source. Exactly
			// M4's behaviour, and always correct.
			out, pErr := d.puller.Pull(ctx, src, expected)
			return out, false, pErr
		case errors.Is(mErr, transfer.ErrSourceLacksBlob):
			// A different action: this source cannot help at all, so move to
			// the next candidate rather than trying to read bytes it does not
			// have.
			lastErr = mErr
			continue
		default:
			return transfer.Outcome{}, false, mErr
		}
	}
	return transfer.Outcome{}, false, lastErr
}

// The destination takes a DIFFERENT ACTION on each 404, and the difference is
// observable in where the bytes came from.
//
// Both answers are 404 and neither is an error, so a destination that read
// only the status would take one action for both — and the wrong one half the
// time. On "no manifest" it must stay with this source; on "no such blob" it
// must leave it.
func TestTheTwo404sProduceDifferentActions(t *testing.T) {
	holder := newNode(t, "peer-holder", "holder")
	empty := newNode(t, "peer-empty", "empty")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(holder.member(), empty.member(), dst.member())

	content := bytes.Repeat([]byte("bytes only one source holds"), 300)
	whole, _, err := hashing.HashReader(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}

	// One source holds the bytes and has never chunked them: §16's undecided.
	held := startSource(t, holder, root, content)
	held.manifests.hold(whole)
	// The other holds nothing at all.
	none := startSource(t, empty, root, nil)

	// no-manifest: the destination stays with this source and pulls whole.
	dest := newDestination(t, dst)
	out, chunked, err := replicate(t.Context(), dest, whole, held.source())
	if err != nil {
		t.Fatalf("a source with no manifest should be pulled whole: %v", err)
	}
	if out.SourcePeerID != holder.peerID {
		t.Errorf("the bytes came from %q, want %q — a 404 for the manifest is not a reason to "+
			"abandon a source that holds the bytes", out.SourcePeerID, holder.peerID)
	}
	if chunked {
		t.Error("the destination believed it had a chunk sequence for a blob nobody has chunked")
	}
	if out.Bytes != int64(len(content)) {
		t.Errorf("%d bytes landed, want %d", out.Bytes, len(content))
	}

	// no-such-blob: the destination leaves this source and tries the next.
	// Same status code, same absence of a manifest, opposite action.
	dest2 := newDestination(t, dst)
	out, _, err = replicate(t.Context(), dest2, whole, none.source(), held.source())
	if err != nil {
		t.Fatalf("the destination did not fall through to the source that holds the bytes: %v", err)
	}
	if out.SourcePeerID != holder.peerID {
		t.Errorf("the bytes came from %q, want %q — a source that answered 'no such blob' was "+
			"used anyway", out.SourcePeerID, holder.peerID)
	}
	if has, err := dest2.store.Has(t.Context(), whole); err != nil || !has {
		t.Errorf("the destination does not hold the blob afterwards (%v)", err)
	}

	// And with ONLY the empty source, the destination gets nowhere and says
	// why. Without this the fall-through above could be an unconditional
	// "always try the next one".
	dest3 := newDestination(t, dst)
	if _, _, err := replicate(t.Context(), dest3, whole, none.source()); !errors.Is(
		err, transfer.ErrSourceLacksBlob) {
		t.Errorf("a lone source that holds nothing produced %v, want ErrSourceLacksBlob", err)
	}
}

// A node with no content behind its peer surface answers 503, matching the
// content route, and the destination treats it as neither of the 404s.
func TestANodeServingNoContentIsRefusedRatherThanRead(t *testing.T) {
	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	srv, err := peerapi.New(peerapi.Options{
		Addr: "127.0.0.1:0", Material: src.material, Members: root,
		SelfPeerID: src.peerID, Logger: slog.New(slog.DiscardHandler),
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

	dest := newDestination(t, dst)
	blob := hashing.MustParse("blake3:" + strings.Repeat("77", 32))
	_, err = dest.puller.FetchManifest(t.Context(), replication.Source{
		PeerID: src.peerID, Name: src.name,
		Endpoint: "https://" + srv.Addr(), PublicKey: src.pub,
	}, blob)
	if !errors.Is(err, transfer.ErrSourceRefused) {
		t.Fatalf("a node serving no content produced %v, want ErrSourceRefused", err)
	}
	// It is emphatically NOT "no manifest": a destination that read it that
	// way would go on to pull whole from a node that serves no bytes.
	if errors.Is(err, transfer.ErrSourceHasNoManifest) || errors.Is(err, transfer.ErrSourceLacksBlob) {
		t.Fatal("a 503 was read as one of the 404s")
	}
}

// ---------------------------------------------------------------------------
// a manifest that does not check out is rejected, not stored

// The rejection fires, and it fires on the destination.
//
// The source is not asked to prove anything about the manifest and could not:
// a source that tampered with its own manifest would also tamper with any
// proof it offered. The destination recomputes the digest over the sequence
// that actually arrived, which is the only check that does not depend on the
// source being honest.
func TestAManifestThatDoesNotMatchItsDigestIsRejected(t *testing.T) {
	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	content := bytes.Repeat([]byte("bytes whose description was tampered with"), 400)
	source := startSource(t, src, root, content)
	whole, _, err := hashing.HashReader(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	good := manifestFor(t, whole, 4096, 65536, 12288, 300, 8192)

	tests := []struct {
		name    string
		breakIt func(m *manifests.Manifest)
	}{
		{
			// A chunk digest changed and the manifest digest left alone: the
			// destination is being told these bytes sit at that offset when
			// they do not.
			name: "a swapped chunk digest",
			breakIt: func(m *manifests.Manifest) {
				m.Chunks[2].Digest = hashing.MustParse("blake3:" + strings.Repeat("de", 32))
			},
		},
		{
			// The sharpest case, and the one sabotage (3) produces: every
			// chunk is valid, every offset is contiguous, and only the ORDER
			// is wrong. Nothing about the pieces detects it.
			name: "the sequence reversed, every chunk intact",
			breakIt: func(m *manifests.Manifest) {
				reversed := make([]chunking.Chunk, len(m.Chunks))
				var off int64
				for i := range m.Chunks {
					c := m.Chunks[len(m.Chunks)-1-i]
					reversed[i] = chunking.Chunk{Offset: off, Length: c.Length, Digest: c.Digest}
					off += c.Length
				}
				m.Chunks = reversed
			},
		},
		{
			// The manifest is internally perfect and describes another blob.
			// Every per-chunk digest checks out; it is simply not the file
			// this node asked about.
			name: "a valid manifest for a different blob",
			breakIt: func(m *manifests.Manifest) {
				other := manifestFor(t, hashing.MustParse("blake3:"+strings.Repeat("99", 32)),
					700, 300, 1500)
				*m = other
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broken := good
			broken.Chunks = append([]chunking.Chunk(nil), good.Chunks...)
			tt.breakIt(&broken)
			source.manifests.serveInstead(whole, broken)

			dest := newDestination(t, dst)
			got, err := dest.puller.FetchManifest(t.Context(), source.source(), whole)
			if !errors.Is(err, transfer.ErrManifestCorrupt) {
				t.Fatalf("err = %v, want ErrManifestCorrupt — the destination accepted a manifest "+
					"that does not check out", err)
			}
			// Rejected, not stored: the caller is handed nothing it could act
			// on, so there is no partially-trusted description anywhere.
			if got.ChunkCount() != 0 || !got.BlobHash.IsZero() {
				t.Fatalf("a rejected manifest was returned anyway: %d chunks for %s",
					got.ChunkCount(), got.BlobHash)
			}
		})
	}

	// The control: with the honest manifest served, the same call succeeds. It
	// is what tells "the destination rejects a bad manifest" from "the
	// destination rejects every manifest".
	source.manifests.store(good)
	source.manifests.serveInstead(whole, good)
	dest := newDestination(t, dst)
	if _, err := dest.puller.FetchManifest(t.Context(), source.source(), whole); err != nil {
		t.Fatalf("the honest manifest was rejected too, so the rejections above prove nothing: %v", err)
	}
}

// ---------------------------------------------------------------------------
// the pinned client, and the rule about bare clients

// A manifest read is refused at the handshake when the source is not a member
// of this node's fabric — no status code involved.
func TestAManifestReadIsRefusedAtTheHandshake(t *testing.T) {
	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	// The source's listener does not know the destination.
	sourceRoot := newTrustRoot(src.member())

	content := []byte("bytes a stranger may not describe")
	source := startSource(t, src, sourceRoot, content)
	whole, _, err := hashing.HashReader(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	source.manifests.store(manifestFor(t, whole, 700, 300, 1500))

	dest := newDestination(t, dst)
	_, err = dest.puller.FetchManifest(t.Context(), source.source(), whole)
	if err == nil {
		t.Fatal("a peer the source does not pin read a manifest from it")
	}
	// Not an HTTP refusal. If this were a status the session would exist, and
	// every route that forgets to check would be reachable over it.
	for _, sentinel := range []error{
		transfer.ErrSourceHasNoManifest, transfer.ErrSourceLacksBlob, transfer.ErrSourceRefused,
	} {
		if errors.Is(err, sentinel) {
			t.Fatalf("the refusal arrived as an HTTP status (%v), not as a failed handshake", err)
		}
	}
}

// Nothing in this package builds a bare http.Client, and this is what says so
// in a way that fails.
//
// The package doc has claimed it since M4-09, and a doc comment is not a
// check. One `&http.Client{...}` anywhere here is one line from an unpinned
// transport that follows redirects, and the traffic would look identical until
// the day it mattered (ADR-0012, §32). mtls.Client and the CheckRedirect it is
// given in clientFor are the only construction path.
func TestNothingInThisPackageBuildsABareHTTPClient(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files++
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if ident.Name == "http" && (sel.Sel.Name == "Client" || sel.Sel.Name == "Transport") {
				t.Errorf("%s builds an http.%s directly, at %s. Every client in this package "+
					"comes from clientFor, which pins one peer's key and refuses redirects",
					name, sel.Sel.Name, fset.Position(lit.Pos()))
			}
			return true
		})
	}
	if files == 0 {
		t.Fatal("the scan found no files, so this test proves nothing")
	}
}
