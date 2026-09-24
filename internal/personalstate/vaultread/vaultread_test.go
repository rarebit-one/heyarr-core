package vaultread_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/rarebit-one/voidbind-go/encryption"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/vaultframe"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/vaultread"
)

func mustKey(t *testing.T) encryption.SpaceKey {
	t.Helper()
	sk, err := encryption.NewSpaceKey()
	if err != nil {
		t.Fatal(err)
	}
	return sk
}

// pattern is deterministic plaintext so a decrypted byte range can be checked
// against exactly what was sealed at that offset.
func pattern(n int64) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*7 + 3) % 251)
	}
	return b
}

// memFetcher is a BlobFetcher backed by in-memory blobs, recording every
// FetchRange call so a test can assert which frames were actually fetched.
type memFetcher struct {
	blobs   map[string][]byte
	fetched []fetchCall
}

type fetchCall struct {
	blobID     string
	start, end int64
}

func newMemFetcher() *memFetcher {
	return &memFetcher{blobs: map[string][]byte{}}
}

func (f *memFetcher) Fetch(_ context.Context, blobID string) ([]byte, error) {
	b, ok := f.blobs[blobID]
	if !ok {
		return nil, errors.New("no such blob: " + blobID)
	}
	return b, nil
}

func (f *memFetcher) FetchRange(_ context.Context, blobID string, start, end int64) ([]byte, error) {
	b, ok := f.blobs[blobID]
	if !ok {
		return nil, errors.New("no such blob: " + blobID)
	}
	if start < 0 || end > int64(len(b)) || start > end {
		return nil, errors.New("range out of bounds")
	}
	f.fetched = append(f.fetched, fetchCall{blobID: blobID, start: start, end: end})
	return b[start:end], nil
}

// seedVault seals plaintext into the fetcher's blob store under sk and returns
// the manifest blob id (the argument ReadRange/ReadAll take) and the manifest
// itself (for building expectations in tests).
func seedVault(t *testing.T, f *memFetcher, sk encryption.SpaceKey, plaintext []byte) (manifestBlobID string, m vaultframe.Manifest) {
	t.Helper()
	var content bytes.Buffer
	m, err := vaultframe.Seal(sk, bytes.NewReader(plaintext), &content)
	if err != nil {
		t.Fatalf("vaultframe.Seal: %v", err)
	}
	f.blobs[m.Content] = content.Bytes()

	sealedManifest, err := vaultframe.SealManifest(sk, m)
	if err != nil {
		t.Fatalf("vaultframe.SealManifest: %v", err)
	}
	// A manifest blob id is content-addressed like any other blob; for the
	// fetcher's purposes any stable key works, so a fixed label is enough.
	manifestBlobID = "blake3:" + "manifest-for-" + m.FileID
	f.blobs[manifestBlobID] = sealedManifest
	return manifestBlobID, m
}

// TestReadRangeAcrossFrameBoundary: a range that starts in one frame and ends
// in the next decrypts to exactly the same bytes as the original plaintext.
func TestReadRangeAcrossFrameBoundary(t *testing.T) {
	sk := mustKey(t)
	f := newMemFetcher()
	plaintext := pattern(3*vaultframe.FrameSize + 500)
	manifestBlobID, _ := seedVault(t, f, sk, plaintext)

	off := int64(vaultframe.FrameSize) + 1000
	n := int64(vaultframe.FrameSize) // spans the 1->2 frame boundary

	got, err := vaultread.ReadRange(context.Background(), f, sk, manifestBlobID, off, n)
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}
	if !bytes.Equal(got, plaintext[off:off+n]) {
		t.Fatal("ReadRange returned the wrong bytes across a frame boundary")
	}
}

// TestReadRangeFetchesOnlyCoveringFrames: only the frames the range covers are
// fetched — not the whole content blob, and not neighbouring frames.
func TestReadRangeFetchesOnlyCoveringFrames(t *testing.T) {
	sk := mustKey(t)
	f := newMemFetcher()
	plaintext := pattern(4*vaultframe.FrameSize + 200)
	manifestBlobID, m := seedVault(t, f, sk, plaintext)

	startToIndex := map[int64]int{}
	for i := 0; i < m.FrameCount; i++ {
		s, _ := m.FrameByteRange(i)
		startToIndex[s] = i
	}

	off := int64(vaultframe.FrameSize) + 1000
	n := int64(vaultframe.FrameSize) // frames 1 and 2 only

	if _, err := vaultread.ReadRange(context.Background(), f, sk, manifestBlobID, off, n); err != nil {
		t.Fatalf("ReadRange: %v", err)
	}

	// One Fetch for the manifest is not a FetchRange call; only content-blob
	// range fetches should be recorded here.
	got := map[int]bool{}
	for _, c := range f.fetched {
		if c.blobID != m.Content {
			t.Fatalf("fetched range from unexpected blob %s", c.blobID)
		}
		got[startToIndex[c.start]] = true
	}
	want := map[int]bool{1: true, 2: true}
	if len(got) != len(want) || !got[1] || !got[2] {
		t.Fatalf("expected to fetch only frames 1 and 2, fetched %v", got)
	}
}

// TestReadAllReconstructsWholeFile: ReadAll reproduces the exact plaintext,
// fetching each frame exactly once.
func TestReadAllReconstructsWholeFile(t *testing.T) {
	sk := mustKey(t)
	f := newMemFetcher()
	plaintext := pattern(2*vaultframe.FrameSize + 777)
	manifestBlobID, m := seedVault(t, f, sk, plaintext)

	got, err := vaultread.ReadAll(context.Background(), f, sk, manifestBlobID)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatal("ReadAll did not reconstruct the original plaintext")
	}
	if len(f.fetched) != m.FrameCount {
		t.Fatalf("expected exactly %d FetchRange calls (one per frame), got %d", m.FrameCount, len(f.fetched))
	}
}

// TestReadAllEmptyFile: a zero-byte vault file reads back as zero bytes with
// no content-blob fetch at all.
func TestReadAllEmptyFile(t *testing.T) {
	sk := mustKey(t)
	f := newMemFetcher()
	manifestBlobID, _ := seedVault(t, f, sk, nil)

	got, err := vaultread.ReadAll(context.Background(), f, sk, manifestBlobID)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 bytes, got %d", len(got))
	}
	if len(f.fetched) != 0 {
		t.Fatalf("expected no content-blob fetches for an empty file, got %d", len(f.fetched))
	}
}

// TestReadRangeWrongKeyFails: the wrong space key cannot open the manifest,
// so the read fails cleanly rather than returning garbage plaintext.
func TestReadRangeWrongKeyFails(t *testing.T) {
	sk := mustKey(t)
	wrong := mustKey(t)
	f := newMemFetcher()
	manifestBlobID, _ := seedVault(t, f, sk, pattern(vaultframe.FrameSize+42))

	if _, err := vaultread.ReadRange(context.Background(), f, wrong, manifestBlobID, 0, 10); err == nil {
		t.Fatal("expected ReadRange with the wrong key to fail")
	}
	if _, err := vaultread.ReadAll(context.Background(), f, wrong, manifestBlobID); err == nil {
		t.Fatal("expected ReadAll with the wrong key to fail")
	}
}

// TestReadRangeOutOfBounds: a range past the end of the file is refused
// rather than silently truncated.
func TestReadRangeOutOfBounds(t *testing.T) {
	sk := mustKey(t)
	f := newMemFetcher()
	plaintext := pattern(100)
	manifestBlobID, _ := seedVault(t, f, sk, plaintext)

	if _, err := vaultread.ReadRange(context.Background(), f, sk, manifestBlobID, 50, 100); err == nil {
		t.Fatal("expected a range past end-of-file to fail")
	}
}
