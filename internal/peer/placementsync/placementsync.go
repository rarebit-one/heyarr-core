// Package placementsync is how a site learns which blobs a sibling site wants
// kept here too, and the wire shape it asks in (ADR-0114, #662).
//
// # The destination asks
//
// A device pins a vault blob at the site it talks to. Replication is a
// destination pull (ADR-0030), so a pin naming ANOTHER site is a fact the other
// site has to learn before its own planner can do anything about it. This
// package carries that fact the same way the bytes travel: the destination B
// asks the source A, over A's peer surface, "which of what you hold do you want
// me to keep?", records the answer as its own desire, and pulls. A never calls
// B. Between one-way-reachable sites the question and the fetch therefore
// succeed or fail together, rather than one of them always running the wrong
// way (ADR-0038).
//
// # A hash range, replaced whole
//
// An exchange covers one range of the hash space, and B replaces everything A
// offered it inside that range with the answer. A withdrawn placement is simply
// absent from the next answer; a lost or repeated exchange changes nothing; no
// side keeps session state. B sends a digest of what it already took, and A
// answers "unchanged" when the digests match, so a converged pair costs one small
// request per range per cycle.
//
// # No peer ids on the wire
//
// Peer ids are node-local (ADR-0012): A's id for B is not B's. So the request
// names nobody. A knows who is asking from the certificate (ADR-0033), and B
// knows whom it asked.
//
// # The asker's claim rides along
//
// `held` is B's complete claim about its OWN disk in the range, on B's own
// connection. A folds it into its `replicas` as an inventory,
// and marks any of B's present replicas in the range it omits as missing, which
// is what lets A say a blob is durable at two sites and stop saying so when it
// is not.
package placementsync

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rarebit-one/void-which-binds-go/hashing"

	"github.com/rarebit-one/heyarr-core/internal/peer/inventory"
)

// Path is where a source answers a sibling's question, under the peer surface's
// prefix. A constant so the asker and the route cannot disagree about it.
const Path = "/peer/v1/placements"

// hashPrefix is what every canonical digest starts with (ADR-0005). A range is
// stated over the hex that follows it.
const hashPrefix = hashing.Algorithm + ":"

// ErrInvalid is a request or answer this package will not put on the wire, and
// will not act on when it comes off one.
var ErrInvalid = errors.New("placementsync: invalid exchange")

// Entry is one blob a source offers: its canonical digest, its size, and
// whether the source holds its bytes right now.
//
// The size is a hint the destination records so a piecewise pull can plan
// before it has the bytes (a pieced transfer needs the length). It is never
// trusted as a fact about the bytes: the pull verifies the digest of whatever
// arrives (§21, invariant 1), and a wrong size fails that transfer.
//
// Held is separate from being offered, and that is load-bearing: a source that
// lost or quarantined its copy still WANTS the blob kept, and is exactly when
// the destination's copy may be the only good one. Dropping the entry would read
// as a withdrawal (ADR-0114), so the source keeps offering it with Held false.
type Entry struct {
	BlobHash string `json:"blob_hash"`
	Size     int64  `json:"size"`
	Held     bool   `json:"held"`
}

// Range is a half-open range of the hash space, [Lo, Hi), stated as lowercase
// hex prefixes of the digest after its algorithm prefix. An empty Hi is the end
// of the space.
type Range struct {
	Lo string `json:"lo"`
	Hi string `json:"hi"`
}

// Ranges is the partition one cycle walks: sixteen ranges, one per leading hex
// digit. Sixteen keeps each answer to a sixteenth of the offered set (a few
// hundred kilobytes for a household vault) and a converged cycle to sixteen
// digest round trips.
func Ranges() []Range {
	const digits = "0123456789abcdef"
	out := make([]Range, 0, len(digits))
	for i := range len(digits) {
		r := Range{Lo: digits[i : i+1]}
		if i+1 < len(digits) {
			r.Hi = digits[i+1 : i+2]
		}
		out = append(out, r)
	}
	return out
}

// Validate checks the bounds are lowercase hex and ordered.
func (r Range) Validate() error {
	for _, b := range []string{r.Lo, r.Hi} {
		for _, c := range b {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return fmt.Errorf("%w: range bound %q is not lowercase hex", ErrInvalid, b)
			}
		}
	}
	if r.Hi != "" && r.Lo >= r.Hi {
		return fmt.Errorf("%w: range [%q, %q) is empty or reversed", ErrInvalid, r.Lo, r.Hi)
	}
	return nil
}

// Bounds is the range as bounds over the full digest string, for a query:
// hash >= lo AND (hi = "" OR hash < hi).
func (r Range) Bounds() (lo, hi string) {
	lo = hashPrefix + r.Lo
	if r.Hi != "" {
		hi = hashPrefix + r.Hi
	}
	return lo, hi
}

// Contains reports whether a canonical digest falls in the range.
func (r Range) Contains(hash string) bool {
	lo, hi := r.Bounds()
	return hash >= lo && (hi == "" || hash < hi)
}

// String is the range for logs.
func (r Range) String() string {
	hi := r.Hi
	if hi == "" {
		hi = "end"
	}
	return "[" + r.Lo + ", " + hi + ")"
}

// Digest fingerprints a set of entries, so two sides can tell they agree
// without sending the set. Order-independent: entries are sorted first.
//
// It is a fingerprint, not an identity, so it is SHA-256 over a canonical line
// format rather than anything Invariant 1 governs. The empty set has a digest
// too, and it is not the empty string, so "I took nothing" and "I sent no
// digest" stay distinguishable.
func Digest(entries []Entry) string {
	lines := make([]string, len(entries))
	for i, e := range entries {
		lines[i] = e.BlobHash + " " + strconv.FormatInt(e.Size, 10) + " " + strconv.FormatBool(e.Held)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Ask is what a destination sends a source.
type Ask struct {
	Range
	// Digest is the asker's Digest of what it currently holds as taken from
	// this source inside the range. When the source's offer has the same
	// digest, it answers Unchanged and sends no entries.
	Digest string `json:"digest"`
	// ObservedAt is when the asker looked at its own disk for Held.
	ObservedAt time.Time `json:"observed_at"`
	// Claim says Held is included: the asker's COMPLETE claim about its own
	// disk in this range. Complete, so the source can
	// mark any of the asker's present replicas the claim omits as missing,
	// including blobs it no longer offers; a claim that only covered the
	// current offer would leave a withdrawn blob "held" forever. An asker
	// sends it when it changed, and an empty claim is a claim.
	Claim bool `json:"claim"`
	// Held is every blob in the range the asker holds present.
	Held []inventory.Entry `json:"held,omitempty"`
}

// Validate checks an Ask's shape. It runs on both ends: the asker validates
// so a malformed question fails where it was built, and the source validates
// because a member is authenticated, not trusted.
func (a Ask) Validate() error {
	if err := a.Range.Validate(); err != nil {
		return err
	}
	if !strings.HasPrefix(a.Digest, "sha256:") {
		return fmt.Errorf("%w: digest %q is not a sha256 set digest", ErrInvalid, a.Digest)
	}
	if !a.Claim {
		if len(a.Held) != 0 {
			return fmt.Errorf("%w: held entries without a claim", ErrInvalid)
		}
		return nil
	}
	if len(a.Held) == 0 {
		return nil
	}
	report := inventory.Report{PeerID: "-", Mode: inventory.ModeIncremental, ObservedAt: a.ObservedAt, Entries: a.Held}
	if err := report.Validate(); err != nil {
		return fmt.Errorf("%w: held: %w", ErrInvalid, err)
	}
	for _, e := range a.Held {
		if !a.Contains(e.BlobHash) {
			return fmt.Errorf("%w: held blob %s is outside the range %s", ErrInvalid, e.BlobHash, a.Range)
		}
		if e.State != inventory.StatePresent {
			return fmt.Errorf("%w: held blob %s is %q: a claim lists what is present, and absence says the rest",
				ErrInvalid, e.BlobHash, e.State)
		}
	}
	return nil
}

// Offer is what a source answers: what it offers the asker inside the range.
type Offer struct {
	Range
	// Digest is the source's Digest of the offered set.
	Digest string `json:"digest"`
	// Unchanged is true when Digest matched the asker's; Entries is then empty
	// and the asker keeps what it has.
	Unchanged bool `json:"unchanged"`
	// Entries is the whole offered set in the range, in hash order.
	Entries []Entry `json:"entries"`
}

// Validate checks an Offer answers the range it was asked about and names
// only canonical digests inside it, once each.
func (o Offer) Validate(asked Range) error {
	if o.Range != asked {
		return fmt.Errorf("%w: answer covers %s, the question asked %s", ErrInvalid, o.Range, asked)
	}
	if o.Unchanged {
		if len(o.Entries) != 0 {
			return fmt.Errorf("%w: an unchanged answer carries entries", ErrInvalid)
		}
		return nil
	}
	seen := make(map[string]struct{}, len(o.Entries))
	for i, e := range o.Entries {
		if _, err := hashing.Parse(e.BlobHash); err != nil {
			return fmt.Errorf("%w: entry %d: %w", ErrInvalid, i, err)
		}
		if !o.Contains(e.BlobHash) {
			return fmt.Errorf("%w: entry %d (%s) is outside the range %s", ErrInvalid, i, e.BlobHash, o.Range)
		}
		if e.Size < 0 {
			return fmt.Errorf("%w: entry %d (%s) has a negative size", ErrInvalid, i, e.BlobHash)
		}
		if _, dup := seen[e.BlobHash]; dup {
			return fmt.Errorf("%w: entry %d: %s appears twice", ErrInvalid, i, e.BlobHash)
		}
		seen[e.BlobHash] = struct{}{}
	}
	if d := Digest(o.Entries); d != o.Digest {
		return fmt.Errorf("%w: the answer's digest %s does not match its entries (%s)", ErrInvalid, o.Digest, d)
	}
	return nil
}
