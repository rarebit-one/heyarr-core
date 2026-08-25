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

// Encode renders availability for the wire as `<count>:<hex bitset>`.
//
// # Why the count is on the wire and not implied by the width
//
// It was implied by the width, and that is wrong by up to seven pieces: a
// bitset for 20 pieces and one for 21 are both three bytes, so a peer claiming
// a geometry this node did not compute slipped through whenever the difference
// rounded into the same byte. A test caught it.
//
// Rounding is exactly where an implied length fails, and the failure is silent
// — the two peers then request pieces whose boundaries they do not share, and
// the bytes fail verification for a reason nobody could diagnose. Saying the
// number is four characters and removes the class.
//
// Hex rather than base64 for the bitset because it is greppable in a log and in
// a test failure, and 128 bytes becomes 256 characters, which is not worth
// optimising.
func (a Availability) Encode() string {
	if a.total == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d:", a.total)
	for _, x := range a.bits {
		fmt.Fprintf(&b, "%02x", x)
	}
	return b.String()
}

// DecodeAvailability parses what Encode produced, for a blob of n pieces.
//
// It refuses a bitset of the wrong width rather than padding or truncating. A
// peer describing a different number of pieces than this node computed is a
// peer that disagrees about the GEOMETRY, and quietly accepting the overlap
// would mean requesting pieces whose boundaries the two do not share — which
// produces bytes that fail verification for a reason nobody could diagnose.
func DecodeAvailability(s string, n int) (Availability, error) {
	a := NewAvailability(n)
	if n <= 0 {
		return a, nil
	}
	total, hex, ok := strings.Cut(s, ":")
	if !ok {
		return Availability{}, fmt.Errorf(
			"pieces: availability %q does not say how many pieces it describes", s)
	}
	var claimed int
	if _, err := fmt.Sscanf(total, "%d", &claimed); err != nil {
		return Availability{}, fmt.Errorf("pieces: availability piece count is not a number: %w", err)
	}
	if claimed != n {
		return Availability{}, fmt.Errorf(
			"pieces: the peer describes %d pieces and this node computed %d — "+
				"the two are describing a different geometry", claimed, n)
	}
	if want := len(a.bits) * 2; len(hex) != want {
		return Availability{}, fmt.Errorf(
			"pieces: availability is %d hex characters for %d pieces, want %d — "+
				"the peer is describing a different geometry", len(hex), n, want)
	}
	for i := range a.bits {
		var v byte
		if _, err := fmt.Sscanf(hex[i*2:i*2+2], "%02x", &v); err != nil {
			return Availability{}, fmt.Errorf("pieces: availability is not hex: %w", err)
		}
		a.bits[i] = v
	}
	// Recount rather than trust: a byte with bits set past the last piece would
	// otherwise make a peer look complete when it is not.
	for i := range n {
		if a.bits[i/8]&(1<<(i%8)) != 0 {
			a.count++
		}
	}
	return a, nil
}
