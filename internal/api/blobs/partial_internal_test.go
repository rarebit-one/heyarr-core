package blobs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// These tests drive partialBlobReader directly against a fake byte-level
// PartialSource, so the block-then-serve and the "never serve an unlanded range"
// invariant (ADR-0042/0043) are proven in the reader's own terms — byte offsets,
// not pieces. The piece→byte translation is the controller adapter's job and is
// tested there against a real CAS; here the source is fully controllable so the
// reader's ordering and blocking can be pinned down deterministically.

type byteRange struct{ from, to int64 }

// fakePartialSource models a blob still arriving as a set of landed byte ranges
// over known content. A hole (an unlanded byte) reads back as zero — exactly the
// staging file's behaviour — so a reader that serves a hole emits zeroes that do
// not match the content, which is what the invariant test turns on.
type fakePartialSource struct {
	mu       sync.Mutex
	content  []byte
	landed   []byteRange
	inflight bool
}

func newFakeSource(content []byte, landed ...byteRange) *fakePartialSource {
	return &fakePartialSource{content: content, landed: landed, inflight: true}
}

func (f *fakePartialSource) size() int64 { return int64(len(f.content)) }

func (f *fakePartialSource) land(r byteRange) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.landed = append(f.landed, r)
}

func (f *fakePartialSource) end() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inflight = false
}

func (f *fakePartialSource) isLandedLocked(off int64) bool {
	for _, r := range f.landed {
		if off >= r.from && off < r.to {
			return true
		}
	}
	return false
}

func (f *fakePartialSource) ArrivingSize(_ context.Context, _ hashing.Hash) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.content)), f.inflight, nil
}

func (f *fakePartialSource) Available(_ context.Context, _ hashing.Hash, off int64) (int64, bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.inflight {
		return 0, false, false, nil
	}
	for _, r := range f.landed {
		if off >= r.from && off < r.to {
			return r.to, true, true, nil
		}
	}
	return 0, false, true, nil
}

func (f *fakePartialSource) ReadPartialAt(_ hashing.Hash, b []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if off >= int64(len(f.content)) {
		return 0, io.EOF
	}
	end := off + int64(len(b))
	if end > int64(len(f.content)) {
		end = int64(len(f.content))
	}
	n := 0
	for pos := off; pos < end; pos++ {
		if f.isLandedLocked(pos) {
			b[n] = f.content[pos]
		} else {
			b[n] = 0 // a hole
		}
		n++
	}
	return n, nil
}

// emptyStore is a real CAS with nothing in it, so store.Open always reports
// not-found — the reader stays on the partial path. The transition test Puts the
// whole blob into it to prove the switch to a completed replica.
func emptyStore(t *testing.T) *cas.FS {
	t.Helper()
	fs, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func testContent(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i%251 + 1) // never zero, so a hole is distinguishable
	}
	return b
}

func TestPartialReaderServesLandedRangeWithoutBlocking(t *testing.T) {
	t.Parallel()
	content := testContent(30000)
	src := newFakeSource(content, byteRange{0, 10000}) // [10000,30000) is a hole
	r := &partialBlobReader{
		ctx:   context.Background(),
		store: emptyStore(t),
		src:   src,
		wait: func(context.Context) error {
			t.Error("blocked over a landed range")
			return errors.New("unexpected block")
		},
		blob: hashing.Hash{},
		size: src.size(),
	}
	if _, err := r.Seek(1000, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("ReadFull over a landed range: %v", err)
	}
	if !bytes.Equal(buf, content[1000:1000+4096]) {
		t.Fatal("served bytes do not match the content over a landed range")
	}
}

// TestPartialReaderNeverServesUnlandedRange is the invariant — sabotage-verify
// it: defeat the ok gate in Read and this must fail. A hole reads back as zeroes;
// the reader must WAIT on it, never emit it. Here [10000,30000) never lands, so a
// correct reader serves [0,10000) and blocks; it must never cross into the hole.
func TestPartialReaderNeverServesUnlandedRange(t *testing.T) {
	t.Parallel()
	content := testContent(30000)
	src := newFakeSource(content, byteRange{0, 10000})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := 0
	r := &partialBlobReader{
		ctx:   ctx,
		store: emptyStore(t),
		src:   src,
		wait: func(context.Context) error {
			waits++
			if waits >= 3 {
				cancel() // the hole never fills; stop the reader
			}
			return ctx.Err()
		},
		blob: hashing.Hash{},
		size: src.size(),
	}
	got, err := io.ReadAll(r)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation while waiting on the hole, got %v", err)
	}
	if waits == 0 {
		t.Fatal("reader never blocked — it must wait on the unlanded range, not read the hole")
	}
	if int64(len(got)) != 10000 {
		t.Fatalf("emitted %d bytes; a correct reader serves exactly the landed [0,10000) and blocks", len(got))
	}
	if !bytes.Equal(got, content[:10000]) {
		t.Fatal("bytes emitted before the hole do not match the content — a hole was served as data")
	}
}

func TestPartialReaderBlocksThenServes(t *testing.T) {
	t.Parallel()
	content := testContent(30000)
	src := newFakeSource(content, byteRange{0, 10000}, byteRange{20000, 30000}) // gap [10000,20000)
	blocked := false
	r := &partialBlobReader{
		ctx:   context.Background(),
		store: emptyStore(t),
		src:   src,
		wait: func(context.Context) error {
			blocked = true
			src.land(byteRange{10000, 20000}) // the missing run arrives
			return nil
		},
		blob: hashing.Hash{},
		size: src.size(),
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll over a blob completed mid-read: %v", err)
	}
	if !blocked {
		t.Fatal("reader never blocked, so the block-then-serve path was not exercised")
	}
	if !bytes.Equal(got, content) {
		t.Fatal("served bytes do not match the content after the missing run landed")
	}
}

// TestPartialReaderTransparentTransition proves §84's transparent transition: a
// reader that started on a partial finishes from the completed whole blob once
// the transfer publishes it.
func TestPartialReaderTransparentTransition(t *testing.T) {
	t.Parallel()
	content := testContent(30000)
	store := emptyStore(t)
	blob, _, err := hashing.HashReader(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	src := newFakeSource(content, byteRange{0, 10000}) // rest arrives via completion
	r := &partialBlobReader{
		ctx:   context.Background(),
		store: store,
		src:   src,
		wait: func(context.Context) error {
			// The transfer completes and publishes the whole blob.
			if _, perr := store.Put(context.Background(), bytes.NewReader(content)); perr != nil {
				t.Fatal(perr)
			}
			src.end()
			return nil
		},
		blob: blob,
		size: src.size(),
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll across the transition to a whole replica: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("served bytes do not match after the transparent transition to the whole blob")
	}
	if r.whole == nil {
		t.Fatal("reader never transitioned to the whole blob; it should have opened the published replica")
	}
}

// TestPartialReaderTransferGone proves a reader whose transfer is abandoned
// mid-flight — nothing more landing, the blob never completed — ends in
// ErrTransferGone rather than serving a hole or spinning.
func TestPartialReaderTransferGone(t *testing.T) {
	t.Parallel()
	content := testContent(30000)
	src := newFakeSource(content, byteRange{0, 10000})
	r := &partialBlobReader{
		ctx:   context.Background(),
		store: emptyStore(t),
		src:   src,
		wait: func(context.Context) error {
			src.end() // the transfer gives up
			return nil
		},
		blob: hashing.Hash{},
		size: src.size(),
	}
	if _, err := io.ReadAll(r); !errors.Is(err, ErrTransferGone) {
		t.Fatalf("expected ErrTransferGone once the abandoned transfer ended, got %v", err)
	}
}
