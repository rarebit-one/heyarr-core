package hashing

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Known-answer tests against published BLAKE3 vectors. If the underlying
// library is ever swapped, these are what say whether the new one computes the
// same identities — which matters more than usual here, because a change would
// silently re-identify every blob in every existing deployment.
func TestKnownAnswers(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262"},
		{"abc", "abc", "6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, n, err := HashReader(strings.NewReader(tt.input))
			if err != nil {
				t.Fatal(err)
			}
			if got.Hex() != tt.want {
				t.Errorf("BLAKE3(%q) = %s, want %s", tt.input, got.Hex(), tt.want)
			}
			if n != int64(len(tt.input)) {
				t.Errorf("length = %d, want %d", n, len(tt.input))
			}
		})
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	valid := strings.Repeat("ab", 32)
	tests := map[string]string{
		"empty":               "",
		"no prefix":           valid,
		"wrong algorithm":     "sha256:" + valid,
		"blake2":              "blake2:" + valid,
		"too short":           "blake3:" + valid[:62],
		"too long":            "blake3:" + valid + "cd",
		"uppercase":           "blake3:" + strings.ToUpper(valid),
		"mixed case":          "blake3:Ab" + valid[2:],
		"non-hex":             "blake3:" + strings.Repeat("zz", 32),
		"prefix only":         "blake3:",
		"colon only":          ":",
		"whitespace padded":   " blake3:" + valid,
		"double prefix":       "blake3:blake3:" + valid,
		"trailing whitespace": "blake3:" + valid + " ",
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(input); err == nil {
				t.Errorf("Parse(%q) succeeded", input)
			} else if !errors.Is(err, ErrInvalidHash) {
				t.Errorf("Parse(%q) returned %v, want it to wrap ErrInvalidHash", input, err)
			}
		})
	}
}

// Uppercase is rejected rather than normalised: two spellings of one identity
// means two catalog rows and two CAS paths for the same bytes, which quietly
// breaks the deduplication guarantee.
func TestParseRejectsUppercaseRatherThanNormalising(t *testing.T) {
	lower := "blake3:" + strings.Repeat("ab", 32)
	upper := "blake3:" + strings.ToUpper(strings.Repeat("ab", 32))

	if _, err := Parse(lower); err != nil {
		t.Fatalf("lowercase rejected: %v", err)
	}
	if h, err := Parse(upper); err == nil {
		t.Errorf("uppercase accepted and normalised to %s — one identity must have one spelling", h)
	}
}

func TestParseRoundTrip(t *testing.T) {
	h, _, err := HashReader(strings.NewReader("round trip"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := Parse(h.String())
	if err != nil {
		t.Fatalf("a hash we produced did not parse: %v", err)
	}
	if !h.Equal(again) {
		t.Errorf("round trip changed the digest: %s then %s", h, again)
	}
}

func TestTextMarshalling(t *testing.T) {
	h, _, err := HashReader(strings.NewReader("marshal"))
	if err != nil {
		t.Fatal(err)
	}
	text, err := h.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	var got Hash
	if err := got.UnmarshalText(text); err != nil {
		t.Fatal(err)
	}
	if !got.Equal(h) {
		t.Errorf("marshal round trip = %s, want %s", got, h)
	}
	if err := got.UnmarshalText([]byte("not a hash")); err == nil {
		t.Error("UnmarshalText accepted a malformed hash")
	}
}

func TestZeroValueIsNotAnIdentity(t *testing.T) {
	var h Hash
	if !h.IsZero() {
		t.Error("the zero Hash does not report itself as zero")
	}
	real, _, err := HashReader(strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	if real.IsZero() {
		t.Error("a real digest reported itself as zero")
	}
}

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blob.bin")
	content := bytes.Repeat([]byte("heyarr"), 100_000) // ~600 KB, spans several buffers
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	fromFile, n, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(content)) {
		t.Errorf("size = %d, want %d", n, len(content))
	}
	fromMemory, _, err := HashReader(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if !fromFile.Equal(fromMemory) {
		t.Errorf("file and reader disagree: %s vs %s", fromFile, fromMemory)
	}
}

func TestHashFileReportsMissingPaths(t *testing.T) {
	_, _, err := HashFile(filepath.Join(t.TempDir(), "nope"))
	if err == nil {
		t.Fatal("HashFile succeeded for a missing file")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error = %q, want it to name the path", err)
	}
}

// Streaming must be independent of how the input is chunked — otherwise the
// identity of a blob would depend on the network's packet boundaries.
func TestDigestIsIndependentOfChunking(t *testing.T) {
	content := bytes.Repeat([]byte("abcdefghij"), 300_000) // ~3 MB
	whole, _, err := HashReader(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}

	for _, chunk := range []int{1, 7, 4096, bufferSize, bufferSize + 1, len(content)} {
		h := New()
		for off := 0; off < len(content); off += chunk {
			end := min(off+chunk, len(content))
			if _, err := h.Write(content[off:end]); err != nil {
				t.Fatal(err)
			}
		}
		if got := h.Sum(); !got.Equal(whole) {
			t.Errorf("chunk size %d produced %s, want %s", chunk, got, whole)
		}
	}
}

func TestVerifyAcceptsMatchingContent(t *testing.T) {
	content := []byte("verify me")
	want, _, err := HashReader(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	n, err := Verify(bytes.NewReader(content), want)
	if err != nil {
		t.Fatalf("Verify rejected matching content: %v", err)
	}
	if n != int64(len(content)) {
		t.Errorf("Verify reported %d bytes, want %d", n, len(content))
	}
}

// Corruption and I/O failure must be distinguishable. Corruption means
// quarantine the blob (ADR-0018); an I/O error means retry. Treating them the
// same is how a flaky disk gets a healthy replica deleted.
func TestVerifyDistinguishesCorruptionFromIOFailure(t *testing.T) {
	want, _, err := HashReader(strings.NewReader("expected"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("corruption", func(t *testing.T) {
		_, err := Verify(strings.NewReader("different"), want)
		var mismatch *ErrMismatch
		if !errors.As(err, &mismatch) {
			t.Fatalf("Verify returned %T (%v), want *ErrMismatch", err, err)
		}
		// The message must carry both digests; "corrupt" alone is not actionable.
		if !strings.Contains(mismatch.Error(), want.String()) {
			t.Errorf("error %q does not name the expected digest", mismatch)
		}
		if mismatch.Got.Equal(want) {
			t.Error("the mismatch reports the same digest for both sides")
		}
	})

	t.Run("io failure", func(t *testing.T) {
		sentinel := errors.New("disk fell over")
		_, err := Verify(&failingReader{err: sentinel}, want)
		var mismatch *ErrMismatch
		if errors.As(err, &mismatch) {
			t.Fatal("an I/O failure was reported as content corruption")
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("error = %v, want it to wrap the I/O error", err)
		}
	})
}

func TestVerifyingReader(t *testing.T) {
	content := bytes.Repeat([]byte("stream"), 50_000)
	want, _, err := HashReader(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("matching content passes", func(t *testing.T) {
		vr := NewVerifyingReader(bytes.NewReader(content), want)
		n, err := io.Copy(io.Discard, vr)
		if err != nil {
			t.Fatal(err)
		}
		if err := vr.Check(); err != nil {
			t.Errorf("Check rejected matching content: %v", err)
		}
		if vr.Size() != n {
			t.Errorf("Size = %d, want %d", vr.Size(), n)
		}
	})

	t.Run("altered content fails", func(t *testing.T) {
		altered := bytes.Clone(content)
		altered[len(altered)/2] ^= 0xFF // one flipped bit in the middle
		vr := NewVerifyingReader(bytes.NewReader(altered), want)
		if _, err := io.Copy(io.Discard, vr); err != nil {
			t.Fatal(err)
		}
		var mismatch *ErrMismatch
		if !errors.As(vr.Check(), &mismatch) {
			t.Error("a single flipped bit was not detected")
		}
	})

	// A caller that stops reading early has verified nothing. Reporting success
	// would be worse than failing, because the caller would act on it.
	t.Run("a partial read is not a pass", func(t *testing.T) {
		vr := NewVerifyingReader(bytes.NewReader(content), want)
		if _, err := io.CopyN(io.Discard, vr, 10); err != nil {
			t.Fatal(err)
		}
		err := vr.Check()
		if err == nil {
			t.Fatal("Check passed after reading 10 bytes of a 300 KB blob")
		}
		if !strings.Contains(err.Error(), "exhausted") {
			t.Errorf("error = %q, want it to explain that the reader was not consumed", err)
		}
	})
}

type failingReader struct{ err error }

func (f *failingReader) Read([]byte) (int, error) { return 0, f.err }

// The throughput number is recorded in ADR-0005; this is the measurement that
// keeps it honest and catches a regression from swapping the library.
func BenchmarkHashThroughput(b *testing.B) {
	content := bytes.Repeat([]byte("heyarr benchmark payload "), 4<<20/25) // ~4 MiB
	b.SetBytes(int64(len(content)))
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := HashReader(bytes.NewReader(content)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHashFile(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 16<<20), 0o600); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(16 << 20)
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := HashFile(path); err != nil {
			b.Fatal(err)
		}
	}
}
