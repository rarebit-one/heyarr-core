package repairsource_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/repairsource"
	"github.com/rarebit-one/heyarr-core/internal/peer/transfer"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// A peer list that answers with exactly what a test says the catalog believes.
type peerList struct {
	peers []replication.Source
	err   error
	asked []string
}

func (p *peerList) BlobSources(_ context.Context, blob string) ([]replication.Source, error) {
	p.asked = append(p.asked, blob)
	return p.peers, p.err
}

// A fetcher that records which peers it was asked, in order, and answers each
// from a script. The ORDER is the interesting record: this package's whole job
// is which candidate it tries next, and a fetcher that only counted calls
// could not fail an ordering fault.
type scriptedFetcher struct {
	answers map[string]answer
	tried   []string
}

type answer struct {
	bytes []byte
	err   error
}

func (f *scriptedFetcher) FetchChunk(_ context.Context, src replication.Source,
	_ hashing.Hash, _ chunking.Chunk,
) ([]byte, error) {
	f.tried = append(f.tried, src.PeerID)
	a, ok := f.answers[src.PeerID]
	if !ok {
		return nil, errors.New("the test did not script an answer for " + src.PeerID)
	}
	return a.bytes, a.err
}

// usable builds a candidate that membership can authenticate. The key is
// non-empty and the endpoint is set, which is all Usable checks.
func usable(id string) replication.Source {
	return replication.Source{
		PeerID: id, Name: id, Endpoint: "https://" + id + ".invalid:8443",
		PublicKey: []byte{1, 2, 3},
	}
}

func fixtureChunk(t *testing.T) chunking.Chunk {
	t.Helper()
	// A real digest of real bytes, so that nothing here is a digest that could
	// only have come from the code under test.
	body := []byte("the bytes a peer would serve for one chunk")
	d, _, err := hashing.HashReader(strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	return chunking.Chunk{Offset: 0, Length: int64(len(body)), Digest: d}
}

func fixtureBlob(t *testing.T) hashing.Hash {
	t.Helper()
	h, _, err := hashing.HashReader(strings.NewReader("a whole blob, of which the chunk is a part"))
	if err != nil {
		t.Fatal(err)
	}
	// The blob's digest and the chunk's must be DIFFERENT, or a fault that
	// confused the two could not fail any assertion below.
	if h.Equal(fixtureChunk(t).Digest) {
		t.Fatal("the fixture's blob digest and chunk digest are the same value")
	}
	return h
}

func newSource(t *testing.T, peers *peerList, fetcher *scriptedFetcher) *repairsource.Source {
	t.Helper()
	s, err := repairsource.New(repairsource.Options{Sources: peers, Fetcher: fetcher})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The ordinary case: the best candidate answers and its bytes come back.
func TestTheFirstCandidateThatAnswersSuppliesTheChunk(t *testing.T) {
	want := []byte("the bytes a peer would serve for one chunk")
	peers := &peerList{peers: []replication.Source{usable("peer-a"), usable("peer-b")}}
	fetcher := &scriptedFetcher{answers: map[string]answer{
		"peer-a": {bytes: want},
		"peer-b": {bytes: []byte("peer-b should never have been asked")},
	}}
	blob := fixtureBlob(t)

	got, err := newSource(t, peers, fetcher).FetchChunk(t.Context(), blob, fixtureChunk(t))
	if err != nil {
		t.Fatalf("FetchChunk: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("FetchChunk returned %q, want %q", got, want)
	}
	if strings.Join(fetcher.tried, ",") != "peer-a" {
		t.Errorf("the peers tried were %v, want only peer-a — a candidate that answered must end "+
			"the walk, or a repair costs one request per peer that happens to be listed",
			fetcher.tried)
	}
	if strings.Join(peers.asked, ",") != blob.String() {
		t.Errorf("the catalog was asked for %v, want the blob being repaired (%s)", peers.asked, blob)
	}
}

// A peer whose inventory is out of date is an ordinary next candidate.
func TestAPeerThatNoLongerHoldsTheBlobIsSkippedRatherThanFatal(t *testing.T) {
	want := []byte("the bytes a peer would serve for one chunk")
	peers := &peerList{peers: []replication.Source{usable("peer-stale"), usable("peer-good")}}
	fetcher := &scriptedFetcher{answers: map[string]answer{
		"peer-stale": {err: transfer.ErrSourceLacksBlob},
		"peer-good":  {bytes: want},
	}}

	got, err := newSource(t, peers, fetcher).FetchChunk(t.Context(), fixtureBlob(t), fixtureChunk(t))
	if err != nil {
		t.Fatalf("FetchChunk: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("FetchChunk returned %q, want the second peer's bytes", got)
	}
	// Spelled out rather than compared to a length: the ORDER is the claim.
	if strings.Join(fetcher.tried, ",") != "peer-stale,peer-good" {
		t.Errorf("the peers tried were %v, want peer-stale then peer-good — `replicas` is a "+
			"belief and a 404 means try the next one", fetcher.tried)
	}
}

// Nobody has it, which ADR-0038 makes an ordinary day rather than a fault.
func TestNoPeerHoldingTheBlobIsErrNoSource(t *testing.T) {
	for _, tc := range []struct {
		name  string
		peers []replication.Source
		reply map[string]answer
		tried string
	}{
		{
			name:  "the catalog lists nobody",
			peers: nil,
			reply: map[string]answer{},
			tried: "",
		},
		{
			name:  "every listed peer has dropped it",
			peers: []replication.Source{usable("peer-a"), usable("peer-b")},
			reply: map[string]answer{
				"peer-a": {err: transfer.ErrSourceLacksBlob},
				"peer-b": {err: transfer.ErrSourceLacksBlob},
			},
			tried: "peer-a,peer-b",
		},
		{
			name: "no listed peer can be authenticated",
			peers: []replication.Source{
				{PeerID: "peer-nokey", Endpoint: "https://x.invalid:8443"},
				{PeerID: "peer-noaddr", PublicKey: []byte{9}},
			},
			reply: map[string]answer{},
			// Refused before a connection is opened: membership is the only
			// trust root here (ADR-0012), so neither is ever dialled.
			tried: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peers := &peerList{peers: tc.peers}
			fetcher := &scriptedFetcher{answers: tc.reply}
			_, err := newSource(t, peers, fetcher).FetchChunk(
				t.Context(), fixtureBlob(t), fixtureChunk(t))
			if !errors.Is(err, integrity.ErrNoSource) {
				t.Fatalf("FetchChunk = %v, want integrity.ErrNoSource — repair turns that into "+
					"OutcomeUnreachable and leaves the blob alone (ADR-0038)", err)
			}
			if strings.Join(fetcher.tried, ",") != tc.tried {
				t.Errorf("the peers dialled were %v, want %q", fetcher.tried, tc.tried)
			}
		})
	}
}

// A peer that served the WRONG BYTES is a fault, and must not be folded into
// "nobody had it" — the two lead to different outcomes and different actions.
func TestAPeerServingCorruptBytesIsAFaultRatherThanAnAbsence(t *testing.T) {
	peers := &peerList{peers: []replication.Source{usable("peer-liar"), usable("peer-good")}}
	fetcher := &scriptedFetcher{answers: map[string]answer{
		"peer-liar": {err: transfer.ErrChunkCorrupt},
		"peer-good": {bytes: []byte("the bytes a peer would serve for one chunk")},
	}}

	_, err := newSource(t, peers, fetcher).FetchChunk(t.Context(), fixtureBlob(t), fixtureChunk(t))
	if err == nil {
		t.Fatal("a peer serving bytes that do not match the manifest was accepted")
	}
	if errors.Is(err, integrity.ErrNoSource) {
		t.Errorf("a peer serving corrupt bytes reported as ErrNoSource (%v) — that reads as an "+
			"ordinary unreachable day, and this is a peer that cannot back its own inventory", err)
	}
	if !errors.Is(err, transfer.ErrChunkCorrupt) {
		t.Errorf("error = %v, want it to carry transfer.ErrChunkCorrupt", err)
	}
	if !strings.Contains(err.Error(), "peer-liar") {
		t.Errorf("error = %q, and it does not name the peer that served the bytes", err)
	}
	// It stops rather than quietly succeeding from the next peer. A repair
	// that silently routed around a lying peer is a repair that leaves the lie
	// in place and nobody looking for it.
	if strings.Join(fetcher.tried, ",") != "peer-liar" {
		t.Errorf("the peers tried were %v, want the walk to stop at peer-liar", fetcher.tried)
	}
}

// A catalog that cannot answer is neither an absence nor a corrupt peer.
func TestAFailingCatalogIsNotReportedAsNoSource(t *testing.T) {
	boom := errors.New("the database is closed")
	peers := &peerList{err: boom}
	fetcher := &scriptedFetcher{answers: map[string]answer{}}

	_, err := newSource(t, peers, fetcher).FetchChunk(t.Context(), fixtureBlob(t), fixtureChunk(t))
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the catalog's own error", err)
	}
	if errors.Is(err, integrity.ErrNoSource) {
		t.Error("a catalog that could not be read reported as ErrNoSource — repair would leave " +
			"the blob alone and call it ordinary, when nothing was actually asked")
	}
}

// A cancelled context stops the walk instead of working through every peer.
func TestACancelledContextStopsTheWalk(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	peers := &peerList{peers: []replication.Source{usable("peer-a"), usable("peer-b")}}
	fetcher := &scriptedFetcher{answers: map[string]answer{}}

	_, err := newSource(t, peers, fetcher).FetchChunk(ctx, fixtureBlob(t), fixtureChunk(t))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if len(fetcher.tried) != 0 {
		t.Errorf("peers %v were dialled after the context was cancelled", fetcher.tried)
	}
}

// The dependencies are required, and the message says which is missing.
func TestNewRefusesAnIncompleteSource(t *testing.T) {
	if _, err := repairsource.New(repairsource.Options{
		Fetcher: &scriptedFetcher{},
	}); err == nil {
		t.Error("New accepted a source with no peer list")
	}
	if _, err := repairsource.New(repairsource.Options{
		Sources: &peerList{},
	}); err == nil {
		t.Error("New accepted a source with no fetcher")
	}
}
