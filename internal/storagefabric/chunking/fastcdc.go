package chunking

import (
	"errors"
	"fmt"
	"io"
	"math/bits"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// Default chunk sizes.
//
// These are chosen, not copied. The quantity an operator actually meets is the
// number of manifest rows behind a large file, and Heyarr's normal large input
// is a 20 GB remux (ADR-0013):
//
//	target average    manifest rows for a 20 GB remux
//	      256 KiB                     ~72,000
//	        1 MiB                     ~18,000
//	        4 MiB                      ~4,500
//
// A 1 MiB target puts a 20 GB remux at roughly eighteen thousand rows: small
// enough that a manifest is an ordinary table read and a diff between two of
// them is cheap, large enough that per-chunk overhead — a row, a 32-byte
// digest, a range request — stays a rounding error against the bytes it
// describes. A smaller average buys finer reuse at a cost paid on every blob,
// including the many that are written once and never modified; a larger one
// makes a resumed or repaired transfer re-fetch too much to be worth resuming.
//
// Min and Max are the conventional quarter and quadruple of the target. The
// minimum stops a pathological input from producing a manifest of tiny rows;
// the maximum bounds both the chunker's buffer and the worst-case re-fetch, and
// is the only thing standing between highly repetitive data and one enormous
// chunk.
//
// The target is a mask, not a promise. Measured over pseudo-random data these
// parameters produce a mean chunk of about 1.09 x Avg — 1.14 MiB — which is the
// figure the row counts above are computed from. The bounds test logs the
// measured mean on every run, so drift shows up rather than being assumed away.
const (
	DefaultMin = 256 << 10 // 256 KiB
	DefaultAvg = 1 << 20   // 1 MiB
	DefaultMax = 4 << 20   // 4 MiB
)

// normalization is the FastCDC normalised-chunking level: before the target
// average the cut mask is this many bits harder to satisfy, and after it this
// many bits easier. It pulls the chunk-size distribution in towards the target
// instead of leaving it the long exponential tail plain Gear chunking produces,
// which is what makes the [Min, Max] bounds bite rarely rather than constantly:
// over 24 MiB of random data at the defaults, no chunk reaches Max at all.
const normalization = 2

// readSize is how much the chunker pulls from the reader at a time. The
// chunker's whole memory footprint is Max + readSize regardless of how large
// the input is.
const readSize = 256 << 10

// ErrInvalidConfig is returned for chunk sizes that cannot describe a chunker.
var ErrInvalidConfig = errors.New("chunking: invalid config")

// Config is the chunk-size parameter table. All three are in bytes.
type Config struct {
	Min int
	Avg int
	Max int
}

// DefaultConfig returns the parameters documented above.
func DefaultConfig() Config {
	return Config{Min: DefaultMin, Avg: DefaultAvg, Max: DefaultMax}
}

// Validate reports whether the parameters describe a usable chunker.
//
// Avg must be a power of two because the cut masks are derived from its
// logarithm; requiring it is better than silently rounding, which would mean
// two peers configured 1000000 and 1048576 agreed they were both "1 MB" and
// then computed different manifests.
func (c Config) Validate() error {
	if c.Min <= 0 {
		return fmt.Errorf("%w: Min is %d, want a positive size", ErrInvalidConfig, c.Min)
	}
	if c.Min >= c.Avg {
		return fmt.Errorf("%w: Min (%d) must be smaller than Avg (%d)", ErrInvalidConfig, c.Min, c.Avg)
	}
	if c.Avg >= c.Max {
		return fmt.Errorf("%w: Avg (%d) must be smaller than Max (%d)", ErrInvalidConfig, c.Avg, c.Max)
	}
	if c.Avg&(c.Avg-1) != 0 {
		return fmt.Errorf("%w: Avg (%d) must be a power of two", ErrInvalidConfig, c.Avg)
	}
	avgBits := bits.TrailingZeros(uint(c.Avg))
	if avgBits <= normalization || avgBits+normalization > 63 {
		return fmt.Errorf("%w: Avg (%d) is %d bits, outside the workable range for normalisation level %d",
			ErrInvalidConfig, c.Avg, avgBits, normalization)
	}
	return nil
}

// Chunk is one content-defined chunk: where it starts, how long it is, and the
// digest of its own bytes. Nothing else. Not the blob's hash, not a path, not
// an index into a table — a chunk that carried any of those would be an
// identity, and identity is the whole-blob digest (ADR-0005).
type Chunk struct {
	Offset int64
	Length int64
	Digest hashing.Hash
}

// End returns the offset one past this chunk's last byte, which is the offset
// the next chunk must start at.
func (c Chunk) End() int64 { return c.Offset + c.Length }

// Chunker splits a reader into content-defined chunks.
//
// It is streaming: memory is Max + readSize whatever the input size, because a
// 20 GB blob is a normal case here and a chunker that buffers the file is not
// usable on the only inputs that need it.
type Chunker struct {
	r     io.Reader
	cfg   Config
	maskS uint64
	maskL uint64

	buf []byte // backing store, len == Max + readSize, never reallocated
	off int    // start of the unconsumed bytes within buf
	n   int    // number of unconsumed bytes

	offset int64 // absolute offset of buf[off] in the stream
	eof    bool
	err    error
}

// New returns a Chunker reading from r.
func New(r io.Reader, cfg Config) (*Chunker, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	avgBits := bits.TrailingZeros(uint(cfg.Avg))
	return &Chunker{
		r:     r,
		cfg:   cfg,
		maskS: maskOf(avgBits + normalization),
		maskL: maskOf(avgBits - normalization),
		buf:   make([]byte, cfg.Max+readSize),
	}, nil
}

// maskOf returns a mask with n bits set, placed at the top of the word.
//
// The high bits are deliberate. With the rolling update fp = (fp<<1) + gear[b],
// bit k of the fingerprint depends on the last k+1 bytes, so masking the low
// bits would decide a boundary from a one- or two-byte window — content-defined
// in name only, and hopeless on repetitive data. Masking the top n bits means
// every bit tested depends on at least 64-n bytes of history.
func maskOf(n int) uint64 {
	return ^uint64(0) << (64 - n)
}

// Next returns the next chunk, or io.EOF when the stream is exhausted. A read
// error from the underlying reader is returned as-is and is sticky.
func (c *Chunker) Next() (Chunk, error) {
	if c.err != nil {
		return Chunk{}, c.err
	}
	if err := c.fill(); err != nil {
		c.err = err
		return Chunk{}, err
	}
	if c.n == 0 {
		c.err = io.EOF
		return Chunk{}, io.EOF
	}

	length := c.cut(c.buf[c.off : c.off+c.n])

	h := hashing.New()
	_, _ = h.Write(c.buf[c.off : c.off+length])

	chunk := Chunk{Offset: c.offset, Length: int64(length), Digest: h.Sum()}
	c.off += length
	c.n -= length
	c.offset += int64(length)
	return chunk, nil
}

// fill tops the buffer up to its full length, compacting first when the tail
// no longer has room. The buffer is allocated once in New and never grows, so
// this is where "bounded memory, independent of input size" is actually
// enforced.
func (c *Chunker) fill() error {
	if c.eof || c.n == len(c.buf) {
		return nil
	}
	if c.off > 0 {
		copy(c.buf, c.buf[c.off:c.off+c.n])
		c.off = 0
	}
	for c.n < len(c.buf) {
		read, err := c.r.Read(c.buf[c.n:])
		c.n += read
		if errors.Is(err, io.EOF) {
			c.eof = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("chunking: reading: %w", err)
		}
		if read == 0 {
			// A reader that returns (0, nil) forever would spin here.
			return errors.New("chunking: reader made no progress")
		}
	}
	return nil
}

// cut returns the length of the first chunk in src, which must be non-empty.
//
// This is FastCDC with normalised chunking: no boundary is considered before
// Min, the harder mask applies until the target average, the easier mask
// applies after it, and Max is a hard stop. The fingerprint deliberately does
// not consume the first Min bytes — they can never produce a boundary, so
// hashing them would be pure cost.
func (c *Chunker) cut(src []byte) int {
	n := len(src)
	if n <= c.cfg.Min {
		return n
	}
	if n > c.cfg.Max {
		n = c.cfg.Max
	}
	normal := c.cfg.Avg
	if n < normal {
		normal = n
	}

	var fp uint64
	i := c.cfg.Min
	for ; i < normal; i++ {
		fp = (fp << 1) + gear[src[i]]
		if fp&c.maskS == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		fp = (fp << 1) + gear[src[i]]
		if fp&c.maskL == 0 {
			return i + 1
		}
	}
	return n
}
