package vaultread_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/rarebit-one/void-which-binds-go/encryption"
	"github.com/rarebit-one/void-which-binds-go/hashing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/vaultframe"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/vaultread"
)

// blobID is b's content address, "blake3:<hex>".
func blobID(b []byte) string {
	h := hashing.New()
	_, _ = h.Write(b)
	return h.Sum().String()
}

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
	// A manifest blob id is content-addressed like any other blob, and the
	// reader checks it, so it is the real BLAKE3 of the sealed manifest.
	manifestBlobID = blobID(sealedManifest)
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

// TestSubstitutedManifestRejected: a node that answers the drive entry's
// manifest id with ANOTHER manifest of the same space — sealed under the same
// key, so it decrypts perfectly well — is refused before it is opened, as an
// integrity failure rather than a wrong key or an absent object (Invariant 1).
func TestSubstitutedManifestRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sk := mustKey(t)
	f := newMemFetcher()
	wantID, _ := seedVault(t, f, sk, pattern(vaultframe.FrameSize+10))
	otherID, _ := seedVault(t, f, sk, []byte("a different file in the same space"))
	f.blobs[wantID] = f.blobs[otherID]

	reads := map[string]func() error{
		"ReadAll": func() error { _, err := vaultread.ReadAll(ctx, f, sk, wantID); return err },
		"ReadRange": func() error {
			_, err := vaultread.ReadRange(ctx, f, sk, wantID, 0, 5)
			return err
		},
		"OpenManifestWithKeys": func() error {
			_, _, err := vaultread.OpenManifestWithKeys([]encryption.SpaceKey{sk}, wantID, f.blobs[wantID])
			return err
		},
	}
	for name, read := range reads {
		if err := read(); !errors.Is(err, vaultread.ErrBlobIntegrity) {
			t.Errorf("%s of a substituted manifest: %v, want ErrBlobIntegrity", name, err)
		}
	}
}

// TestTamperedContentRejected: one flipped byte in the content blob is an
// integrity failure. A whole-file read catches it by hashing the content blob
// against the manifest's content id before decrypting anything; a range read
// over the tampered frame catches it by that frame's AEAD tag.
func TestTamperedContentRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sk := mustKey(t)
	size := int64(2*vaultframe.FrameSize + 3)
	for _, tc := range []struct {
		name   string
		at     func(n int) int
		off, n int64
	}{
		{"first byte", func(int) int { return 0 }, 0, 10},
		{"last byte", func(n int) int { return n - 1 }, size - 3, 3},
	} {
		f := newMemFetcher()
		id, m := seedVault(t, f, sk, pattern(size))
		tampered := bytes.Clone(f.blobs[m.Content])
		tampered[tc.at(len(tampered))] ^= 0x01
		f.blobs[m.Content] = tampered
		if _, err := vaultread.ReadAll(ctx, f, sk, id); !errors.Is(err, vaultread.ErrBlobIntegrity) {
			t.Errorf("ReadAll with the %s of the content tampered: %v, want ErrBlobIntegrity", tc.name, err)
		}
		if _, err := vaultread.ReadRange(ctx, f, sk, id, tc.off, tc.n); !errors.Is(err, vaultread.ErrBlobIntegrity) {
			t.Errorf("ReadRange over the tampered %s: %v, want ErrBlobIntegrity", tc.name, err)
		}
	}
}

// TestSubstitutedFrameRejected: a range read cannot check the whole content
// blob, so it rests on the per-frame AEAD binding of (file_id, frame_index). A
// frame lifted from ANOTHER file of the same space, at the same index and byte
// range, decrypts under the space key but names the other file — refused as an
// integrity failure; so is this file's own frame moved to another index.
func TestSubstitutedFrameRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sk := mustKey(t)
	size := int64(2*vaultframe.FrameSize + 3)

	for _, tc := range []struct {
		name   string
		splice func(f *memFetcher, m, other vaultframe.Manifest) []byte
	}{
		{"a frame of another file", func(f *memFetcher, m, other vaultframe.Manifest) []byte {
			b := bytes.Clone(f.blobs[m.Content])
			s, e := m.FrameByteRange(1)
			copy(b[s:e], f.blobs[other.Content][s:e])
			return b
		}},
		{"this file's frame at another index", func(f *memFetcher, m, _ vaultframe.Manifest) []byte {
			b := bytes.Clone(f.blobs[m.Content])
			s0, e0 := m.FrameByteRange(0)
			s1, _ := m.FrameByteRange(1)
			copy(b[s1:], f.blobs[m.Content][s0:e0])
			return b
		}},
	} {
		f := newMemFetcher()
		id, m := seedVault(t, f, sk, pattern(size))
		_, other := seedVault(t, f, sk, pattern(size))
		f.blobs[m.Content] = tc.splice(f, m, other)

		off := int64(vaultframe.FrameSize) + 5
		if _, err := vaultread.ReadRange(ctx, f, sk, id, off, 10); !errors.Is(err, vaultread.ErrBlobIntegrity) ||
			!errors.Is(err, vaultframe.ErrFrame) {
			t.Errorf("ReadRange over %s: %v, want ErrBlobIntegrity (ErrFrame)", tc.name, err)
		}
		if _, err := vaultread.ReadAll(ctx, f, sk, id); !errors.Is(err, vaultread.ErrBlobIntegrity) {
			t.Errorf("ReadAll over %s: %v, want ErrBlobIntegrity", tc.name, err)
		}
		// The frame that was not touched still reads.
		if got, err := vaultread.ReadRange(ctx, f, sk, id, 0, 10); err != nil || !bytes.Equal(got, pattern(size)[:10]) {
			t.Errorf("ReadRange of the untouched frame beside %s: %v", tc.name, err)
		}
	}
}

// TestMissingBlobIsNotIntegrity: a blob the node cannot serve stays a fetch
// error, so a caller can still tell "absent" from "wrong bytes".
func TestMissingBlobIsNotIntegrity(t *testing.T) {
	t.Parallel()
	sk := mustKey(t)
	f := newMemFetcher()
	id, m := seedVault(t, f, sk, pattern(100))
	delete(f.blobs, m.Content)
	if _, err := vaultread.ReadAll(context.Background(), f, sk, id); err == nil || errors.Is(err, vaultread.ErrBlobIntegrity) {
		t.Fatalf("a missing content blob: %v, want a fetch error that is not ErrBlobIntegrity", err)
	}
}

// TestDigestMismatchReturnsNoPlaintext: a whole-file read decrypts as it
// streams, so every frame can open cleanly and the content still not be the
// blob the manifest names. Here the manifest records a content id the served
// bytes do not hash to, though each frame is genuine: the read must fail as an
// integrity error and hand back no plaintext at all. The last-byte tamper is
// the same contract on a frame that does not open.
func TestDigestMismatchReturnsNoPlaintext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sk := mustKey(t)
	size := int64(2*vaultframe.FrameSize + 3)

	// Frames that all open, under a manifest whose content id is wrong.
	f := newMemFetcher()
	_, m := seedVault(t, f, sk, pattern(size))
	genuine := f.blobs[m.Content]
	m.Content = blobID([]byte("not the content blob"))
	f.blobs[m.Content] = genuine
	sealed, err := vaultframe.SealManifest(sk, m)
	if err != nil {
		t.Fatal(err)
	}
	id := blobID(sealed)
	f.blobs[id] = sealed

	got, err := vaultread.ReadAll(ctx, f, sk, id)
	if !errors.Is(err, vaultread.ErrBlobIntegrity) {
		t.Fatalf("ReadAll with a final-digest mismatch: %v, want ErrBlobIntegrity", err)
	}
	if got != nil {
		t.Fatalf("ReadAll returned %d bytes of plaintext on a digest mismatch", len(got))
	}
	// The frames themselves are genuine, so a range read (which rests on the
	// per-frame binding, not the whole-blob id) still reads them.
	if part, err := vaultread.ReadRange(ctx, f, sk, id, 0, 10); err != nil || !bytes.Equal(part, pattern(size)[:10]) {
		t.Fatalf("ReadRange of genuine frames: %v", err)
	}

	// The last byte tampered: the last frame does not open.
	f2 := newMemFetcher()
	id2, m2 := seedVault(t, f2, sk, pattern(size))
	tampered := bytes.Clone(f2.blobs[m2.Content])
	tampered[len(tampered)-1] ^= 0x01
	f2.blobs[m2.Content] = tampered
	got, err = vaultread.ReadAll(ctx, f2, sk, id2)
	if !errors.Is(err, vaultread.ErrBlobIntegrity) || got != nil {
		t.Fatalf("ReadAll with the last byte tampered: %d bytes, %v; want no plaintext and ErrBlobIntegrity", len(got), err)
	}
}
