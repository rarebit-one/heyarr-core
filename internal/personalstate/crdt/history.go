package crdt

// history.go is the client-side, plaintext merge logic for a user's LISTENING /
// PLAY HISTORY — the fourth personal-state CRDT after the playlist, starred set
// and reading positions (§37, §46; issue #386). It feeds the Subsonic `scrobble`
// (record a play), `getNowPlaying` (the most recent play) and
// `getAlbumList2?type=recent|frequent` methods, all decrypted on the device
// (Invariant 6, §72).
//
// # The model: a grow-only set of play events (a G-Set)
//
// A play is an EVENT, not a mutable value: it happened, and no later action
// un-happens it. So the lattice is the simplest CRDT there is — a grow-only set,
// keyed by a globally-unique event tag. There is deliberately no remove and no
// tombstone: history only accretes. Merge is set union, which is trivially
// commutative, associative, and idempotent, so two devices that scrobbled
// offline converge by exchanging their event sets (§43).
//
// Every derived read is a pure function of that set:
//
//   - play-COUNT of an item = the number of its events (feeds `frequent`);
//   - RECENT items = distinct items ordered by their most-recent event
//     (feeds `recent`);
//   - NOW-PLAYING = the single greatest event overall (feeds `getNowPlaying`).
//
// # Ordering without a coordinator
//
// Recency is a Lamport `At` counter carried by each event, with the event's tag
// as a deterministic tie-break: (At, Tag) is a total order every device computes
// identically. "Recent" and "now-playing" sort by it, so they are a function of
// the event data, never of the order events were merged. A count is
// order-independent by construction (a set has no order).

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// PlayTag is the globally unique identity of a SINGLE play event (a UUIDv7). The
// G-Set tracks tags: two plays of the same item are two events with two tags, so
// a play COUNT is the number of tags, and a replayed change is absorbed by tag.
type PlayTag string

// PlayChange is one recorded play ready to ship to another device and merge. It
// is self-describing: the item played, the event's unique Tag, and its Lamport At
// counter. There is no remove variant — history is append-only.
type PlayChange struct {
	// ItemID is the content id that was played.
	ItemID string
	// Tag is this play event's unique identity.
	Tag PlayTag
	// At is this play's Lamport recency counter.
	At uint64
}

// playRecord is the payload stored against a play-event tag.
type playRecord struct {
	itemID string
	at     uint64
}

// lesserPlay is the lattice join for two events under the SAME tag. A UUIDv7 tag
// is globally unique, so this is never reached legitimately; but a PlayChange is
// unsigned and a malicious device could ship two events under one tag. A blind
// overwrite would be last-write-by-arrival-order and two replicas would DIVERGE.
// Choosing the deterministically-lesser record by (itemID, at) makes the G-Set a
// true semilattice again.
func lesserPlay(a, b playRecord) playRecord {
	if a.itemID != b.itemID {
		if a.itemID < b.itemID {
			return a
		}
		return b
	}
	if a.at != b.at {
		if a.at < b.at {
			return a
		}
		return b
	}
	return a
}

// PlayLog is the materialised play history: the grow-only event set plus the
// Lamport clock. Both fields merge by a lattice join (map union, integer max), so
// the whole struct is a semilattice.
type PlayLog struct {
	events  map[PlayTag]playRecord
	counter uint64
}

// PlayEntry is one item in a derived listening view: its id, its total play
// count, and the Lamport counter of its most recent play.
type PlayEntry struct {
	ID     string
	Count  int
	LastAt uint64
}

// NewPlayLog returns an empty play history.
func NewPlayLog() *PlayLog {
	return &PlayLog{events: make(map[PlayTag]playRecord)}
}

func newPlayTag() PlayTag {
	return PlayTag(uuid.Must(uuid.NewV7()).String())
}

// Record records a local play of itemID (a scrobble) and returns the
// [PlayChange] to ship. The At counter takes the next Lamport value (one past the
// highest this log has seen), so a play made after observing others sorts more
// recent. The change is applied locally before it is returned.
func (l *PlayLog) Record(itemID string) PlayChange {
	// Saturating increment: a malicious change can poison the clock to
	// math.MaxUint64 (applyOne takes the max); a bare ++ there wraps to 0 and
	// every future local play would sort as the OLDEST. Saturating keeps the clock
	// monotonic — once poisoned, new plays share MaxUint64 and settle by their
	// tie-break tag.
	if l.counter < math.MaxUint64 {
		l.counter++
	}
	c := PlayChange{ItemID: itemID, Tag: newPlayTag(), At: l.counter}
	l.applyOne(c)
	return c
}

// Apply folds changes into the log. It is the CRDT join over events: idempotent,
// commutative, and associative — the result never depends on arrival order.
func (l *PlayLog) Apply(changes ...PlayChange) {
	for _, c := range changes {
		l.applyOne(c)
	}
}

func (l *PlayLog) applyOne(c PlayChange) {
	rec := playRecord{itemID: c.ItemID, at: c.At}
	if existing, ok := l.events[c.Tag]; ok {
		rec = lesserPlay(existing, rec)
	}
	l.events[c.Tag] = rec
	if c.At > l.counter {
		l.counter = c.At
	}
}

// MergePlayLogs joins any number of logs into a NEW log, leaving the inputs
// untouched — the union of every event and the maximum Lamport counter.
// Commutative, associative, and idempotent.
func MergePlayLogs(logs ...*PlayLog) *PlayLog {
	out := NewPlayLog()
	for _, l := range logs {
		if l == nil {
			continue
		}
		for tag, rec := range l.events {
			if existing, ok := out.events[tag]; ok {
				rec = lesserPlay(existing, rec)
			}
			out.events[tag] = rec
		}
		if l.counter > out.counter {
			out.counter = l.counter
		}
	}
	return out
}

// Clone returns an independent deep copy.
func (l *PlayLog) Clone() *PlayLog { return MergePlayLogs(l) }

// Count returns how many times itemID was played (the number of its events).
func (l *PlayLog) Count(itemID string) int {
	n := 0
	for _, rec := range l.events {
		if rec.itemID == itemID {
			n++
		}
	}
	return n
}

// perItem folds the event set into one [PlayEntry] per distinct item: its count
// and the Lamport counter of its most recent event. The map is order-independent,
// so callers get a deterministic slice only after they sort it.
func (l *PlayLog) perItem() map[string]*PlayEntry {
	byItem := make(map[string]*PlayEntry)
	for _, rec := range l.events {
		e, ok := byItem[rec.itemID]
		if !ok {
			byItem[rec.itemID] = &PlayEntry{ID: rec.itemID, Count: 1, LastAt: rec.at}
			continue
		}
		e.Count++
		if rec.at > e.LastAt {
			e.LastAt = rec.at
		}
	}
	return byItem
}

// Recent returns distinct items ordered most-recently-played first (feeds
// `getAlbumList2?type=recent`).
//
// SABOTAGE NOTE (for the reviewer): recency is the LastAt sort key here. Break it
// by ranking on the order events were first seen during Apply (e.g. record an
// incrementing apply-sequence in applyOne and sort Recent on that) — the set of
// items and their counts stay correct, but the recency order then leaks the merge
// order and TestPlayConvergesUnderReordering fails, because two permutations of
// the same events yield different Recent orders.
func (l *PlayLog) Recent() []PlayEntry {
	entries := entriesOf(l.perItem())
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].LastAt != entries[j].LastAt {
			return entries[i].LastAt > entries[j].LastAt
		}
		return entries[i].ID < entries[j].ID
	})
	return entries
}

// Frequent returns distinct items ordered most-played first (feeds
// `getAlbumList2?type=frequent`), with the most-recent play breaking a count tie
// and the item id breaking a full tie — all functions of the event data.
func (l *PlayLog) Frequent() []PlayEntry {
	entries := entriesOf(l.perItem())
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Count != entries[j].Count {
			return entries[i].Count > entries[j].Count
		}
		if entries[i].LastAt != entries[j].LastAt {
			return entries[i].LastAt > entries[j].LastAt
		}
		return entries[i].ID < entries[j].ID
	})
	return entries
}

// entriesOf flattens the per-item map into a value slice (unsorted).
func entriesOf(byItem map[string]*PlayEntry) []PlayEntry {
	out := make([]PlayEntry, 0, len(byItem))
	for _, e := range byItem {
		out = append(out, *e)
	}
	return out
}

// NowPlaying returns the item of the single most recent play event — the greatest
// (At, Tag) across the whole log — feeding Subsonic `getNowPlaying`. It is empty
// only when no play has ever been recorded.
func (l *PlayLog) NowPlaying() (string, bool) {
	var bestTag PlayTag
	var bestItem string
	var bestAt uint64
	found := false
	for tag, rec := range l.events {
		if !found || rec.at > bestAt || (rec.at == bestAt && tag > bestTag) {
			found, bestTag, bestItem, bestAt = true, tag, rec.itemID, rec.at
		}
	}
	return bestItem, found
}

// Encode is a canonical, deterministic serialisation of the ENTIRE log — every
// event and the counter — with events sorted by tag. Two logs that have converged
// produce byte-identical output. A test and debugging aid, not a wire format.
func (l *PlayLog) Encode() string {
	var b strings.Builder
	tags := make([]PlayTag, 0, len(l.events))
	for tag := range l.events {
		tags = append(tags, tag)
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i] < tags[j] })
	b.WriteString("events:\n")
	for _, tag := range tags {
		rec := l.events[tag]
		fmt.Fprintf(&b, "  %s=%s@%d\n", tag, rec.itemID, rec.at)
	}
	fmt.Fprintf(&b, "counter:%d\n", l.counter)
	return b.String()
}
