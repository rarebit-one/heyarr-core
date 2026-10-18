package crdt

// drive.go is the client-side, plaintext merge logic for a VAULT DRIVE — a
// person's files as a mutable, path-addressed filesystem (ADR-0095), the fifth
// personal-state CRDT after the playlist, the starred set, the reading position
// and the history log (§37, §43, §45). Its bytes live in the CAS as ciphertext
// blobs (ADR-0021); THIS file owns only the encrypted namespace over them, and,
// like every CRDT here, it is decrypted and merged on the device — a peer holding
// the opaque changes reads none of it (Invariant 6, §72; ADR-0049).
//
// # The model: a path-keyed map, heads and history DERIVED from the write set
//
// The catalog is a map from a normalised path to an entry (ADR-0095). A directory
// is not a node — it exists exactly as long as some live path has it as a prefix —
// so a rename or move is remove-at-old + add-at-new, two changes, and the
// move-tree CRDT's hard case is unspellable rather than merely handled.
//
// A path does NOT reduce to a last-writer-wins register: two devices writing
// different blobs to one path with neither built on the other must BOTH survive,
// the loser relocated to a "conflicted copy" path, because for a file "keep the
// last write" is data loss (ADR-0095).
//
// The lattice that makes this order-independent: a path ACCUMULATES the set of
// every write it has seen (keyed by the write's own total-order key), and a delete
// raises a tombstone key. The live heads and the version history are then a PURE
// FUNCTION of that set, not of arrival order:
//
//   - each write carries the key of the head it OBSERVED ([DriveChange.Base]);
//   - a write is a PRIOR VERSION exactly when some other write in the set was
//     built on it (its key appears as another write's Base) — someone superseded
//     it. This is a property of the set, so replaying in any order yields the same
//     answer (§43);
//   - the LIVE HEADS are the writes nobody built on, minus those the tombstone
//     covers. One head is the normal case; more than one is an unresolved
//     conflict (two writes sharing a Base).
//
// Because the state is just "union the write sets, take the max tombstone," Apply
// (folding changes) and [MergeDrives] (folding materialised drives) are the same
// join and cannot disagree — the property an earlier heads/versions-by-arrival
// draft failed (a change arriving before its Base diverged).
//
// # Ordering without a coordinator
//
// Precedence uses the SAME total order [posKey] = (At, Writer) that
// [ReadingPositions] uses: a Lamport counter dominates and a per-write UUIDv7 tag
// breaks a tie identically on every replica (ADR-0095).
//
// STATUS: #538 / ADR-0095. Convergence, version history, trash and the two-way
// conflict (multiple live heads) are implemented and order-independent. W2 adds,
// as PURE FUNCTIONS of the converged write set so they inherit its
// order-independence: blob-id validation on ingress ([DriveChange.Validate]);
// Unicode NFC in [normalisePath]; conflicted-copy RELOCATION ([Drive.Resolved] in
// drive_conflict.go), which presents every loser head as a visible file at a
// derived path every replica computes identically; and a client-side retention /
// GC reference view ([Drive.Retain] in drive_retention.go) reporting the blob ids a
// policy leaves unreferenced.
//
// Still deferred (own follow-ups, not W2): a dotted version vector so chained /
// multi-way put-vs-delete concurrency resolves by causality rather than by the
// total order this model uses, and the CONTROL-PLANE GC that actually reclaims the
// bytes Retain reports (ADR-0018).

import (
	"encoding/json"
	"fmt"
	"math"
	"path"
	"sort"
	"strings"

	"github.com/rarebit-one/void-which-binds-go/hashing"
	"golang.org/x/text/unicode/norm"
)

// ErrInvalidChange rejects a [DriveChange] that is not well-formed BEFORE it can
// enter the CRDT (task W2/#538). A malformed change must never reach the write set:
// [Put] returns it wrapped so a local write fails at the source, and [Apply] SKIPS
// any change that carries it so a corrupt or hostile peer change cannot poison the
// merge. Skipping is deterministic — validity is a pure function of the change's
// bytes, so every replica drops the identical set and convergence is preserved.
var ErrInvalidChange = fmt.Errorf("crdt: invalid drive change")

// driveValue is one blob placed at one path by one write: the ciphertext blob id
// (ADR-0021), its plaintext size and content mtime, the write's own total-order
// key, and the key of the head it observed (base). It reuses [posKey]/[PosTag]
// from readingpos.go — same package, same coordinator-free total order.
type driveValue struct {
	Blob  string    // ciphertext blob id (BLAKE3 of ciphertext, ADR-0021)
	Size  int64     // plaintext size, a client-side hint the peer never sees
	MTime int64     // content modification time (unix seconds), display only
	Meta  DriveMeta // this write's optional metadata (ADR-0113)
	key   posKey    // this write's own precedence key
	base  posKey    // the head this write observed (zero == observed nothing live)
}

// refs are the blob ids this write references: its content and its metadata's.
func (v driveValue) refs() []string {
	if v.Blob == "" {
		return nil
	}
	return append([]string{v.Blob}, v.Meta.blobRefs()...)
}

// OpKind is what a [DriveChange] does at its path.
type OpKind uint8

const (
	// OpPut sets (or creates) the blob at Path.
	OpPut OpKind = iota
	// OpDelete tombstones Path — the file leaves the live tree but its last blob is
	// retained as trash until retention drops it (ADR-0095; retention = TODO).
	OpDelete
)

// DriveChange is one write to the drive, ready to ship and merge. Its exported
// JSON fields ARE the wire contract: the bridge marshals this struct and encrypts
// the bytes (statesync.EncodeChange -> client.Manager.Encrypt), so the shape is
// also the cross-language parity contract (§7). A rename emits TWO changes — an
// OpDelete at the old path and an OpPut at the new.
//
// Kind is the ADR-0113 `k` tag. A write made here carries [KindDrive]; an untagged
// change predates the tag. The embedded [DriveMeta] is OpPut-only optional metadata
// whose fields sit inline in the JSON.
type DriveChange struct {
	Kind  string `json:"k,omitempty"` // ADR-0113 kind tag: "drive", or absent on a legacy change
	Op    OpKind `json:"op"`
	Path  string `json:"path"`            // normalised (see normalisePath)
	Blob  string `json:"blob,omitempty"`  // OpPut only
	Size  int64  `json:"size,omitempty"`  // OpPut only
	MTime int64  `json:"mtime,omitempty"` // OpPut only
	DriveMeta
	At     uint64 `json:"at"`     // this write's Lamport counter
	Writer PosTag `json:"writer"` // this write's unique tie-break identity
	// Base is the total-order key of the head this write observed and means to
	// replace (zero value == the writer saw no live head). A write whose key is
	// some other write's Base is thereby a prior version. TODO(#538): promote this
	// single-parent hint to a dotted version vector for faithful multi-way and
	// put-vs-delete concurrency (ADR-0095).
	Base posKey `json:"base"`
}

// Validate reports whether the change is well-formed enough to enter the CRDT
// (task W2/#538). The rules, checked identically on every replica so the accept /
// reject decision is a pure function of the bytes:
//
//   - Op is one of the known kinds;
//   - an OpPut names a blob that parses as a canonical "blake3:<64 hex>" id
//     (internal/hashing.Parse) — a client encrypts and content-addresses a blob
//     before it writes the path, so a put without a well-formed id is a bug or an
//     attack, never a real write;
//   - an OpDelete carries NO blob: the trash blob is DERIVED from the retained
//     writes the tombstone covers, not carried on the delete, so a blob here is
//     malformed. An empty blob is therefore valid ONLY for a delete.
//
// A zero Writer is rejected too: the (At, Writer) total order needs a tie-break
// tag, and a write with none could not be ordered deterministically against a
// concurrent one.
//
// ADR-0113 adds: the path must be non-empty once normalised (a stray change could
// otherwise tombstone ""), an OpPut's metadata must pass [DriveMeta.Validate], and
// an OpDelete carries no metadata.
func (c DriveChange) Validate() error {
	if c.Writer == "" {
		return fmt.Errorf("%w: write has no writer tag", ErrInvalidChange)
	}
	if normalisePath(c.Path) == "" {
		return fmt.Errorf("%w: a change must name a path", ErrInvalidChange)
	}
	switch c.Op {
	case OpPut:
		if c.Blob == "" {
			return fmt.Errorf("%w: an OpPut must name a blob", ErrInvalidChange)
		}
		if _, err := hashing.Parse(c.Blob); err != nil {
			return fmt.Errorf("%w: blob %q is not a canonical blake3 id: %w", ErrInvalidChange, c.Blob, err)
		}
		if err := c.DriveMeta.Validate(); err != nil {
			return err
		}
	case OpDelete:
		if c.Blob != "" {
			return fmt.Errorf("%w: an OpDelete carries no blob, got %q", ErrInvalidChange, c.Blob)
		}
		if !c.IsZero() {
			return fmt.Errorf("%w: an OpDelete carries no metadata", ErrInvalidChange)
		}
	default:
		return fmt.Errorf("%w: unknown op %d", ErrInvalidChange, c.Op)
	}
	return nil
}

// UnmarshalJSON reads the `k` tag before anything else (ADR-0113): a change of
// another kind decodes to just its tag, which [DriveChange.Foreign] then skips,
// whatever shape the rest of its payload has.
func (c *DriveChange) UnmarshalJSON(b []byte) error {
	kind, foreign, err := foreignKind(b, KindDrive)
	if err != nil {
		return err
	}
	if foreign {
		*c = DriveChange{Kind: kind}
		return nil
	}
	type plain DriveChange
	return json.Unmarshal(b, (*plain)(c))
}

// Foreign reports whether the change belongs to ANOTHER kind sharing the space
// (ADR-0113): it is tagged with a `k` other than [KindDrive], or it is untagged and
// has no path, which by shape makes it a legacy change of another kind (an album
// change). A fold skips a foreign change silently: it is not malformed.
func (c DriveChange) Foreign() bool {
	if c.Kind != "" {
		return c.Kind != KindDrive
	}
	return c.Path == ""
}

// driveRecord is what is stored against a path: the accumulated set of writes
// (keyed by their own key) and the greatest tombstone. Heads and history are
// derived from this set, never stored, so two converged records are equal.
type driveRecord struct {
	values map[posKey]driveValue
	delKey posKey // greatest OpDelete key seen (zero == never deleted)
}

// Drive is the materialised path->record map plus the Lamport clock. Both fields
// join by set-union / max, so the whole struct is a semilattice (§43).
type Drive struct {
	entries map[string]driveRecord
	counter uint64
}

// DriveEntry is one live file's current state for a caller: its path, the chosen
// blob, and whether the path currently has more than one live head (a conflict the
// UI should surface as conflicted copies until W2's relocation lands).
type DriveEntry struct {
	Path       string
	Blob       string
	Size       int64
	MTime      int64
	Meta       DriveMeta // the chosen head's metadata (ADR-0113); zero when it carried none
	Conflicted bool
}

// NewDrive returns an empty drive.
func NewDrive() *Drive { return &Drive{entries: make(map[string]driveRecord)} }

// normalisePath pins the wire rule ADR-0095 requires so two devices derive the
// IDENTICAL key for "the same path" and never conflict spuriously: Unicode NFC,
// forward-slash separators, cleaned, leading slash trimmed, case-sensitive.
//
// NFC first (ADR-0095 names it part of the wire rule): a macOS device that spells
// "é" as the decomposed pair U+0065 U+0301 and a Linux device that spells it as
// the composed U+00E9 mean the SAME path and must derive the same key, or they
// would fork one file into two and conflict spuriously. NFC is idempotent, so
// re-normalising an already-composed path is a no-op and the rule stays stable.
func normalisePath(p string) string {
	p = norm.NFC.String(p)
	return strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(p, "\\", "/")), "/")
}

func (d *Drive) nextAt() uint64 {
	// Saturating, as in readingpos.Set: a poisoned clock stays monotonic rather
	// than wrapping to 0 and sorting a fresh local write as the oldest.
	if d.counter < math.MaxUint64 {
		d.counter++
	}
	return d.counter
}

// liveHeads derives the current live blobs at this path from the accumulated set:
// the writes nobody built on, minus any the tombstone covers. Pure over the set
// and returned sorted by key, so the result is order-independent and deterministic.
func (r driveRecord) liveHeads() []driveValue {
	built := make(map[posKey]bool, len(r.values))
	for _, v := range r.values {
		if v.base != (posKey{}) {
			built[v.base] = true
		}
	}
	var live []driveValue
	for k, v := range r.values {
		if built[k] {
			continue // some later write superseded this one -> it is a prior version
		}
		if r.delKey != (posKey{}) && r.delKey.greater(k) {
			continue // the tombstone is newer than this write (skeleton: LWW put-vs-delete)
		}
		live = append(live, v)
	}
	sort.Slice(live, func(i, j int) bool { return live[j].key.greater(live[i].key) })
	return live
}

// currentHead returns the highest-precedence live blob and whether the path is
// currently conflicted (more than one live head).
func (r driveRecord) currentHead() (driveValue, bool, bool) {
	live := r.liveHeads()
	if len(live) == 0 {
		return driveValue{}, false, false
	}
	best := live[len(live)-1] // sorted ascending by key, so the last is greatest
	return best, true, len(live) > 1
}

// Put records a local write of blob at path and returns the [DriveChange] to ship.
// Base is the current head so a receiver can tell a clean successor from a
// concurrent divergence. The change is applied locally before it is returned.
//
// A blob that is not a canonical "blake3:<64 hex>" id is rejected with
// [ErrInvalidChange] and nothing is written — the malformed value never enters the
// CRDT (task W2/#538). This is the "fail at the source" half of the contract;
// [Apply] silently skips the same malformation arriving from a peer.
func (d *Drive) Put(p, blob string, size, mtime int64) (DriveChange, error) {
	return d.PutMeta(p, blob, size, mtime, DriveMeta{})
}

// PutMeta is [Put] carrying optional metadata (ADR-0113). Re-putting a path's
// current blob with new metadata is how metadata is added to an existing file:
// the write builds on the current head, so it supersedes rather than conflicts.
// Malformed metadata is rejected with [ErrInvalidChange] and nothing is written.
func (d *Drive) PutMeta(p, blob string, size, mtime int64, meta DriveMeta) (DriveChange, error) {
	np := normalisePath(p)
	var base posKey
	if head, ok, _ := d.entries[np].currentHead(); ok {
		base = head.key
	}
	c := DriveChange{Kind: KindDrive, Op: OpPut, Path: np, Blob: blob, Size: size, MTime: mtime, DriveMeta: meta, Writer: newPosTag(), Base: base}
	if err := c.Validate(); err != nil {
		return DriveChange{}, err
	}
	c.At = d.nextAt()
	d.applyOne(c)
	return c, nil
}

// Delete tombstones path and returns the [DriveChange] to ship. A path that is
// empty once normalised is rejected with [ErrInvalidChange] and nothing is written,
// so a local delete can never record a tombstone its peers would refuse.
func (d *Drive) Delete(p string) (DriveChange, error) {
	np := normalisePath(p)
	c := DriveChange{Kind: KindDrive, Op: OpDelete, Path: np, Writer: newPosTag()}
	if err := c.Validate(); err != nil {
		return DriveChange{}, err
	}
	c.At = d.nextAt()
	d.applyOne(c)
	return c, nil
}

// Apply folds changes into the drive. It is the CRDT join over writes: idempotent,
// commutative and associative — the result never depends on arrival order (§43).
//
// A change that fails [DriveChange.Validate] is SKIPPED, not applied: a malformed
// blob id or a delete carrying a blob is dropped so a corrupt or hostile peer
// cannot poison the merge (task W2/#538). The skip keeps the CRDT contract — the
// reject decision is a pure function of the change's bytes, so every replica drops
// the identical set and the survivors still converge regardless of order. Use
// [Drive.ApplyReport] when the caller needs to know WHICH changes were rejected.
// A change of another kind sharing the space ([DriveChange.Foreign]) is skipped
// too, silently (ADR-0113).
func (d *Drive) Apply(changes ...DriveChange) {
	d.ApplyReport(changes...)
}

// ApplyReport is [Apply] that also returns the changes it skipped as malformed, so
// a caller can log or surface a peer sending garbage. The applied changes converge
// exactly as under Apply; the rejected slice is a diagnostic, never CRDT state.
func (d *Drive) ApplyReport(changes ...DriveChange) (rejected []DriveChange) {
	for _, c := range changes {
		if c.Foreign() {
			continue // another kind's change: not ours, and not malformed (ADR-0113)
		}
		if err := c.Validate(); err != nil {
			rejected = append(rejected, c)
			continue
		}
		d.applyOne(c)
	}
	return rejected
}

func (d *Drive) applyOne(c DriveChange) {
	if c.At > d.counter {
		d.counter = c.At
	}
	np := normalisePath(c.Path)
	rec := d.entries[np]
	if rec.values == nil {
		rec.values = make(map[posKey]driveValue)
	}
	key := posKey{At: c.At, Writer: c.Writer}
	switch c.Op {
	case OpDelete:
		if key.greater(rec.delKey) {
			rec.delKey = key
		}
	case OpPut:
		rec.values[key] = driveValue{Blob: c.Blob, Size: c.Size, MTime: c.MTime, Meta: c.DriveMeta, key: key, base: c.Base}
	}
	d.entries[np] = rec
}

// MergeDrives joins any number of drives into a NEW drive, leaving the inputs
// untouched — per path the union of write sets and the maximum tombstone, and the
// maximum Lamport counter. Commutative, associative and idempotent, and identical
// to folding the same writes through Apply.
func MergeDrives(drives ...*Drive) *Drive {
	out := NewDrive()
	for _, d := range drives {
		if d == nil {
			continue
		}
		for p, rec := range d.entries {
			cur := out.entries[p]
			if cur.values == nil {
				cur.values = make(map[posKey]driveValue)
			}
			for k, v := range rec.values {
				cur.values[k] = v
			}
			if rec.delKey.greater(cur.delKey) {
				cur.delKey = rec.delKey
			}
			out.entries[p] = cur
		}
		if d.counter > out.counter {
			out.counter = d.counter
		}
	}
	return out
}

// Get returns the current blob at path, if the path is live.
func (d *Drive) Get(p string) (DriveEntry, bool) {
	rec, ok := d.entries[normalisePath(p)]
	if !ok {
		return DriveEntry{}, false
	}
	head, live, conflicted := rec.currentHead()
	if !live || head.Blob == "" {
		return DriveEntry{}, false
	}
	return DriveEntry{Path: normalisePath(p), Blob: head.Blob, Size: head.Size, MTime: head.MTime, Meta: head.Meta, Conflicted: conflicted}, true
}

// List returns every live file, sorted by path for a deterministic read.
func (d *Drive) List() []DriveEntry {
	out := make([]DriveEntry, 0, len(d.entries))
	for p, rec := range d.entries {
		head, live, conflicted := rec.currentHead()
		if !live || head.Blob == "" {
			continue
		}
		out = append(out, DriveEntry{Path: p, Blob: head.Blob, Size: head.Size, MTime: head.MTime, Meta: head.Meta, Conflicted: conflicted})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Versions returns the superseded blob ids retained at path, newest-precedence
// first — the version history a caller restores from (ADR-0095). A superseded write
// of a blob that is still live (a metadata-only rewrite, ADR-0113) is not a content
// version and is left out. Trash (a
// tombstoned path's covered blobs) is retained the same way. [Drive.Retain]
// (drive_retention.go) reports which of these a retention policy leaves
// unreferenced; a dedicated Trashed() reader is a separate follow-up.
func (d *Drive) Versions(p string) []string {
	rec, ok := d.entries[normalisePath(p)]
	if !ok {
		return nil
	}
	prior := rec.priorVersions()
	out := make([]string, 0, len(prior))
	for _, v := range prior {
		out = append(out, v.Blob)
	}
	return out
}

// priorVersions are the record's content version history, newest-precedence
// first: one write per blob that is not also a live head's blob. A metadata-only
// rewrite (ADR-0113) re-puts a blob, so a blob counts once, at its newest write.
func (r driveRecord) priorVersions() []driveValue {
	live := r.liveHeads()
	liveKeys := make(map[posKey]bool, len(live))
	liveBlobs := make(map[string]bool, len(live))
	for _, h := range live {
		liveKeys[h.key] = true
		liveBlobs[h.Blob] = true
	}
	prior := make([]driveValue, 0, len(r.values))
	for k, v := range r.values {
		if !liveKeys[k] && v.Blob != "" && !liveBlobs[v.Blob] {
			prior = append(prior, v)
		}
	}
	sort.Slice(prior, func(i, j int) bool { return prior[i].key.greater(prior[j].key) })
	seen := make(map[string]bool, len(prior))
	out := prior[:0]
	for _, v := range prior {
		if !seen[v.Blob] {
			seen[v.Blob] = true
			out = append(out, v)
		}
	}
	return out
}

// Encode is a canonical, deterministic serialisation of the ENTIRE drive, sorted
// by path. Two converged drives produce byte-identical output. A test and
// debugging aid, not a wire format.
func (d *Drive) Encode() string {
	var b strings.Builder
	paths := make([]string, 0, len(d.entries))
	for p := range d.entries {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	b.WriteString("entries:\n")
	for _, p := range paths {
		rec := d.entries[p]
		live := rec.liveHeads()
		fmt.Fprintf(&b, "  %s tombstone=%d:%s heads=%d writes=%d\n", p, rec.delKey.At, rec.delKey.Writer, len(live), len(rec.values))
		for _, h := range live {
			fmt.Fprintf(&b, "    head %s size=%d@%d:%s%s\n", h.Blob, h.Size, h.key.At, h.key.Writer, encodeMeta(h.Meta))
		}
	}
	fmt.Fprintf(&b, "counter:%d\n", d.counter)
	return b.String()
}

// encodeMeta renders metadata for [Drive.Encode]: empty when there is none, else
// " meta=" and the canonical JSON (struct field order, zero fields omitted).
func encodeMeta(m DriveMeta) string {
	if m.IsZero() {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err) // a struct of strings and integers always marshals
	}
	return " meta=" + string(b)
}
