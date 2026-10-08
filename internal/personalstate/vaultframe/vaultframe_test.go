package vaultframe_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/rarebit-one/void-which-binds-go/encryption"
	"github.com/rarebit-one/void-which-binds-go/hashing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/vaultframe"
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

// sealToBytes seals plaintext and returns the ciphertext blob and manifest.
func sealToBytes(t *testing.T, sk encryption.SpaceKey, plaintext []byte) ([]byte, vaultframe.Manifest) {
	t.Helper()
	var buf bytes.Buffer
	m, err := vaultframe.Seal(sk, bytes.NewReader(plaintext), &buf)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return buf.Bytes(), m
}

// TestSealOpenRoundTrip: sealing then opening every frame reproduces the plaintext,
// and the manifest describes the geometry (a 2.5-frame file is three frames).
func TestSealOpenRoundTrip(t *testing.T) {
	sk := mustKey(t)
	plaintext := pattern(2*vaultframe.FrameSize + 12345)
	cipher, m := sealToBytes(t, sk, plaintext)

	if m.FrameCount != 3 {
		t.Fatalf("want 3 frames, got %d", m.FrameCount)
	}
	if m.PlaintextSize != int64(len(plaintext)) {
		t.Fatalf("plaintext size = %d, want %d", m.PlaintextSize, len(plaintext))
	}

	// Walk frames by their manifest byte ranges, open each, concatenate. The last
	// frame's end must be exactly the ciphertext length — proving the offset math
	// (and the nonce/tag constants it rests on) against real sealed bytes.
	var got []byte
	for i := 0; i < m.FrameCount; i++ {
		start, end := m.FrameByteRange(i)
		data, err := m.OpenFrame(sk, i, cipher[start:end])
		if err != nil {
			t.Fatalf("OpenFrame(%d): %v", i, err)
		}
		got = append(got, data...)
		if i == m.FrameCount-1 && end != int64(len(cipher)) {
			t.Fatalf("last frame ends at %d, ciphertext is %d bytes", end, len(cipher))
		}
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatal("round-trip did not reproduce the plaintext")
	}
}

// TestContentIDIsBlake3OfCiphertext: the manifest names the content blob by the
// BLAKE3 of its ciphertext (never the plaintext, ADR-0021).
func TestContentIDIsBlake3OfCiphertext(t *testing.T) {
	sk := mustKey(t)
	cipher, m := sealToBytes(t, sk, pattern(vaultframe.FrameSize+7))
	h := hashing.New()
	_, _ = h.Write(cipher)
	if m.Content != h.Sum().String() {
		t.Fatalf("content id %s != blake3 of ciphertext %s", m.Content, h.Sum().String())
	}
	if bytes.Contains(cipher, pattern(vaultframe.FrameSize + 7)[:64]) {
		t.Fatal("plaintext appears in the ciphertext")
	}
}

// TestOpenRangeFetchesOnlyCoveringFrames: a range straddling a frame boundary
// decrypts correctly while fetching only the frames it covers.
func TestOpenRangeFetchesOnlyCoveringFrames(t *testing.T) {
	sk := mustKey(t)
	plaintext := pattern(3*vaultframe.FrameSize + 500)
	cipher, m := sealToBytes(t, sk, plaintext)

	// Map each frame's ciphertext start offset to its index, using only the
	// exported FrameByteRange, so the fetch hook can tell which frame it served.
	startToIndex := map[int64]int{}
	for i := 0; i < m.FrameCount; i++ {
		s, _ := m.FrameByteRange(i)
		startToIndex[s] = i
	}

	// A range from late in frame 1 into early frame 2 — covers frames 1 and 2 only.
	off := int64(vaultframe.FrameSize) + 1000
	n := int64(vaultframe.FrameSize) // spans the 1→2 boundary
	fetched := map[int]bool{}
	got, err := m.OpenRange(sk, off, n, func(start, end int64) ([]byte, error) {
		fetched[startToIndex[start]] = true
		return cipher[start:end], nil
	})
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	if !bytes.Equal(got, plaintext[off:off+n]) {
		t.Fatal("OpenRange returned the wrong bytes")
	}
	if fetched[0] || fetched[3] || !fetched[1] || !fetched[2] {
		t.Fatalf("expected to fetch only frames 1 and 2, fetched %v", fetched)
	}
}

// TestReorderedFrameRejected: opening a frame's slot with another frame's
// ciphertext (same object) fails the index check.
func TestReorderedFrameRejected(t *testing.T) {
	sk := mustKey(t)
	cipher, m := sealToBytes(t, sk, pattern(2*vaultframe.FrameSize))
	s0, e0 := m.FrameByteRange(0)
	// Try to pass frame 0's bytes as if they were frame 1.
	if _, err := m.OpenFrame(sk, 1, cipher[s0:e0]); err == nil {
		t.Fatal("a frame opened at the wrong index must be rejected")
	}
}

// TestSplicedFrameRejected: a frame from another object (different file id) is
// rejected even though it decrypts under the same key.
func TestSplicedFrameRejected(t *testing.T) {
	sk := mustKey(t)
	_, m1 := sealToBytes(t, sk, pattern(vaultframe.FrameSize))
	cipher2, _ := sealToBytes(t, sk, pattern(vaultframe.FrameSize))
	s, e := m1.FrameByteRange(0)
	// m1 expects its own file id; cipher2's frame 0 carries a different one.
	if _, err := m1.OpenFrame(sk, 0, cipher2[s:e]); err == nil {
		t.Fatal("a frame spliced from another object must be rejected")
	}
}

// TestTamperedFrameRejected: flipping a ciphertext byte fails the AEAD.
func TestTamperedFrameRejected(t *testing.T) {
	sk := mustKey(t)
	cipher, m := sealToBytes(t, sk, pattern(vaultframe.FrameSize))
	tampered := append([]byte(nil), cipher...)
	tampered[len(tampered)/2] ^= 0xFF
	if _, err := m.OpenFrame(sk, 0, tampered); err == nil {
		t.Fatal("a tampered frame must be rejected")
	}
}

// TestManifestSealRoundTrip: a sealed manifest reverses, and a different key
// cannot open it.
func TestManifestSealRoundTrip(t *testing.T) {
	sk := mustKey(t)
	_, m := sealToBytes(t, sk, pattern(vaultframe.FrameSize+1))
	sealed, err := vaultframe.SealManifest(sk, m)
	if err != nil {
		t.Fatal(err)
	}
	back, err := vaultframe.OpenManifest(sk, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if back != m {
		t.Fatalf("manifest round-trip changed it: %+v vs %+v", back, m)
	}
	if _, err := vaultframe.OpenManifest(mustKey(t), sealed); err == nil {
		t.Fatal("a different key must not open the manifest")
	}
}

// TestEmptyFile: a zero-byte file is zero frames.
func TestEmptyFile(t *testing.T) {
	sk := mustKey(t)
	cipher, m := sealToBytes(t, sk, nil)
	if m.FrameCount != 0 || m.PlaintextSize != 0 || len(cipher) != 0 {
		t.Fatalf("empty file: frames=%d size=%d cipher=%d", m.FrameCount, m.PlaintextSize, len(cipher))
	}
	got, err := m.OpenRange(sk, 0, 0, func(int64, int64) ([]byte, error) { return nil, nil })
	if err != nil || len(got) != 0 {
		t.Fatalf("empty range read: %v, %d bytes", err, len(got))
	}
}

// TestOpenManifestRefusesImpossibleGeometry (#712): a manifest that opens under
// the right key but could not have come from Seal is ErrManifest, never a
// geometry a reader would size buffers or frame ranges from. Every case is a
// manifest the Kotlin client's VaultFrame.validateManifest also refuses.
func TestOpenManifestRefusesImpossibleGeometry(t *testing.T) {
	t.Parallel()
	sk := mustKey(t)
	_, good := sealToBytes(t, sk, pattern(vaultframe.FrameSize+5)) // two frames, the last short
	_, empty := sealToBytes(t, sk, nil)
	for _, m := range []vaultframe.Manifest{good, empty} {
		sealed, err := vaultframe.SealManifest(sk, m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := vaultframe.OpenManifest(sk, sealed); err != nil {
			t.Fatalf("a manifest Seal wrote is refused: %v (%+v)", err, m)
		}
	}

	cases := map[string]func(m *vaultframe.Manifest){
		"another version":           func(m *vaultframe.Manifest) { m.Version = 2 },
		"a short file id":           func(m *vaultframe.Manifest) { m.FileID = m.FileID[:30] },
		"an upper-case file id":     func(m *vaultframe.Manifest) { m.FileID = "ABCDEF" + m.FileID[6:] },
		"a non-hex file id":         func(m *vaultframe.Manifest) { m.FileID = "zz" + m.FileID[2:] },
		"a content id of no scheme": func(m *vaultframe.Manifest) { m.Content = m.Content[len("blake3:"):] },
		"a short content id":        func(m *vaultframe.Manifest) { m.Content = m.Content[:20] },
		"a zero frame size":         func(m *vaultframe.Manifest) { m.FrameSize = 0 },
		"a frame size over 1 MiB":   func(m *vaultframe.Manifest) { m.FrameSize = vaultframe.FrameSize + 1 },
		"a negative plaintext size": func(m *vaultframe.Manifest) { m.PlaintextSize = -1 },
		"too few frames":            func(m *vaultframe.Manifest) { m.FrameCount = 1 },
		"too many frames":           func(m *vaultframe.Manifest) { m.FrameCount = 3 },
		"a negative frame count":    func(m *vaultframe.Manifest) { m.FrameCount = -1 },
		"frames for an empty file":  func(m *vaultframe.Manifest) { m.PlaintextSize = 0 },
	}
	for name, mutate := range cases {
		m := good
		mutate(&m)
		sealed, err := vaultframe.SealManifest(sk, m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := vaultframe.OpenManifest(sk, sealed); !errors.Is(err, vaultframe.ErrManifest) {
			t.Errorf("%s: OpenManifest = %v, want ErrManifest", name, err)
		}
	}

	notJSON, err := encryption.EncryptChange(sk, []byte("not a manifest"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vaultframe.OpenManifest(sk, notJSON); !errors.Is(err, vaultframe.ErrManifest) {
		t.Errorf("undecodable manifest: OpenManifest = %v, want ErrManifest", err)
	}
	// A wrong key is still the library's decrypt refusal, so a keyring moves on.
	sealed, _ := vaultframe.SealManifest(sk, good)
	if _, err := vaultframe.OpenManifest(mustKey(t), sealed); !errors.Is(err, encryption.ErrDecrypt) {
		t.Errorf("wrong key: OpenManifest = %v, want encryption.ErrDecrypt", err)
	}
}
