// Every response in this file is drained and closed by the helper that made
// the request, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by peerSend
package peerapi_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

// M5-05's acceptance, from the SOURCE's side (§20, ADR-0030, ADR-0034).
//
// The subject of this file is the three things the route must not do: it must
// not generate a manifest to answer, it must not decide anything on the
// destination's behalf, and it must not be reachable by a peer whose
// membership record is gone.
//
// The sharpest assertion here is an assertion on an ABSENCE — that answering a
// 404 generated no manifest and enqueued no chunk_blob job. An absence
// assertion whose subject cannot exist is not an assertion, it is a sentence,
// so the fixture below deliberately CAN generate: it implements a generation
// method, counts every call to it, and the honest build never reaches it. That
// is what makes the check capable of failing, and it does fail when the route
// is sabotaged into generating on demand.

// ---------------------------------------------------------------------------
// the fixture

// sourceManifests is a source node's manifest store, and its accounting.
//
// It answers reads from a map, exactly as the catalogue does, and it records
// what was asked of it. The counters are the point: "the route answered 404"
// is satisfied by a route that chunked a blob first and failed, and only the
// counters tell those apart.
type sourceManifests struct {
	mu sync.Mutex
	// stored is the manifests this node actually has.
	stored map[string]manifests.Manifest
	// held is every blob this node holds bytes for, manifest or not.
	held map[string]bool
	// exempt is the blobs a decision was recorded about (§16's not_required).
	exempt map[string]bool

	// reads counts ChunkManifest calls: one read per request, and no loop that
	// asks again hoping for a better answer.
	reads int
	// generated counts manifests this node produced while serving a request.
	// It must stay at zero. See GenerateChunkManifest.
	generated int
	// enqueued is the chunk_blob jobs this node scheduled while serving a
	// request. It must stay empty.
	enqueued []string
}

func newSourceManifests() *sourceManifests {
	return &sourceManifests{
		stored: map[string]manifests.Manifest{},
		held:   map[string]bool{},
		exempt: map[string]bool{},
	}
}

// hold records that this node has the bytes, with no manifest and no decision
// — §16's `undecided`, which is the state the "nothing happened" test is about.
func (s *sourceManifests) hold(blob hashing.Hash) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held[blob.String()] = true
}

// exempts records the decision that a blob will never need a manifest.
func (s *sourceManifests) exempts(blob hashing.Hash) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held[blob.String()] = true
	s.exempt[blob.String()] = true
}

// store records a manifest this node has.
func (s *sourceManifests) store(m manifests.Manifest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held[m.BlobHash.String()] = true
	s.stored[m.BlobHash.String()] = m
}

// ChunkManifest is the read the route makes. It writes nothing.
func (s *sourceManifests) ChunkManifest(
	_ context.Context, blob hashing.Hash,
) (manifests.Manifest, manifests.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	key := blob.String()
	if !s.held[key] {
		return manifests.Manifest{}, "", fmt.Errorf("%w: %s", peerapi.ErrNoSuchBlob, blob)
	}
	if m, ok := s.stored[key]; ok {
		return m, manifests.StatePresent, nil
	}
	if s.exempt[key] {
		return manifests.Manifest{}, manifests.StateNotRequired, nil
	}
	return manifests.Manifest{}, manifests.StateUndecided, nil
}

// GenerateChunkManifest is the thing that must not happen.
//
// It exists so that "no manifest was generated" has a subject. A fixture with
// no way to generate would pass that assertion on every build ever written,
// including one that generates — because the check would be measuring a
// capability nobody has rather than a decision nobody took. This one chunks,
// records the manifest, counts the generation and enqueues the chunk_blob job
// a real implementation would, so the absence below is an absence of
// something that could have happened.
//
// The honest route never calls it. Nothing in peerapi knows this method
// exists.
func (s *sourceManifests) GenerateChunkManifest(
	_ context.Context, blob hashing.Hash,
) (manifests.Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generated++
	s.enqueued = append(s.enqueued, "chunk_blob:"+blob.String())
	m, err := manifests.Build(blob, chunking.DefaultConfig(),
		fixtureChunks(nil, 4096, 8192), time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC))
	if err != nil {
		return manifests.Manifest{}, err
	}
	s.stored[blob.String()] = m
	return m, nil
}

// counts reports the accounting under the lock.
func (s *sourceManifests) counts() (reads, generated int, enqueued []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads, s.generated, append([]string(nil), s.enqueued...)
}

// serveWithManifests is serve() with a manifest source behind the route.
func serveWithManifests(
	t *testing.T, self *peerNode, members mtls.Membership, src peerapi.ManifestSource,
) *listener {
	t.Helper()
	logs := &syncBuffer{}
	srv, err := peerapi.New(peerapi.Options{
		Addr:       "127.0.0.1:0",
		Material:   self.material,
		Members:    members,
		SelfPeerID: self.peerID,
		Manifests:  src,
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

func (l *listener) manifestURL(hash string) string {
	return "https://" + l.addr + peerapi.BlobManifestPath(hash)
}

// fixtureChunks builds a chunk sequence whose INDEX order and DIGEST order are
// different, and whose lengths are all different too.
//
// This is the lesson from the manifest model's own fixture: an agent's
// ordering check passed there because the digests were numbered monotonically,
// so index order and digest order coincided and a handler that sorted by
// digest would have looked correct. Here the digests are the BLAKE3 of the
// chunk's content, and the content is chosen so that the resulting digests are
// neither ascending nor descending across the sequence — asserted by
// [assertOrderIsNotDerivable], which fails the test rather than the fixture
// quietly weakening it.
func fixtureChunks(t *testing.T, lengths ...int64) []chunking.Chunk {
	if t != nil {
		t.Helper()
	}
	out := make([]chunking.Chunk, 0, len(lengths))
	var off int64
	for i, n := range lengths {
		// Distinct content per chunk, so the digests are unrelated to the
		// index rather than derived from it.
		content := bytes.Repeat([]byte{byte('a' + i)}, int(n))
		h := hashing.New()
		_, _ = h.Write(content)
		out = append(out, chunking.Chunk{Offset: off, Length: n, Digest: h.Sum()})
		off += n
	}
	return out
}

// assertOrderIsNotDerivable fails when the fixture's index order happens to
// coincide with its digest order, in either direction.
//
// Without it, the ordering assertions in this file could pass against a
// handler that sorted the chunks — which is exactly the shape of pass this
// repository has already shipped once.
func assertOrderIsNotDerivable(t *testing.T, chunks []chunking.Chunk) {
	t.Helper()
	if len(chunks) < 3 {
		t.Fatalf("an ordering fixture of %d chunks cannot distinguish a sort from a sequence",
			len(chunks))
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
		t.Fatalf("the fixture's digests are in %s order, so index order and digest order coincide "+
			"and a handler that sorted by digest would pass every assertion below",
			map[bool]string{true: "ascending", false: "descending"}[ascending])
	}
	lengths := map[int64]bool{}
	for _, c := range chunks {
		if lengths[c.Length] {
			t.Fatal("two chunks in the fixture have the same length, so a length-ordered handler " +
				"could reproduce the sequence by accident")
		}
		lengths[c.Length] = true
	}
}

// storedManifest is the manifest the fixture source holds, deterministic so it
// can also be a golden file.
func storedManifest(t *testing.T) manifests.Manifest {
	t.Helper()
	blob := hashing.MustParse("blake3:" + strings.Repeat("11", 32))
	chunks := fixtureChunks(t, 4096, 65536, 12288, 300, 8192)
	assertOrderIsNotDerivable(t, chunks)
	m, err := manifests.Build(blob, chunking.DefaultConfig(), chunks,
		time.Date(2026, 8, 24, 9, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func decodeManifest(t *testing.T, body string) peerapi.ManifestResponse {
	t.Helper()
	var out peerapi.ManifestResponse
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("the peer surface answered something that is not a manifest: %v\n%s", err, body)
	}
	return out
}

func decodeProblem(t *testing.T, body string) problem.Problem {
	t.Helper()
	var out problem.Problem
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("the refusal is not a problem document: %v\n%s", err, body)
	}
	return out
}

// ---------------------------------------------------------------------------
// the happy path, asserted as a SEQUENCE

// A destination fetches a manifest over mTLS and gets the chunk sequence the
// source stored — in order, by digest, element by element.
//
// The assertion is on the digest sequence rather than on the count or on a set
// membership, because a set of individually valid chunks assembled in the
// wrong order is a set of valid chunks and the wrong file (ADR-0034), and it
// is the one fault the per-chunk digests cannot see.
func TestAPeerFetchesTheChunkSequenceTheSourceStoredInOrder(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	dest := newPeerNode(t, "peer-destination", "destination")
	root := newTrustRoot(source.member(), dest.member())

	src := newSourceManifests()
	stored := storedManifest(t)
	src.store(stored)
	l := serveWithManifests(t, source, root, src)

	client := dialler(t, dest, mtls.PinnedKey(source.member()))
	status, body, _, err := peerSend(t, client, http.MethodGet,
		l.manifestURL(stored.BlobHash.String()), "")
	if err != nil {
		t.Fatalf("reading a manifest from a peer: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}

	got := decodeManifest(t, body)
	if got.BlobHash != stored.BlobHash.String() {
		t.Errorf("the manifest describes %q, want %q", got.BlobHash, stored.BlobHash)
	}
	// The digest names the MANIFEST. It is the source's own record of what it
	// holds, and it is what lets the destination check it got that.
	if got.Digest != stored.Digest.String() {
		t.Errorf("manifest digest = %q, want %q", got.Digest, stored.Digest)
	}
	if got.Digest == got.BlobHash {
		t.Error("the manifest digest equals the blob digest — one of them is being used as the other")
	}
	if got.ServedBy != source.peerID {
		t.Errorf("served_by = %q, want the answering node %q", got.ServedBy, source.peerID)
	}

	// THE assertion: the same digests, in the same positions.
	if len(got.Chunks) != len(stored.Chunks) {
		t.Fatalf("the manifest carries %d chunks, the source stored %d",
			len(got.Chunks), len(stored.Chunks))
	}
	for i, want := range stored.Chunks {
		if got.Chunks[i].Digest != want.Digest.String() {
			t.Errorf("chunk %d is %s, the source stored %s at that index — the SEQUENCE differs, "+
				"and a sequence in the wrong order reassembles the wrong file",
				i, got.Chunks[i].Digest, want.Digest)
		}
		if got.Chunks[i].Offset != want.Offset || got.Chunks[i].Length != want.Length {
			t.Errorf("chunk %d spans %d+%d, the source stored %d+%d",
				i, got.Chunks[i].Offset, got.Chunks[i].Length, want.Offset, want.Length)
		}
	}

	// And the sequence that arrived reproduces the digest the source recorded,
	// which is the check a destination actually runs.
	rebuilt, err := got.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if err := rebuilt.Validate(); err != nil {
		t.Fatalf("the manifest that arrived does not check out against its own digest: %v", err)
	}

	// The source recorded what it served, the way the content route does.
	if logs := l.logs.String(); !strings.Contains(logs, "served a chunk manifest to a peer") {
		t.Errorf("the source served a manifest and recorded nothing:\n%s", logs)
	}
	if logs := l.logs.String(); !strings.Contains(logs, dest.peerID) {
		t.Errorf("the source did not record WHICH peer it served:\n%s", logs)
	}
}

// The source formed no opinion about the caller: two different peers get
// byte-identical answers.
//
// This is the "no negotiation" property stated as something falsifiable. A
// route that had started tailoring its answer — omitting chunks the caller was
// thought to have, ordering by anything caller-derived — would differ here,
// and would differ in a way no single-caller test could see.
func TestTheManifestIsIdenticalForEveryCaller(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	one := newPeerNode(t, "peer-one", "one")
	two := newPeerNode(t, "peer-two", "two")
	root := newTrustRoot(source.member(), one.member(), two.member())

	src := newSourceManifests()
	stored := storedManifest(t)
	src.store(stored)
	l := serveWithManifests(t, source, root, src)
	url := l.manifestURL(stored.BlobHash.String())

	_, first, _, err := peerSend(t, dialler(t, one, mtls.PinnedKey(source.member())),
		http.MethodGet, url, "")
	if err != nil {
		t.Fatal(err)
	}
	_, second, _, err := peerSend(t, dialler(t, two, mtls.PinnedKey(source.member())),
		http.MethodGet, url, "")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("two peers were served different descriptions of one blob — the source is deciding "+
			"something about the caller (ADR-0030)\none: %s\ntwo: %s", first, second)
	}
}

// ---------------------------------------------------------------------------
// 🔴 404, and nothing happened

// The assertion this issue turns on.
//
// A blob the source HOLDS and has not chunked answers 404 — and the source
// generated no manifest and enqueued no chunk_blob job doing it. The second
// half is the one that matters: a route that chunked on demand would answer
// 200 here and look like a feature, and a route that chunked and failed would
// answer 404 and look like this test passing.
func TestAskingForAManifestThatDoesNotExistGeneratesNothing(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	dest := newPeerNode(t, "peer-destination", "destination")
	root := newTrustRoot(source.member(), dest.member())

	src := newSourceManifests()
	unchunked := hashing.MustParse("blake3:" + strings.Repeat("22", 32))
	src.hold(unchunked)
	l := serveWithManifests(t, source, root, src)

	client := dialler(t, dest, mtls.PinnedKey(source.member()))
	status, body, _, err := peerSend(t, client, http.MethodGet,
		l.manifestURL(unchunked.String()), "")
	if err != nil {
		t.Fatalf("asking a peer for a manifest it does not have: %v", err)
	}
	// The counters are read and asserted BEFORE the status, deliberately. A
	// route that chunked the blob and then failed answers 404 too, and a test
	// that stopped at the status would report that as a pass — the status is
	// the cheap half of this assertion and it must not mask the expensive one.
	reads, generated, enqueued := src.counts()
	// 🔴 The absence, and it has a subject: the fixture CAN generate, counts
	// it, and enqueues the job a real implementation would.
	if generated != 0 {
		t.Errorf("answering a manifest read produced %d manifest(s). A GET that chunks is a full "+
			"read of the blob on the source — a remote denial of service with a polite name "+
			"(ADR-0034)", generated)
	}
	if len(enqueued) != 0 {
		t.Errorf("answering a manifest read enqueued %v. Deciding to chunk is a separate call by a "+
			"caller that wanted to, never a side effect of somebody asking", enqueued)
	}
	if reads != 1 {
		t.Errorf("the route made %d reads for one request; it must ask once and accept the answer", reads)
	}
	if status != http.StatusNotFound {
		t.Fatalf("a blob with no manifest answered %d, want 404 — the third state is the answer, "+
			"not a condition to resolve\n%s", status, body)
	}
	// The source still has no manifest for those bytes afterwards. The
	// counters above are about what happened during the request; this is about
	// what the request left behind.
	if _, state, err := src.ChunkManifest(t.Context(), unchunked); err != nil ||
		state != manifests.StateUndecided {
		t.Errorf("after the 404 the blob is in state %q (err %v), want undecided — the request "+
			"changed the source's state", state, err)
	}

	// And it said which of §16's manifest-less answers it was, which is the
	// diagnostic signal a source being asked for manifests it never made owes
	// an operator.
	if logs := l.logs.String(); !strings.Contains(logs, "a peer asked for a chunk manifest this node does not have") {
		t.Errorf("the source answered 404 and recorded nothing:\n%s", logs)
	}
	if logs := l.logs.String(); !strings.Contains(logs, "manifest_state="+string(manifests.StateUndecided)) {
		t.Errorf("the source did not record WHICH manifest-less state it was in:\n%s", logs)
	}
}

// The other manifest-less state answers the same way. §16's `not_required` is
// a decision somebody took rather than an absence, and it is just as final:
// asking must not overturn it by producing the manifest it says is unnecessary.
func TestABlobDecidedToNeedNoManifestAlsoAnswers404AndGeneratesNothing(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	dest := newPeerNode(t, "peer-destination", "destination")
	root := newTrustRoot(source.member(), dest.member())

	src := newSourceManifests()
	small := hashing.MustParse("blake3:" + strings.Repeat("33", 32))
	src.exempts(small)
	l := serveWithManifests(t, source, root, src)

	client := dialler(t, dest, mtls.PinnedKey(source.member()))
	status, body, _, err := peerSend(t, client, http.MethodGet, l.manifestURL(small.String()), "")
	if err != nil {
		t.Fatal(err)
	}
	// The counters first, for the reason the test above gives.
	if _, generated, enqueued := src.counts(); generated != 0 || len(enqueued) != 0 {
		t.Errorf("asking about an exempt blob generated %d manifest(s) and enqueued %v — the "+
			"recorded decision was overturned by somebody asking", generated, enqueued)
	}
	if status != http.StatusNotFound {
		t.Fatalf("a blob recorded as needing no manifest answered %d, want 404\n%s", status, body)
	}
	if got := decodeProblem(t, body).Type; got != problem.TypeNoChunkManifest {
		t.Errorf("type = %q, want %q", got, problem.TypeNoChunkManifest)
	}
	if logs := l.logs.String(); !strings.Contains(logs, "manifest_state="+string(manifests.StateNotRequired)) {
		t.Errorf("the source did not record that this was a recorded decision rather than an "+
			"absence:\n%s", logs)
	}
}

// ---------------------------------------------------------------------------
// the three answers stay apart

// 404-no-manifest, 404-no-such-blob and 503 are distinguished by the `type`
// URI, which is the contract. The destination's differing ACTION is asserted
// on the other side of the wire, in internal/peer/transfer.
func TestTheThreeManifestlessAnswersAreDistinguishable(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	dest := newPeerNode(t, "peer-destination", "destination")
	root := newTrustRoot(source.member(), dest.member())

	src := newSourceManifests()
	held := hashing.MustParse("blake3:" + strings.Repeat("44", 32))
	absent := hashing.MustParse("blake3:" + strings.Repeat("55", 32))
	src.hold(held)
	l := serveWithManifests(t, source, root, src)
	client := dialler(t, dest, mtls.PinnedKey(source.member()))

	status, body, _, err := peerSend(t, client, http.MethodGet, l.manifestURL(held.String()), "")
	if err != nil {
		t.Fatal(err)
	}
	noManifest := decodeProblem(t, body)
	if status != http.StatusNotFound || noManifest.Type != problem.TypeNoChunkManifest {
		t.Fatalf("a held blob with no manifest answered %d %q", status, noManifest.Type)
	}

	status, body, _, err = peerSend(t, client, http.MethodGet, l.manifestURL(absent.String()), "")
	if err != nil {
		t.Fatal(err)
	}
	noBlob := decodeProblem(t, body)
	if status != http.StatusNotFound || noBlob.Type != problem.TypeNotFound {
		t.Fatalf("a blob this node does not hold answered %d %q", status, noBlob.Type)
	}

	// Both are 404, and the status alone therefore says nothing. The type URI
	// is the whole discriminator, and it is not a substring of the other —
	// this repository has shipped an assert_contains that matched a prefix
	// once already.
	if noManifest.Type == noBlob.Type {
		t.Fatal("the two 404s carry the same type URI, so a destination cannot tell them apart " +
			"and must guess between 'pull whole from here' and 'try another source'")
	}
	if strings.Contains(noManifest.Type, noBlob.Type) || strings.Contains(noBlob.Type, noManifest.Type) {
		t.Fatalf("%q and %q — one type URI contains the other, and a `contains` check on either "+
			"would match both", noManifest.Type, noBlob.Type)
	}

	// And a node with no manifest source at all answers 503, matching the
	// content route: it is not serving content here, so there is nothing to
	// try again for.
	bare := serveWithManifests(t, source, root, nil)
	bareClient := dialler(t, dest, mtls.PinnedKey(source.member()))
	status, body, _, err = peerSend(t, bareClient, http.MethodGet, bare.manifestURL(held.String()), "")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("a node with no content behind its peer surface answered %d, want 503\n%s", status, body)
	}

	// A malformed digest is a fourth thing again, and no amount of retrying
	// will make it a hash.
	status, _, _, err = peerSend(t, client, http.MethodGet, l.manifestURL("not-a-digest"), "")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("a malformed digest answered %d, want 400", status)
	}
}

// ---------------------------------------------------------------------------
// the mount is inside the mTLS chain

// serverSeen wraps a trust root and records which keys the SERVER was asked
// about.
//
// It exists because "the request failed" is not evidence about the server. A
// refusal can come from the dialler, from the network, or from a listener that
// is not running, and all three look identical to a client. This records what
// the listener's own membership check saw, so the assertion below is that the
// SERVER reached a decision about a specific key — not that a client got an
// error.
type serverSeen struct {
	inner *trustRoot
	mu    sync.Mutex
	keys  []string
}

func (s *serverSeen) Lookup(ctx context.Context, pub []byte) (mtls.Peer, error) {
	s.mu.Lock()
	s.keys = append(s.keys, string(pub))
	s.mu.Unlock()
	return s.inner.Lookup(ctx, pub)
}

// sawSince reports how many times the server was asked about a key after mark.
func (s *serverSeen) sawSince(mark int, pub ed25519.PublicKey) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, k := range s.keys[mark:] {
		if k == string(pub) {
			n++
		}
	}
	return n
}

func (s *serverSeen) mark() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.keys)
}

// A revoked peer is refused, and it is refused by the middleware chain the
// route is mounted inside — not by anything the handler decided.
//
// A new route on an existing listener is exactly where a mount gets attached
// to the wrong chain, and the failure is silent: the route works, the tests
// pass, and the only observable difference is who can reach it.
//
// The two halves are different facts and both are needed. The REUSED
// connection is what proves the mount: TLS was completed while this peer was a
// member, so nothing at the transport can refuse the second request and only
// the per-request membership check can. The NEW connection is refused at the
// handshake, and it is asserted from the SERVER's side — which key the
// listener's own trust root was asked about, and what it recorded — because
// TLS 1.3 puts the client certificate in the client's last flight and a
// client-side Handshake() returns success against a listener that accepts
// every key.
func TestARevokedPeerCannotReadAManifest(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	dest := newPeerNode(t, "peer-destination", "destination")
	root := newTrustRoot(source.member(), dest.member())
	seen := &serverSeen{inner: root}

	src := newSourceManifests()
	stored := storedManifest(t)
	src.store(stored)
	// The listener consults `seen`, which delegates to the same membership
	// table the dialler uses and records what the SERVER was asked.
	l := serveWithManifests(t, source, seen, src)
	url := l.manifestURL(stored.BlobHash.String())

	client := dialler(t, dest, mtls.PinnedKey(source.member()))

	// The control. Without it every refusal below passes against a surface
	// that serves nobody.
	status, body, _, err := peerSend(t, client, http.MethodGet, url, "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("an enrolled peer could not read a manifest: %d %v\n%s", status, err, body)
	}
	served := strings.Count(l.logs.String(), "served a chunk manifest to a peer")
	if served != 1 {
		t.Fatalf("the source recorded %d manifest reads for one request", served)
	}

	// Revoke, which in this design means delete the record: there is no
	// revocation list to add to (ADR-0012).
	root.remove(dest.pub)
	mark := seen.mark()

	// Half one: the connection the peer already holds. Nothing at the
	// transport can refuse this — the handshake is long finished — so a 200
	// here would mean the route is mounted outside the chain that re-checks
	// membership on every request.
	status, body, reused, err := peerSend(t, client, http.MethodGet, url, "")
	switch {
	case err != nil:
		t.Fatalf("after revocation the request failed at the transport (%v) rather than being "+
			"refused on the connection the peer already had — this run does not show that the "+
			"route is inside the membership chain", err)
	case status != http.StatusForbidden:
		t.Fatalf("a revoked peer read a manifest over its existing connection and got %d, want 403. "+
			"The route is mounted outside requirePeerIdentity\n%s", status, body)
	case !reused:
		t.Fatal("the refused request opened a NEW connection, so it says nothing about the " +
			"per-request check")
	}
	if !strings.Contains(body, "not a member") {
		t.Errorf("the refusal does not say why: %s", body)
	}
	// And nothing was served in the process.
	if got := strings.Count(l.logs.String(), "served a chunk manifest to a peer"); got != served {
		t.Errorf("the source served %d manifests after the revocation, want 0", got-served)
	}

	// Half two: a fresh connection, refused at the handshake. handshakeTo
	// writes a request and reads a byte back, because TLS 1.3 alone would
	// report success here on a listener that accepts everybody.
	if err := handshakeTo(t, l.addr, clientConfigFor(t, dest, root)); err == nil {
		t.Fatal("a revoked peer completed a usable connection to the manifest route. A status code " +
			"later would be too late: the session exists, and every route that forgets to check is " +
			"reachable over it")
	}
	// The server-side half of that: the listener's own trust root was asked
	// about THIS key, after the revocation, and refused it. Without this the
	// assertion above is satisfied by a listener that was never running.
	if n := seen.sawSince(mark, dest.pub); n == 0 {
		t.Fatal("the listener was never asked about the revoked peer's key, so the failure above " +
			"was not the server refusing it")
	}
	if logs := l.logs.String(); !strings.Contains(logs, "not a member") {
		t.Errorf("the listener refused without recording why:\n%s", logs)
	}
}

// ---------------------------------------------------------------------------
// the wire shape

// The response shape and both refusals are golden files. The `type` URI is the
// contract a destination branches on, so a change to either 404 is a change to
// what every destination does, and it should require editing a committed file
// rather than only a string.
func TestTheManifestWireShapeIsAGoldenFile(t *testing.T) {
	source := newPeerNode(t, "peer-source", "source")
	dest := newPeerNode(t, "peer-destination", "destination")
	root := newTrustRoot(source.member(), dest.member())

	src := newSourceManifests()
	stored := storedManifest(t)
	src.store(stored)
	held := hashing.MustParse("blake3:" + strings.Repeat("22", 32))
	src.hold(held)
	absent := hashing.MustParse("blake3:" + strings.Repeat("55", 32))
	l := serveWithManifests(t, source, root, src)
	client := dialler(t, dest, mtls.PinnedKey(source.member()))

	for _, tt := range []struct {
		name   string
		hash   string
		golden string
	}{
		{"the manifest", stored.BlobHash.String(), "manifest.json"},
		{"no manifest for a blob this node holds", held.String(), "manifest_absent.json"},
		{"no such blob", absent.String(), "manifest_no_such_blob.json"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, body, _, err := peerSend(t, client, http.MethodGet, l.manifestURL(tt.hash), "")
			if err != nil {
				t.Fatal(err)
			}
			var indented bytes.Buffer
			if err := json.Indent(&indented, []byte(body), "", "  "); err != nil {
				t.Fatalf("the response is not JSON: %v\n%s", err, body)
			}
			indented.WriteByte('\n')
			testutil.Golden(t, filepath.Join("testdata", tt.golden), indented.Bytes())
		})
	}
}
