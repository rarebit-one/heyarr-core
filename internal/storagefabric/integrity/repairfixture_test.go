package integrity_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// repairConfig is a deliberately tiny chunker, so a few kilobytes of test
// fixture produce a manifest with enough chunks for "the fetch is proportional
// to the damage" to be a measurable claim rather than an assertion about one
// chunk.
func repairConfig() chunking.Config { return chunking.Config{Min: 64, Avg: 256, Max: 1024} }

// --- the manifest store -----------------------------------------------------

// fakeManifests is an in-memory manifests.Store. The real one is exercised in
// internal/persistence/catalog.
type fakeManifests struct {
	byBlob map[string]manifests.Manifest
	states map[string]manifests.State
}

func newFakeManifests() *fakeManifests {
	return &fakeManifests{byBlob: map[string]manifests.Manifest{}, states: map[string]manifests.State{}}
}

func (f *fakeManifests) Load(_ context.Context, blob hashing.Hash) (manifests.Manifest, bool, error) {
	m, ok := f.byBlob[blob.String()]
	return m, ok, nil
}

func (f *fakeManifests) Save(_ context.Context, m manifests.Manifest) error {
	f.byBlob[m.BlobHash.String()] = m
	f.states[m.BlobHash.String()] = manifests.StatePresent
	return nil
}

func (f *fakeManifests) StateOf(_ context.Context, blob hashing.Hash) (manifests.State, error) {
	if s, ok := f.states[blob.String()]; ok {
		return s, nil
	}
	return manifests.StateUndecided, nil
}

func (f *fakeManifests) RecordNotRequired(_ context.Context, blob hashing.Hash, _ string) error {
	f.states[blob.String()] = manifests.StateNotRequired
	return nil
}

func (f *fakeManifests) Discard(_ context.Context, blob hashing.Hash) error {
	delete(f.byBlob, blob.String())
	delete(f.states, blob.String())
	return nil
}

// --- the peer ---------------------------------------------------------------

// fakePeer is a ChunkSource: a peer that holds the good bytes of some blobs.
//
// It is the only place in these tests that stands in for the M5-06/M5-07
// ranged fetch, and it is deliberately capable of misbehaving — an absent
// blob, a corrupt copy, a short read — because every one of those is a case
// the repairer has to survive without touching the local original.
type fakePeer struct {
	bytesByBlob map[string][]byte

	// corrupt flips a byte in every chunk it serves: the peer's copy is
	// damaged too.
	corrupt bool
	// short truncates every chunk it serves by one byte.
	short bool

	calls     int
	bytesSent int64
	// onFetch runs before each fetch. The crash tests use it to die at a
	// chosen point inside the repair window.
	onFetch func(n int)
}

func newFakePeer() *fakePeer { return &fakePeer{bytesByBlob: map[string][]byte{}} }

func (p *fakePeer) hold(h hashing.Hash, b []byte) { p.bytesByBlob[h.String()] = b }

func (p *fakePeer) FetchChunk(
	_ context.Context, blob hashing.Hash, chunk chunking.Chunk,
) ([]byte, error) {
	p.calls++
	if p.onFetch != nil {
		p.onFetch(p.calls)
	}
	whole, ok := p.bytesByBlob[blob.String()]
	if !ok {
		return nil, integrity.ErrNoSource
	}
	end := chunk.End()
	if end > int64(len(whole)) {
		return nil, integrity.ErrNoSource
	}
	out := append([]byte(nil), whole[chunk.Offset:end]...)
	if p.corrupt {
		out[0] ^= 0xFF
	}
	if p.short && len(out) > 0 {
		out = out[:len(out)-1]
	}
	p.bytesSent += int64(len(out))
	return out, nil
}

// --- the fixture ------------------------------------------------------------

type repairFixture struct {
	t     *testing.T
	store *cas.FS
	cat   *fakeCatalog
	man   *fakeManifests
	peer  *fakePeer
	clock *clock
}

func newRepairFixture(t *testing.T) *repairFixture {
	t.Helper()
	store, err := cas.OpenFS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatal(err)
	}
	return &repairFixture{
		t: t, store: store, cat: newFakeCatalog(),
		man: newFakeManifests(), peer: newFakePeer(), clock: newClock(),
	}
}

func (f *repairFixture) repairer() *integrity.Repairer {
	f.t.Helper()
	r, err := integrity.NewRepairer(integrity.RepairOptions{
		Store: f.store, Manifests: f.man, Catalog: f.cat, Source: f.peer, Clock: f.clock,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

// store a blob, record it in the catalog, build and save its manifest, and
// give the peer the good bytes.
func (f *repairFixture) putChunked(content []byte) hashing.Hash {
	f.t.Helper()
	desc, err := f.store.Put(f.t.Context(), bytes.NewReader(content))
	if err != nil {
		f.t.Fatal(err)
	}
	f.cat.add(desc.Hash, desc.Size, 1)
	m := buildManifest(f.t, desc.Hash, content)
	if err := f.man.Save(f.t.Context(), m); err != nil {
		f.t.Fatal(err)
	}
	f.peer.hold(desc.Hash, content)
	return desc.Hash
}

// buildManifest chunks content and builds the manifest naming blob.
func buildManifest(t *testing.T, blob hashing.Hash, content []byte) manifests.Manifest {
	t.Helper()
	cfg := repairConfig()
	ch, err := chunking.New(bytes.NewReader(content), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var chunks []chunking.Chunk
	for {
		c, err := ch.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, c)
	}
	m, err := manifests.Build(blob, cfg, chunks, time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// blobFile finds a blob's file by walking, so no test encodes the store's
// fanout — the layout is private to the cas package (§18).
func (f *repairFixture) blobFile(h hashing.Hash) string {
	f.t.Helper()
	var found string
	err := filepath.WalkDir(f.store.Root(), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if d.Name() == h.Hex() && strings.Contains(path, string(os.PathSeparator)+"blobs"+string(os.PathSeparator)) {
			found = path
		}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return found
}

// damage rewrites a region of a blob's bytes in place at the same length,
// which is what an external tool does to an original the CAS shares an inode
// with (#43), and the one fault a shallow check cannot see.
func damageFile(t *testing.T, path string, offset int64, replacement []byte) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(replacement, offset); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o440); err != nil {
		t.Fatal(err)
	}
}

// sha256File is the byte-level identity of a file, used for "byte-identical to
// its damaged self" assertions. Deliberately a different hash from the one the
// code under test uses: an assertion that reached for hashing.HashFile would be
// asserting with the same machinery it is testing.
func sha256File(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- a test path
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sha256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// treeSnapshot is every file under the store root with its sha256, so "nothing
// was written" can be asserted over the whole store rather than over the one
// path a test remembered to look at.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, rErr := filepath.Rel(root, path)
		if rErr != nil {
			return rErr
		}
		out[rel] = sha256File(t, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertSameTree(t *testing.T, before, after map[string]string) {
	t.Helper()
	for path, sum := range before {
		got, ok := after[path]
		if !ok {
			t.Errorf("the store lost %s", path)
			continue
		}
		if got != sum {
			t.Errorf("the store rewrote %s", path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("the store gained %s, having been asked to repair nothing", path)
		}
	}
}

// quarantinedFiles lists the quarantine artefacts, by full path.
func (f *repairFixture) quarantinedFiles() []string {
	f.t.Helper()
	dir := filepath.Join(f.store.Root(), "quarantine")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		f.t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out
}

// pseudoRandom is a deterministic fixture generator: the same seed always
// produces the same bytes, so chunk boundaries are stable across runs and a
// failure is reproducible from the seed alone.
func pseudoRandom(seed int64, n int) []byte {
	r := rand.New(rand.NewSource(seed)) // #nosec G404 -- test fixture data, not a secret
	b := make([]byte, n)
	_, _ = r.Read(b)
	return b
}
