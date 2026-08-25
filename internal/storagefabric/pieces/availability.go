package pieces

import (
	"fmt"
	"strings"
)

// Availability is which pieces a peer holds.
//
// # Why this exists at all
//
// It is the difference between a pull and a swarm. Replication has always asked
// "do you have this blob" and got a yes or a no, because a peer either held the
// bytes or did not. §23 needs a third answer — "I have some of it, here is
// which" — so that two peers who are both still fetching can serve each other
// what they already have.
//
// # A bitset, and the reason is the message size
//
// One bit per piece. At the geometry's target of ~1024 pieces that is 128
// bytes, which is small enough to send with every inventory exchange and never
// think about again. A list of indices would be smaller when a peer holds
// almost nothing and far larger when it holds almost everything, and the
// almost-everything case is the one that happens constantly as a transfer
// converges.
//
// # It is a CLAIM, not evidence
//
// A peer saying it has a piece is a peer saying so. Nothing here is trusted:
// every piece a destination receives is verified against its own hash before it
// counts, and the whole object is verified against its BLAKE3 digest before it
// becomes a blob (invariant 1). A lying peer wastes a request and is walked
// past, exactly as ADR-0036 has repair treat one.
type Availability struct {
	bits  []byte
	count int
	total int
}

// NewAvailability returns an empty availability for a blob of n pieces.
func NewAvailability(n int) Availability {
	if n <= 0 {
		return Availability{}
	}
	return Availability{bits: make([]byte, (n+7)/8), total: n}
}

// Total is how many pieces the blob has.
func (a Availability) Total() int { return a.total }

// Count is how many pieces are held.
func (a Availability) Count() int { return a.count }

// Has reports whether one piece is held.
func (a Availability) Has(index int) bool {
	if index < 0 || index >= a.total {
		return false
	}
	return a.bits[index/8]&(1<<(index%8)) != 0
}

// Add records that a piece is held. Adding one twice is not an error and does
// not double-count — a handler that re-verifies a piece it already had must be
// safe to re-run (invariant 9).
func (a *Availability) Add(index int) {
	if index < 0 || index >= a.total || a.Has(index) {
		return
	}
	a.bits[index/8] |= 1 << (index % 8)
	a.count++
}

// Missing is every piece not held, in ascending order.
//
// Ascending rather than rarest-first or random: ADR-0042 says plainly that
// strategy is not in scope for a fabric of this size, and a deterministic order
// is what lets a test assert which piece a session asked for. When strategy
// arrives it replaces this function and nothing else.
func (a Availability) Missing() []int {
	out := make([]int, 0, a.total-a.count)
	for i := range a.total {
		if !a.Has(i) {
			out = append(out, i)
		}
	}
	return out
}

// Intersect is the pieces this availability holds that another does not — what
// this peer could usefully serve that one.
//
// The direction is deliberate and easy to get backwards: the receiver is the
// SOURCE and the argument is the DESTINATION.
func (a Availability) Intersect(want Availability) []int {
	out := make([]int, 0, a.count)
	for i := range a.total {
		if a.Has(i) && !want.Has(i) {
			out = append(out, i)
		}
	}
	return out
}

// Encode renders availability for the wire as `<blob size>:<hex bitset>`.
//
// # Why the SIZE and not the piece count
//
// The first version carried the count, because a bitset's width alone cannot
// tell 20 pieces from 21 — both are three bytes. That fixed the rounding
// ambiguity and left a worse one: **the count does not determine the piece
// length.** 1024 pieces is a 256 MiB blob at 256 KiB pieces, and equally a
// 8 GiB blob at 8 MiB pieces. A node serving from a partial — which does not
// know the blob's size, because a partial's length is a high-water mark
// (ADR-0043) — could not work out where a piece starts.
//
// The size determines everything: For derives the piece length and the count
// from it, deterministically, which is the property two peers already rely on
// to divide a blob identically without negotiating. So the size goes on the
// wire and the count is derived rather than claimed — which also makes the
// width check exact instead of approximate.
//
// Hex for the bitset because it is greppable in a log and in a test failure,
// and 128 bytes becomes 256 characters, which is not worth optimising.
func Encode(g Geometry, a Availability) string {
	if g.Size <= 0 || a.total == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d:", g.Size)
	for _, x := range a.bits {
		fmt.Fprintf(&b, "%02x", x)
	}
	return b.String()
}

// Decode parses what Encode produced, returning the geometry it describes and
// the availability within it.
//
// The geometry is DERIVED from the size rather than read from the wire, so a
// peer cannot describe a division this node would not compute — it can only
// describe a different blob size, which is caught here.
func Decode(s string) (Geometry, Availability, error) {
	sizeText, hex, ok := strings.Cut(s, ":")
	if !ok {
		return Geometry{}, Availability{}, fmt.Errorf(
			"pieces: availability %q does not say what size blob it describes", s)
	}
	var size int64
	if _, err := fmt.Sscanf(sizeText, "%d", &size); err != nil {
		return Geometry{}, Availability{}, fmt.Errorf(
			"pieces: availability size is not a number: %w", err)
	}
	g, err := For(size)
	if err != nil {
		return Geometry{}, Availability{}, err
	}

	a := NewAvailability(g.Count())
	if want := len(a.bits) * 2; len(hex) != want {
		return Geometry{}, Availability{}, fmt.Errorf(
			"pieces: availability is %d hex characters for a %d byte blob, want %d — "+
				"the peer is describing a different geometry", len(hex), size, want)
	}
	for i := range a.bits {
		var v byte
		if _, err := fmt.Sscanf(hex[i*2:i*2+2], "%02x", &v); err != nil {
			return Geometry{}, Availability{}, fmt.Errorf("pieces: availability is not hex: %w", err)
		}
		a.bits[i] = v
	}
	// Recount rather than trust: a byte with bits set past the last piece would
	// otherwise make a peer look complete when it is not.
	for i := range g.Count() {
		if a.bits[i/8]&(1<<(i%8)) != 0 {
			a.count++
		}
	}
	return g, a, nil
}

// Remove clears a piece.
//
// Used by a transfer to forget that a source CLAIMED a piece — after that
// source failed to serve it, or served it at the wrong length. It is not used
// to un-record a piece this node holds: bytes on disk are not un-written by
// changing a bitset, and a piece removed from the record is simply refetched.
func (a *Availability) Remove(i int) {
	if i < 0 || i >= a.total || !a.Has(i) {
		return
	}
	a.bits[i/8] &^= 1 << (i % 8)
	a.count--
}
