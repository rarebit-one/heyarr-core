package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/pieces"
)

// stagingStore is a cas.Store that holds nothing whole and one blob in part.
//
// It implements only what peerPieces reaches for, and every method a test does
// not arrange fails loudly rather than returning a zero value, so a test that
// takes an unintended path says so.
type stagingStore struct {
	cas.Store

	// partial is the staging bytes, indexed by blob.
	partial map[string][]byte
	// progress is the availability record, indexed by blob.
	progress map[string]string
	// readAt counts lock-free partial reads.
	readAt int
}

func newStagingStore() *stagingStore {
	return &stagingStore{partial: map[string][]byte{}, progress: map[string]string{}}
}

func (s *stagingStore) Stat(context.Context, hashing.Hash) (cas.Descriptor, error) {
	// Nothing is held whole; that is the case this fixture is about.
	return cas.Descriptor{}, cas.ErrNotFound
}

func (s *stagingStore) LoadPieceProgress(blob hashing.Hash) (string, error) {
	return s.progress[blob.String()], nil
}

func (s *stagingStore) ReadPartialAt(blob hashing.Hash, b []byte, off int64) (int, error) {
	s.readAt++
	staged, ok := s.partial[blob.String()]
	if !ok {
		return 0, cas.ErrNotFound
	}
	if off >= int64(len(staged)) {
		return 0, io.EOF
	}
	return copy(b, staged[off:]), nil
}

func blobNamed(t *testing.T, phrase string) hashing.Hash {
	t.Helper()
	sum := sha256.Sum256([]byte(phrase))
	return hashing.MustParse("blake3:" + hex.EncodeToString(sum[:]))
}

// 🔴 THE assertion of ADR-0043, and the one place where believing the bitset
// wrongly harms somebody else rather than failing locally.
//
// A partial's length is a HIGH-WATER MARK. Piece 9 landing before piece 3
// extends the file past piece 3's offset, so the staging file has bytes there
// — they are simply not piece 3. Every other consumer of a partial re-hashes
// what it read and fails closed; a peer served those bytes writes them into ITS
// staging file and counts a piece that was never fetched.
//
// So the bitset gates the read, and the presence of bytes at the offset is not
// evidence of anything.
func TestAPieceTheRecordDoesNotClaimIsRefusedEvenThoughTheBytesAreThere(t *testing.T) {
	blob := blobNamed(t, "a blob whose piece 9 landed before piece 3")
	store := newStagingStore()

	g, err := pieces.For(20 * pieces.MinPieceLength)
	if err != nil {
		t.Fatal(err)
	}
	// The file is long enough to cover piece 3 — because piece 9 landed — but
	// only pieces 0 and 9 are claimed.
	staged := bytes.Repeat([]byte{0xEE}, int(g.Size))
	store.partial[blob.String()] = staged
	have := pieces.NewAvailability(g.Count())
	have.Add(0)
	have.Add(9)
	store.progress[blob.String()] = pieces.Encode(g, have)

	p := peerPieces{store: store}

	// The gap. Bytes exist at its offset; it was never fetched.
	if _, err := p.ReadPiece(context.Background(), blob, 3); !errors.Is(err, peerapi.ErrNoSuchPiece) {
		t.Errorf("piece 3 was served (err = %v), so a peer would have been sent "+
			"bytes that were never that piece", err)
	}
	if store.readAt != 0 {
		t.Errorf("the staging file was read %d times for an unclaimed piece", store.readAt)
	}

	// The claimed one is served, so the refusal above is a gate and not a
	// blanket refusal.
	got, err := p.ReadPiece(context.Background(), blob, 9)
	if err != nil {
		t.Fatalf("piece 9 is claimed and was refused: %v", err)
	}
	off, length, _ := g.Range(9)
	if int64(len(got)) != length {
		t.Errorf("served %d bytes for piece 9, want %d from offset %d", len(got), length, off)
	}
	if !bytes.Equal(got, staged[off:off+length]) {
		t.Error("piece 9's bytes are not the staging file's bytes at piece 9's offset")
	}
}

// A piece past the end of the blob's division is refused rather than read.
//
// The index arrives from a peer, so it is input: without this it would become
// an offset past the end of the staging file and either a short read or a
// panic, depending on the store.
func TestAPieceIndexPastTheEndIsRefused(t *testing.T) {
	blob := blobNamed(t, "a small blob")
	store := newStagingStore()

	g, err := pieces.For(3 * pieces.MinPieceLength)
	if err != nil {
		t.Fatal(err)
	}
	store.partial[blob.String()] = bytes.Repeat([]byte{1}, int(g.Size))
	have := pieces.NewAvailability(g.Count())
	have.Add(0)
	store.progress[blob.String()] = pieces.Encode(g, have)

	p := peerPieces{store: store}
	for _, index := range []int{g.Count(), g.Count() + 1, 1 << 20} {
		if _, err := p.ReadPiece(context.Background(), blob, index); !errors.Is(err, peerapi.ErrNoSuchPiece) {
			t.Errorf("piece %d of a %d-piece blob was not refused: %v", index, g.Count(), err)
		}
	}
}

// A blob with no progress record at all is not held even in part.
func TestAPieceOfABlobWithNoTransferIsRefused(t *testing.T) {
	p := peerPieces{store: newStagingStore()}
	_, err := p.ReadPiece(context.Background(), blobNamed(t, "never heard of it"), 0)
	if !errors.Is(err, peerapi.ErrNoSuchPiece) {
		t.Errorf("err = %v, want ErrNoSuchPiece", err)
	}
}

// A store that cannot read a partial serves nothing from one, rather than
// pretending. peerPieces asserts the capability at the call site; a store
// without it honestly has no in-flight transfers to serve from.
func TestAStoreThatCannotReadPartialsServesNoPieceFromOne(t *testing.T) {
	p := peerPieces{store: statOnlyStore{}}
	_, err := p.ReadPiece(context.Background(), blobNamed(t, "anything"), 0)
	if !errors.Is(err, peerapi.ErrNoSuchPiece) {
		t.Errorf("err = %v, want ErrNoSuchPiece", err)
	}
}

// statOnlyStore holds nothing and cannot stage.
type statOnlyStore struct{ cas.Store }

func (statOnlyStore) Stat(context.Context, hashing.Hash) (cas.Descriptor, error) {
	return cas.Descriptor{}, cas.ErrNotFound
}
