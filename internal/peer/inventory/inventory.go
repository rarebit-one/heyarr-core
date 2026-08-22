// Package inventory is what a peer actually holds, and the wire shape it
// reports that in (§19, §20, §21, ADR-0029, M4-07).
//
// # replicas is a belief; an inventory is a disk
//
// The controller's `replicas` table already looks like an inventory:
// (blob_hash, peer_id, state, bytes_present, verified_at). It is not one. It
// is what the CONTROLLER believes. Every writer of that table before this
// milestone resolves the self peer first, so no row has ever described a
// machine other than the one that wrote it, and belief and disk have never had
// the chance to disagree. A second Full Peer is what gives them the chance: a
// peer that lost a disk, restored an older CAS, or quarantined a blob holds an
// inventory the controller's table does not reflect.
//
// Disk reality wins. §21 puts verification on the destination for exactly this
// reason — the ground truth about bytes is the bytes — so this package derives
// the inventory from the content store and NOT from the peer's own catalog
// beliefs. A collector that read a catalog would report the same fiction the
// controller already holds, and the exchange would confirm nothing.
//
// # A report can take a replica away
//
// The consequence worth stating plainly, because it is the kind that gets
// discovered late: a peer reporting that it no longer holds a blob must be
// able to move a `replicas` row from `present` to `missing`. An inventory that
// could only ADD replicas converges on a table that never shrinks and always
// claims the library is safer than it is — and that table is what garbage
// collection consults before deleting what it thinks is a surplus copy.
//
// Removals become `missing`, never deleted rows. A peer that lost bytes must
// be visible, not silently absent.
//
// # Full and incremental, and what each one CONFIRMS
//
// A full report carries everything the peer holds. An incremental report
// carries only what changed since the peer's own previous observation, so a
// library of any size does not ship its whole set every cycle.
//
// The difference is not only size, it is what each one asserts about a blob it
// does not mention:
//
//   - A FULL report mentions every blob the peer has. A blob absent from it is
//     positively asserted absent, so the controller marks it missing, and the
//     peer HAS confirmed that blob — by exclusion. Freshness advances.
//   - An INCREMENTAL report asserts nothing about a blob it does not mention.
//     The row is untouched and its freshness does NOT advance, because nobody
//     confirmed it. An incremental report communicates a loss by carrying an
//     explicit `missing` entry.
//
// That is what makes the full report the drift corrector: it is the only shape
// that can say "and nothing else", which is the statement that repairs a
// `replicas` row nobody has contradicted in a year.
package inventory

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// Mode is the shape of a report. See the package doc for what each one
// confirms about a blob it does not mention — the two are not interchangeable
// and the controller's reconciliation branches on this value.
type Mode string

// The report shapes.
const (
	// ModeFull is everything the peer holds. Absence is an assertion.
	ModeFull Mode = "full"
	// ModeIncremental is what changed since the peer's previous observation.
	// Absence is silence.
	ModeIncremental Mode = "incremental"
)

// Valid reports whether m is a mode this package understands.
func (m Mode) Valid() bool { return m == ModeFull || m == ModeIncremental }

// State is what a peer reports about one blob.
//
// It is a subset of replicas.state on purpose. `pending` is a controller-side
// fact — a transfer it scheduled and is waiting for — and a peer reporting it
// would be asserting something about the controller's plans rather than about
// its own disk.
type State string

// The states a peer may report.
const (
	// StatePresent is bytes on disk, addressable, servable.
	StatePresent State = "present"
	// StateCorrupt is bytes the peer still HAS and cannot serve: quarantined,
	// because they stopped hashing to their own name (ADR-0018).
	//
	// Reported rather than omitted. Omitting it would report the blob as gone
	// and invite a replacement transfer over the evidence; reporting it
	// present would leave the controller believing in a copy nothing can read.
	StateCorrupt State = "corrupt"
	// StateMissing is a blob this peer no longer holds. It is how an
	// incremental report communicates a loss.
	StateMissing State = "missing"
)

// Valid reports whether s is a state a peer may report.
func (s State) Valid() bool {
	return s == StatePresent || s == StateCorrupt || s == StateMissing
}

// Entry is one blob, as the reporting peer's disk has it.
type Entry struct {
	// BlobHash is the canonical digest (ADR-0005). Identity is the hash and
	// nothing else — not a path, not a row id.
	BlobHash string `json:"blob_hash"`
	// State is what the peer holds. See State.
	State State `json:"state"`
	// BytesPresent is how many bytes the peer has. It is the file's length,
	// which for a present blob is the whole thing and for anything else is 0 —
	// partial holdings are chunk-level and are M4-11's.
	BytesPresent int64 `json:"bytes_present"`
	// VerifiedAt is when the reporting peer last confirmed these bytes hash to
	// their own name, if it has ever done so.
	//
	// It is nil rather than "now" when the peer has no verification record.
	// Collecting an inventory reads a directory; it does not re-hash a library
	// (that is a deep fsck, and it costs a full read of every blob). A
	// collector that stamped the collection time here would manufacture
	// verification evidence out of a directory listing, and every later
	// decision that trusts verified_at would be trusting that.
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
}

// Report is what a peer sends to the controller.
type Report struct {
	// PeerID is a DECLARATION, not a credential (ADR-0033). The controller
	// derives the acting peer from the client certificate, compares this
	// against it, and refuses a mismatch. It is never read as the answer.
	PeerID string `json:"peer_id"`
	// Mode is full or incremental.
	Mode Mode `json:"mode"`
	// ObservedAt is when the PEER looked at its disk, on the peer's clock.
	// Distinct from when the controller received it: the gap between the two
	// is how stale the report already was when it arrived.
	ObservedAt time.Time `json:"observed_at"`
	// Entries is the inventory. A full report with no entries is a peer that
	// holds nothing, which is a legitimate and important thing to be able to
	// say — a peer whose disk is empty must be able to report that, and an
	// implementation that treated the empty set as "nothing to say" would let
	// a wiped peer keep its replicas forever.
	Entries []Entry `json:"entries"`
}

// ErrUnknownPeer is a report from a peer with no row in the catalog.
//
// It is distinct from a malformed report because it is not the peer's fault
// and the peer can do nothing about it: the caller authenticated against
// membership and pinned correctly, and the catalog simply has no row to hang
// replicas from. That is an operator-visible disagreement between two tables,
// and reporting it as a bad request would send the peer looking at its own
// inventory for a problem that is not there.
var ErrUnknownPeer = errors.New("inventory: no catalog row for this peer")

// ErrInvalidReport is a report this package will not put on the wire, and will
// not fold in when it comes off one.
var ErrInvalidReport = errors.New("inventory: invalid report")

// Validate checks a report's shape.
//
// It runs on both ends deliberately. The peer validates so a malformed report
// fails where it was built, and the controller validates because a peer is
// authenticated, not trusted: membership proves which peer is speaking, not
// that it is speaking sense.
func (r Report) Validate() error {
	if !r.Mode.Valid() {
		return fmt.Errorf("%w: mode %q is neither %q nor %q", ErrInvalidReport, r.Mode, ModeFull, ModeIncremental)
	}
	if r.ObservedAt.IsZero() {
		return fmt.Errorf("%w: observed_at is required — a report with no observation time "+
			"cannot be aged, and freshness is the whole point of recording one", ErrInvalidReport)
	}
	seen := make(map[string]bool, len(r.Entries))
	for i, e := range r.Entries {
		if _, err := hashing.Parse(e.BlobHash); err != nil {
			return fmt.Errorf("%w: entry %d: %w", ErrInvalidReport, i, err)
		}
		if !e.State.Valid() {
			return fmt.Errorf("%w: entry %d (%s): state %q is not one of %q, %q, %q",
				ErrInvalidReport, i, e.BlobHash, e.State, StatePresent, StateCorrupt, StateMissing)
		}
		if e.BytesPresent < 0 {
			return fmt.Errorf("%w: entry %d (%s): bytes_present is negative", ErrInvalidReport, i, e.BlobHash)
		}
		if seen[e.BlobHash] {
			// Two entries for one blob would make the folded-in result depend
			// on iteration order, which is a trust question answered by
			// ORDER BY. Refused rather than deduplicated.
			return fmt.Errorf("%w: entry %d: %s appears twice", ErrInvalidReport, i, e.BlobHash)
		}
		seen[e.BlobHash] = true
	}
	return nil
}

// Outcome is what folding a report into `replicas` did.
//
// Counts, not rows. The per-blob facts are durable in `replicas`, which is
// where anything wanting the detail must look — the same argument that keeps
// per-blob events out of the event log during an inventory exchange.
type Outcome struct {
	// ReportID is the receipt row this report was recorded as.
	ReportID string `json:"report_id"`
	// PeerID is the peer the CONTROLLER acted on, derived from the
	// certificate. A peer comparing it against what it sent is asserting that
	// its report landed under its own identity.
	PeerID string `json:"peer_id"`
	Mode   Mode   `json:"mode"`
	// Entries is how many the report carried.
	Entries int `json:"entries"`
	// Added is replicas rows this report created.
	Added int `json:"added"`
	// Changed is existing rows whose state or byte count this report altered,
	// excluding those it moved to missing.
	Changed int `json:"changed"`
	// Removed is rows this report moved to `missing` — by an explicit missing
	// entry, or by absence from a full report. It is counted apart from
	// Changed because it is the only direction that makes the library less
	// safe.
	Removed int `json:"removed"`
	// Unknown is entries naming a blob this controller has no row for. Not an
	// error: a peer restored from a newer catalog legitimately holds bytes
	// this controller has not learned about. It cannot be recorded as a
	// replica of a blob that does not exist, so it is counted and reported.
	Unknown    int       `json:"unknown"`
	ObservedAt time.Time `json:"observed_at"`
	ReceivedAt time.Time `json:"received_at"`
}

// Snapshot is one observation of a peer's store.
//
// It is the peer's own working state, not a wire shape: a peer keeps its last
// snapshot so the next cycle can be a diff rather than the whole library.
type Snapshot struct {
	// ObservedAt is when this observation was taken.
	ObservedAt time.Time
	// byHash is the observation. Unexported so a Snapshot cannot be mutated
	// after the fact by a caller holding the map — a diff against a snapshot
	// somebody edited would silently report changes that never happened.
	byHash map[string]Entry
}

// NewSnapshot builds a snapshot from entries. A duplicate blob is an error for
// the same reason Validate refuses one: the result would depend on order.
func NewSnapshot(observedAt time.Time, entries []Entry) (Snapshot, error) {
	byHash := make(map[string]Entry, len(entries))
	for _, e := range entries {
		if _, dup := byHash[e.BlobHash]; dup {
			return Snapshot{}, fmt.Errorf("%w: %s appears twice in one observation", ErrInvalidReport, e.BlobHash)
		}
		byHash[e.BlobHash] = e
	}
	return Snapshot{ObservedAt: observedAt, byHash: byHash}, nil
}

// Len is how many blobs the observation covers.
func (s Snapshot) Len() int { return len(s.byHash) }

// Entries lists the observation, ordered by hash so a report is byte-stable
// for the same disk. Determinism here is what lets a test compare two reports
// rather than two set-membership assertions (ADR-0017).
func (s Snapshot) Entries() []Entry {
	out := make([]Entry, 0, len(s.byHash))
	for _, e := range s.byHash {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BlobHash < out[j].BlobHash })
	return out
}

// Full renders the whole observation as a full report.
func (s Snapshot) Full(peerID string) Report {
	return Report{PeerID: peerID, Mode: ModeFull, ObservedAt: s.ObservedAt, Entries: s.Entries()}
}

// Since renders what changed between previous and s as an incremental report.
//
// A blob that appeared, or whose state or byte count moved, is carried as it
// now is. A blob that was in previous and is gone from s is carried as an
// explicit `missing` entry — which is the whole reason an incremental report
// can take a replica away rather than only ever adding them.
//
// A blob that changed in neither is omitted, and the controller will not touch
// its row or its freshness. That is the size win: a cycle in which nothing
// happened ships an empty entry list.
func (s Snapshot) Since(previous Snapshot, peerID string) Report {
	var entries []Entry
	for hash, now := range s.byHash {
		before, existed := previous.byHash[hash]
		if existed && before.State == now.State && before.BytesPresent == now.BytesPresent &&
			sameInstant(before.VerifiedAt, now.VerifiedAt) {
			continue
		}
		entries = append(entries, now)
	}
	for hash, before := range previous.byHash {
		if _, still := s.byHash[hash]; still {
			continue
		}
		if before.State == StateMissing {
			// Already reported gone last cycle. Re-reporting it would be
			// telling the controller the same thing forever, and the whole
			// point of an incremental report is that it stops.
			continue
		}
		entries = append(entries, Entry{BlobHash: hash, State: StateMissing, BytesPresent: 0})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].BlobHash < entries[j].BlobHash })
	return Report{PeerID: peerID, Mode: ModeIncremental, ObservedAt: s.ObservedAt, Entries: entries}
}

// sameInstant compares two optional times, treating both-absent as equal.
func sameInstant(a, b *time.Time) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Equal(*b)
	}
}
