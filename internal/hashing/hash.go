// Package hashing computes and verifies BLAKE3 whole-object digests, the
// canonical byte identity (spec §13).
//
// A Blob's identity is the digest of its entire byte sequence and nothing else
// (ADR-0005). Chunk manifests arrived in Milestone 5 and are an optimisation for
// transfer and deduplication — a manifest is stored UNDER the whole-object
// digest and never becomes one (ADR-0034).
package hashing

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/zeebo/blake3"
)

// Algorithm is the only hash algorithm Heyarr uses for blob identity.
const Algorithm = "blake3"

// DigestLen is the digest length in bytes; HexLen is its hex encoding.
const (
	DigestLen = 32
	HexLen    = DigestLen * 2
)

// bufferSize is the streaming read size. Large enough that syscall overhead is
// negligible against disk throughput, small enough that hashing a 60 GB remux
// costs a megabyte of memory rather than scaling with the file.
const bufferSize = 1 << 20 // 1 MiB

// ErrInvalidHash is returned when a string is not a well-formed blob hash.
var ErrInvalidHash = errors.New("hashing: invalid hash")

// Hash is a canonical BLAKE3 blob identity, formatted "blake3:<64 lowercase hex>".
//
// It is a value type rather than a string alias so that a malformed digest
// cannot reach the catalog by being passed where an identity is expected: the
// only way to construct one is Parse or a hashing function.
type Hash struct {
	digest [DigestLen]byte
}

// Parse validates and decodes a hash in canonical form.
//
// Uppercase hex is rejected rather than normalised. Two spellings of one
// identity would mean two rows for one blob, two CAS paths, and a
// deduplication guarantee that quietly does not hold.
func Parse(s string) (Hash, error) {
	algo, hexDigest, ok := strings.Cut(s, ":")
	if !ok {
		return Hash{}, fmt.Errorf("%w: %q has no algorithm prefix, want %q", ErrInvalidHash, s, Algorithm+":<hex>")
	}
	if algo != Algorithm {
		return Hash{}, fmt.Errorf("%w: algorithm %q is not supported, want %q", ErrInvalidHash, algo, Algorithm)
	}
	if len(hexDigest) != HexLen {
		return Hash{}, fmt.Errorf("%w: digest is %d hex characters, want %d", ErrInvalidHash, len(hexDigest), HexLen)
	}
	if strings.ToLower(hexDigest) != hexDigest {
		return Hash{}, fmt.Errorf("%w: digest must be lowercase hex, got %q", ErrInvalidHash, hexDigest)
	}
	var h Hash
	if _, err := hex.Decode(h.digest[:], []byte(hexDigest)); err != nil {
		return Hash{}, fmt.Errorf("%w: %q is not hex: %w", ErrInvalidHash, hexDigest, err)
	}
	return h, nil
}

// MustParse is Parse for compile-time constants and tests.
func MustParse(s string) Hash {
	h, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return h
}

// String returns the canonical form.
func (h Hash) String() string {
	return Algorithm + ":" + hex.EncodeToString(h.digest[:])
}

// Hex returns the digest without the algorithm prefix. The CAS uses it to
// derive its directory fanout.
func (h Hash) Hex() string { return hex.EncodeToString(h.digest[:]) }

// Bytes returns a copy of the raw digest.
func (h Hash) Bytes() []byte {
	out := make([]byte, DigestLen)
	copy(out, h.digest[:])
	return out
}

// IsZero reports whether this is the zero value rather than a real digest.
func (h Hash) IsZero() bool { return h == Hash{} }

// Equal compares two hashes. Digests are public values, so this does not need
// to be constant-time — unlike token comparison (ADR-0011).
func (h Hash) Equal(other Hash) bool { return h == other }

// MarshalText implements encoding.TextMarshaler, so a Hash round-trips through
// JSON and database drivers in canonical form.
func (h Hash) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (h *Hash) UnmarshalText(text []byte) error {
	parsed, err := Parse(string(text))
	if err != nil {
		return err
	}
	*h = parsed
	return nil
}

// Hasher computes a BLAKE3 digest incrementally.
type Hasher struct {
	h *blake3.Hasher
	n int64
}

// New returns a Hasher.
func New() *Hasher { return &Hasher{h: blake3.New()} }

// Write adds bytes to the digest. It never returns an error.
func (h *Hasher) Write(p []byte) (int, error) {
	n, err := h.h.Write(p)
	h.n += int64(n)
	return n, err
}

// Sum returns the digest of everything written so far.
func (h *Hasher) Sum() Hash {
	var out Hash
	h.h.Sum(out.digest[:0])
	return out
}

// Size is the number of bytes written.
func (h *Hasher) Size() int64 { return h.n }

// HashReader consumes r entirely and returns its digest and length.
func HashReader(r io.Reader) (Hash, int64, error) {
	h := New()
	buf := make([]byte, bufferSize)
	if _, err := io.CopyBuffer(h, r, buf); err != nil {
		return Hash{}, 0, fmt.Errorf("hashing: reading: %w", err)
	}
	return h.Sum(), h.Size(), nil
}

// HashFile returns the digest and length of the file at path.
func HashFile(path string) (Hash, int64, error) {
	f, err := os.Open(path) // #nosec G304 -- callers pass paths from library roots, validated upstream
	if err != nil {
		return Hash{}, 0, fmt.Errorf("hashing: opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	h, n, err := HashReader(f)
	if err != nil {
		return Hash{}, 0, fmt.Errorf("hashing: %s: %w", path, err)
	}
	return h, n, nil
}

// ErrMismatch reports bytes that did not hash to what was expected. It carries
// both digests, because "corrupt" without them is not actionable.
type ErrMismatch struct {
	Want Hash
	Got  Hash
	Size int64
}

func (e *ErrMismatch) Error() string {
	return fmt.Sprintf("hashing: content mismatch: expected %s, got %s over %d bytes", e.Want, e.Got, e.Size)
}

// Verify reads r fully and reports whether it hashes to want.
//
// It returns *ErrMismatch on a mismatch, so callers can distinguish corruption
// — which means quarantine the blob (ADR-0018) — from an I/O failure, which
// means retry. Treating those the same is how a flaky disk gets a healthy
// replica deleted.
func Verify(r io.Reader, want Hash) (int64, error) {
	got, n, err := HashReader(r)
	if err != nil {
		return n, err
	}
	if !got.Equal(want) {
		return n, &ErrMismatch{Want: want, Got: got, Size: n}
	}
	return n, nil
}

// VerifyingReader wraps r and hashes bytes as they are read, so verification
// costs no second pass over the data.
//
// This is how a destination verifies a transfer without buffering it (§21) and
// how the CAS hashes on write. Check must be called after the reader is
// exhausted; a caller that stops early has verified nothing, and Check says so.
type VerifyingReader struct {
	r      io.Reader
	hasher *Hasher
	want   Hash
	eof    bool
}

// NewVerifyingReader wraps r, expecting its full contents to hash to want.
func NewVerifyingReader(r io.Reader, want Hash) *VerifyingReader {
	return &VerifyingReader{r: r, hasher: New(), want: want}
}

func (v *VerifyingReader) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	if n > 0 {
		_, _ = v.hasher.Write(p[:n])
	}
	if errors.Is(err, io.EOF) {
		v.eof = true
	}
	return n, err
}

// Check reports whether what was read matches. It returns an error if the
// reader was not consumed to EOF, because a partial read proves nothing and
// silently reporting success would be worse than failing.
func (v *VerifyingReader) Check() error {
	if !v.eof {
		return errors.New("hashing: verification attempted before the reader was exhausted")
	}
	got := v.hasher.Sum()
	if !got.Equal(v.want) {
		return &ErrMismatch{Want: v.want, Got: got, Size: v.hasher.Size()}
	}
	return nil
}

// Size is the number of bytes read so far.
func (v *VerifyingReader) Size() int64 { return v.hasher.Size() }
