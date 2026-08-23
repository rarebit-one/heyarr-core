package chunking

import (
	"encoding/binary"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// gearSeed is the domain separator the gear table is derived from. It is part
// of the on-the-wire contract, not an implementation detail: two peers that
// derived their gear tables from different seeds would cut the same bytes into
// different chunks, compute different manifests for one blob, and every reuse
// decision between them would silently be wrong. Changing it is a format
// break, not a refactor.
const gearSeed = "heyarr/fastcdc/gear/v1"

// gearDigest pins the finished table as data.
//
// The table is derived rather than written out as 256 literals so that the
// derivation is auditable in four lines instead of trusting a wall of copied
// magic numbers — but a derivation is only as pinned as its test, so
// TestGearTableIsPinned asserts the digest of the serialised table equals this
// constant. Derivation plus a pinned digest is the same guarantee as a literal
// table, and it fails loudly if anyone edits the seed.
const gearDigest = "blake3:e2b123e6831e0cc48cb1e771611500ceb4b7a4e11b48960fdb4f32bfaabdce3f"

// gear is the FastCDC gear table: one random 64-bit value per byte value.
//
// It is derived from BLAKE3 over gearSeed with an explicit little-endian
// decoding, so it is identical on every platform Heyarr builds for. No
// platform-dependent arithmetic, no unsafe reinterpretation of memory, no
// dependence on int width — those are exactly the ways two peers end up with
// different tables and nobody notices until a transfer deduplicates nothing.
var gear = buildGear()

func buildGear() [256]uint64 {
	var table [256]uint64
	var counter [4]byte
	// Each BLAKE3 digest is 32 bytes, which is four uint64 entries.
	for i := 0; i < len(table); i += 4 {
		h := hashing.New()
		_, _ = h.Write([]byte(gearSeed))
		binary.LittleEndian.PutUint32(counter[:], uint32(i)) //nolint:gosec // i is bounded by len(table)
		_, _ = h.Write(counter[:])
		digest := h.Sum().Bytes()
		for j := range 4 {
			table[i+j] = binary.LittleEndian.Uint64(digest[j*8 : j*8+8])
		}
	}
	return table
}

// gearBytes serialises the table in canonical little-endian order so a test can
// pin it by digest.
func gearBytes() []byte {
	out := make([]byte, 0, len(gear)*8)
	var buf [8]byte
	for _, v := range gear {
		binary.LittleEndian.PutUint64(buf[:], v)
		out = append(out, buf[:]...)
	}
	return out
}
