package crdt

// album.go is the client-side, plaintext merge logic for a photo ALBUM — the
// first Mnemosyne-only personal-state kind (issue #756, epic #733) and the sixth
// CRDT in this package after the playlist, the starred set, the reading position,
// the history log and the vault drive. An album is an ORDERED collection of
// photos inside a space: each item names a vault photo by its sealed manifest id
// (ADR-0097) and carries the device-made thumbnail and EXIF-sidecar blob ids plus
// the plaintext BLAKE3 the device uses to dedupe the same photo added twice.
// Sharing an album means the album lives in a `family` / `shared` space
// (ADR-0108); there are no per-album ACLs.
//
// Like every CRDT here it is decrypted and merged on the device. Everything in an
// [AlbumChange] — the album id, the titles, the manifest ids, the plaintext hash,
// the dimensions — rides INSIDE the sealed change (statesync.EncodeChange), so the
// peer holding it reads none of it and the plaintext hash never becomes a
// confirmation oracle (Invariant 6, §72; ADR-0021). The server has no album code
// at all: it stores opaque changes and snapshots keyed by space, as it does for
// every other kind, and nothing here adds a migration or a route.
//
// # The model: a write set per album, the view DERIVED from it
//
// The lattice follows drive.go rather than the playlist: every album ACCUMULATES
// the set of writes it has seen, each keyed by the write's own total-order key
// (At, Writer), and the resolved album is a PURE FUNCTION of that set, never of
// arrival order (§43). Concretely, per album:
//
//   - CREATE and DELETE are two max-registers over the key. The album is live
//     iff its greatest create key beats its greatest delete key, and a delete
//     COVERS every write with a smaller key — the drive's tombstone rule — so a
//     re-created album starts empty rather than resurrecting its old items;
//   - TITLE, CAPTION and COVER are per-album LWW registers on (At, Writer), with
//     the value itself as the final tie-break so a hostile key collision still
//     settles identically (readingpos.go's laterRecord);
//   - an ADD ITEM mints the item's identity: the add's own key. Items are an
//     add-wins observed-remove set — a REMOVE tombstones exactly the add key it
//     observed ([AlbumChange.Base]), so an add it never saw survives; a concurrent
//     re-add of the same photo is a different key and is never covered;
//   - a MOVE is an LWW register on the item's position, keyed by the item's add
//     key and joined on the move's own key; the add's position is the register's
//     initial value, so a move that sorts before the add (hostile, or a reorder
//     that raced) is simply ignored.
//
// Because the state is "union the writes, max the registers", Apply (folding
// changes) and [MergeAlbums] (folding materialised states) are one join and
// cannot disagree.
//
// # Ordering, and dedupe, without a coordinator
//
// Items order by (Pos, At, Writer): an explicit position first, then the add's
// own key — so two devices that append concurrently land on the same Pos and are
// ordered by their keys identically everywhere. Pos is a uint64 the client spaces
// by [AlbumPosStep] so a move can take the midpoint; a client that runs out of
// room re-numbers with further moves. [Albums.Resolved] then collapses items that
// share a plaintext hash to the first in that order: the same photo added from two
// devices (two manifests, one plaintext) is shown once, and the hidden duplicate
// still exists in the write set, so the collapse is a pure projection.
//
// STATUS: #756. Convergence, the full op set, the resolved projection, snapshots
// and ingress validation are implemented and order-independent; parity vectors
// are minted into testdata/vectors/album.json for the Kotlin twin. Deferred:
// per-item captions, and a dotted version vector for chained put-vs-delete
// concurrency (the same follow-up the drive carries).

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/rarebit-one/void-which-binds-go/hashing"
)

// ErrInvalidAlbumChange rejects an [AlbumChange] that is not well-formed BEFORE it
// can enter the CRDT. The local writers return it wrapped so a bad write fails at
// the source; [Albums.Apply] SKIPS any change that carries it so a corrupt or
// hostile peer change cannot poison the merge. Validity is a pure function of the
// change's bytes, so every replica drops the identical set and still converges.
var ErrInvalidAlbumChange = fmt.Errorf("crdt: invalid album change")

// AlbumPosStep is the gap a local append leaves between item positions, so a
// later move can take a midpoint without renumbering its neighbours.
const AlbumPosStep uint64 = 1 << 16

// WriteKey is the exported spelling of a write's (At, Writer) total-order key —
// the identity an [AlbumChange] uses to name the item (the add) it removes or
// moves. It is the same total order [posKey] gives the drive and the reading
// position; it is a distinct type so its wire form is the lowercase `at` /
// `writer` the rest of the album change uses.
type WriteKey struct {
	At     uint64 `json:"at,omitempty"`
	Writer PosTag `json:"writer,omitempty"`
}

func (k WriteKey) key() posKey  { return posKey(k) }
func (k WriteKey) isZero() bool { return k == WriteKey{} }
func keyOf(k posKey) WriteKey   { return WriteKey(k) }

// fmtKey spells a key as "<at>:<writer>" for Encode.
func fmtKey(k posKey) string { return fmt.Sprintf("%d:%s", k.At, k.Writer) }

func (k posKey) isZero() bool { return k == posKey{} }

// covers reports whether a tombstone at k hides the write w: a set tombstone
// strictly newer than the write (the drive's put-vs-delete rule).
func (k posKey) covers(w posKey) bool { return !k.isZero() && k.greater(w) }

// PhotoManifest is the photo object an album item carries: where the photo's
// bytes are, and what the device needs to render and dedupe it without opening
// them. Every field lives INSIDE the sealed change / snapshot: the server stores
// the ciphertext and never sees a manifest id, a dimension, or — the one that
// would otherwise be a confirmation oracle (ADR-0021) — the plaintext hash.
type PhotoManifest struct {
	// Manifest is the vault blob id of the photo's sealed manifest (ADR-0097),
	// which in turn names the content blob. It is the item's content reference.
	Manifest string `json:"manifest"`
	// Thumbnail is the vault blob id of the device-made sealed thumbnail; the
	// server never thumbnails vault bytes (ADR-0109). Optional.
	Thumbnail string `json:"thumbnail,omitempty"`
	// Sidecar is the vault blob id of the sealed EXIF sidecar. Optional.
	Sidecar string `json:"sidecar,omitempty"`
	// PlaintextHash is the BLAKE3 of the photo's PLAINTEXT bytes, the in-space
	// dedupe key [Albums.Resolved] collapses on. Required.
	PlaintextHash string `json:"plaintext_hash"`
	// Width and Height are the pixel dimensions, display only.
	Width  uint32 `json:"width,omitempty"`
	Height uint32 `json:"height,omitempty"`
	// TakenAt is the capture time (unix seconds), display and sort hint only.
	TakenAt int64 `json:"taken_at,omitempty"`
}

// Validate reports whether the manifest is well-formed: the content manifest id
// and the plaintext hash are canonical "blake3:<64 hex>" ids, and so is every
// optional blob id that is present.
func (p PhotoManifest) Validate() error {
	if p.Manifest == "" {
		return fmt.Errorf("%w: an item must name its manifest", ErrInvalidAlbumChange)
	}
	if p.PlaintextHash == "" {
		return fmt.Errorf("%w: an item must carry its plaintext hash", ErrInvalidAlbumChange)
	}
	for _, id := range []struct{ what, id string }{
		{"manifest", p.Manifest},
		{"plaintext_hash", p.PlaintextHash},
		{"thumbnail", p.Thumbnail},
		{"sidecar", p.Sidecar},
	} {
		if id.id == "" {
			continue
		}
		if _, err := hashing.Parse(id.id); err != nil {
			return fmt.Errorf("%w: %s %q is not a canonical blake3 id: %w", ErrInvalidAlbumChange, id.what, id.id, err)
		}
	}
	return nil
}

// canonical is the deterministic tie-break form of a manifest, used only when a
// hostile peer lands two different items on one key.
func (p PhotoManifest) canonical() string {
	return fmt.Sprintf("%s|%s|%s|%s|%d|%d|%d", p.Manifest, p.PlaintextHash, p.Thumbnail, p.Sidecar, p.Width, p.Height, p.TakenAt)
}

// AlbumOp is what an [AlbumChange] does to its album.
type AlbumOp uint8

const (
	// AlbumOpCreate brings the album into existence, optionally with a Title.
	AlbumOpCreate AlbumOp = iota
	// AlbumOpSetTitle sets the album title (LWW).
	AlbumOpSetTitle
	// AlbumOpSetCaption sets the album caption (LWW).
	AlbumOpSetCaption
	// AlbumOpSetCover names the cover photo by manifest id (LWW; empty clears).
	AlbumOpSetCover
	// AlbumOpAddItem adds Item at Pos; the change's own key becomes the item's id.
	AlbumOpAddItem
	// AlbumOpRemoveItem tombstones the item whose add key is Base.
	AlbumOpRemoveItem
	// AlbumOpMoveItem sets the position of the item whose add key is Base to Pos.
	AlbumOpMoveItem
	// AlbumOpDelete tombstones the album and covers every earlier write.
	AlbumOpDelete
)

// AlbumChange is one write to an album, ready to ship and merge. Its exported
// JSON fields ARE the wire contract: the bridge marshals this struct and encrypts
// the bytes (statesync.EncodeChange), so the shape is also the cross-language
// parity contract pinned in testdata/vectors/album.json.
type AlbumChange struct {
	Kind    string         `json:"k,omitempty"` // ADR-0113 kind tag: "album", or absent on a legacy change
	Op      AlbumOp        `json:"op"`
	AlbumID string         `json:"album_id"`
	Item    *PhotoManifest `json:"item,omitempty"`    // AlbumOpAddItem only
	Pos     uint64         `json:"pos,omitempty"`     // AlbumOpAddItem, AlbumOpMoveItem
	Title   string         `json:"title,omitempty"`   // AlbumOpCreate, AlbumOpSetTitle
	Caption string         `json:"caption,omitempty"` // AlbumOpSetCaption only
	Cover   string         `json:"cover,omitempty"`   // AlbumOpSetCover only
	At      uint64         `json:"at"`                // this write's Lamport counter
	Writer  PosTag         `json:"writer"`            // this write's unique tie-break identity
	// Base is the add key of the item this change targets (remove and move); zero
	// for every other op.
	Base WriteKey `json:"base"`
}

// UnmarshalJSON reads the `k` tag before anything else (ADR-0113): a change of
// another kind decodes to just its tag, which [AlbumChange.Foreign] then skips,
// whatever shape the rest of its payload has.
func (c *AlbumChange) UnmarshalJSON(b []byte) error {
	kind, foreign, err := foreignKind(b, KindAlbum)
	if err != nil {
		return err
	}
	if foreign {
		*c = AlbumChange{Kind: kind}
		return nil
	}
	type plain AlbumChange
	return json.Unmarshal(b, (*plain)(c))
}

// Foreign reports whether the change belongs to ANOTHER kind sharing the space
// (ADR-0113): it is tagged with a `k` other than [KindAlbum], or it is untagged and
// names no album, which by shape makes it a legacy change of another kind (a drive
// sync's file write). A fold skips it silently.
func (c AlbumChange) Foreign() bool {
	if c.Kind != "" {
		return c.Kind != KindAlbum
	}
	return c.AlbumID == ""
}

// Validate reports whether the change is well-formed enough to enter the CRDT.
// Checked identically on every replica so the accept / reject decision is a pure
// function of the bytes: a known op, a writer tag, an album id, and only the
// payload fields its op reads — an item carried on a remove, or a cover on an
// add, is malformed, never a real write.
func (c AlbumChange) Validate() error {
	if c.Writer == "" {
		return fmt.Errorf("%w: write has no writer tag", ErrInvalidAlbumChange)
	}
	if c.AlbumID == "" {
		return fmt.Errorf("%w: write has no album id", ErrInvalidAlbumChange)
	}
	switch c.Op {
	case AlbumOpCreate:
		return c.only(fieldTitle)
	case AlbumOpSetTitle:
		return c.only(fieldTitle)
	case AlbumOpSetCaption:
		return c.only(fieldCaption)
	case AlbumOpSetCover:
		if c.Cover != "" {
			if _, err := hashing.Parse(c.Cover); err != nil {
				return fmt.Errorf("%w: cover %q is not a canonical blake3 id: %w", ErrInvalidAlbumChange, c.Cover, err)
			}
		}
		return c.only(fieldCover)
	case AlbumOpAddItem:
		if c.Item == nil {
			return fmt.Errorf("%w: an add must carry an item", ErrInvalidAlbumChange)
		}
		if err := c.Item.Validate(); err != nil {
			return err
		}
		return c.only(fieldItem | fieldPos)
	case AlbumOpRemoveItem:
		if c.Base.isZero() {
			return fmt.Errorf("%w: a remove must name the item it observed", ErrInvalidAlbumChange)
		}
		return c.only(fieldBase)
	case AlbumOpMoveItem:
		if c.Base.isZero() {
			return fmt.Errorf("%w: a move must name the item it observed", ErrInvalidAlbumChange)
		}
		return c.only(fieldBase | fieldPos)
	case AlbumOpDelete:
		return c.only(0)
	default:
		return fmt.Errorf("%w: unknown op %d", ErrInvalidAlbumChange, c.Op)
	}
}

type albumField uint8

const (
	fieldItem albumField = 1 << iota
	fieldPos
	fieldTitle
	fieldCaption
	fieldCover
	fieldBase
)

// only rejects any payload field the op does not read.
func (c AlbumChange) only(allowed albumField) error {
	present := map[albumField]string{}
	if c.Item != nil {
		present[fieldItem] = "item"
	}
	if c.Pos != 0 {
		present[fieldPos] = "pos"
	}
	if c.Title != "" {
		present[fieldTitle] = "title"
	}
	if c.Caption != "" {
		present[fieldCaption] = "caption"
	}
	if c.Cover != "" {
		present[fieldCover] = "cover"
	}
	if !c.Base.isZero() {
		present[fieldBase] = "base"
	}
	for _, f := range []albumField{fieldItem, fieldPos, fieldTitle, fieldCaption, fieldCover, fieldBase} {
		if name, ok := present[f]; ok && allowed&f == 0 {
			return fmt.Errorf("%w: op %d does not carry %s", ErrInvalidAlbumChange, c.Op, name)
		}
	}
	return nil
}

// albumReg is an LWW register with a "was ever written" bit, so a cleared value
// and a never-set value differ in the state (they snapshot differently).
type albumReg struct {
	set   bool
	value string
	key   posKey
}

// join is the register's lattice join: the greater key wins, and a fully tied key
// (hostile; a real UUIDv7 never collides) is broken by the value itself.
func (r albumReg) join(o albumReg) albumReg {
	switch {
	case !o.set:
		return r
	case !r.set:
		return o
	case r.key.greater(o.key):
		return r
	case o.key.greater(r.key):
		return o
	case r.value >= o.value:
		return r
	default:
		return o
	}
}

// albumItem is one add: the photo and the position the add placed it at. Its key
// (the add's own (At, Writer)) is the item's identity and the map key.
type albumItem struct {
	photo PhotoManifest
	pos   uint64
}

// joinItem settles a hostile collision (two adds under one key) deterministically.
func joinItem(a, b albumItem) albumItem {
	ca, cb := a.photo.canonical(), b.photo.canonical()
	if ca != cb {
		if ca < cb {
			return a
		}
		return b
	}
	if a.pos <= b.pos {
		return a
	}
	return b
}

// albumMove is the position register a move writes against an item key.
type albumMove struct {
	pos uint64
	key posKey
}

func (m albumMove) join(o albumMove) albumMove {
	if m.key.greater(o.key) {
		return m
	}
	if o.key.greater(m.key) {
		return o
	}
	if m.pos >= o.pos {
		return m
	}
	return o
}

// albumRecord is everything stored against an album id: the accumulated writes.
// The resolved album is derived from it, never stored, so two converged records
// are equal.
type albumRecord struct {
	createKey posKey
	delKey    posKey
	title     albumReg
	caption   albumReg
	cover     albumReg
	items     map[posKey]albumItem
	removed   map[posKey]struct{}
	moves     map[posKey]albumMove
}

func newAlbumRecord() albumRecord {
	return albumRecord{
		items:   make(map[posKey]albumItem),
		removed: make(map[posKey]struct{}),
		moves:   make(map[posKey]albumMove),
	}
}

// Albums is the materialised album-id -> record map plus the Lamport clock. Every
// field joins by set-union / max, so the whole struct is a semilattice (§43).
type Albums struct {
	albums  map[string]albumRecord
	counter uint64
}

// NewAlbums returns an empty album set.
func NewAlbums() *Albums { return &Albums{albums: make(map[string]albumRecord)} }

// peekAt is the Lamport value the next local write takes — one past the highest
// seen, saturating as everywhere in this package so a poisoned clock stays
// monotonic rather than wrapping to 0 and sorting a fresh write as the oldest.
// It does not advance the clock: applyOne does, when the write is accepted, so a
// rejected local write leaves the counter where it was.
func (a *Albums) peekAt() uint64 {
	if a.counter < math.MaxUint64 {
		return a.counter + 1
	}
	return a.counter
}

func (a *Albums) record(id string) albumRecord {
	rec, ok := a.albums[id]
	if !ok {
		rec = newAlbumRecord()
	}
	return rec
}

func (a *Albums) mint(op AlbumOp, albumID string) AlbumChange {
	return AlbumChange{Kind: KindAlbum, Op: op, AlbumID: albumID, At: a.peekAt(), Writer: newPosTag()}
}

// local validates, applies and returns a locally minted change. A malformed
// change is the "fail at the source" half of the contract — nothing is written
// and the clock does not move; [Albums.Apply] skips the same malformation
// arriving from a peer.
func (a *Albums) local(c AlbumChange) (AlbumChange, error) {
	if err := c.Validate(); err != nil {
		return AlbumChange{}, err
	}
	a.applyOne(c)
	return c, nil
}

// Create brings albumID into existence with an optional title and returns the
// [AlbumChange] to ship. The change is applied locally before it is returned.
func (a *Albums) Create(albumID, title string) (AlbumChange, error) {
	c := a.mint(AlbumOpCreate, albumID)
	c.Title = title
	return a.local(c)
}

// SetTitle sets the album title.
func (a *Albums) SetTitle(albumID, title string) (AlbumChange, error) {
	c := a.mint(AlbumOpSetTitle, albumID)
	c.Title = title
	return a.local(c)
}

// SetCaption sets the album caption.
func (a *Albums) SetCaption(albumID, caption string) (AlbumChange, error) {
	c := a.mint(AlbumOpSetCaption, albumID)
	c.Caption = caption
	return a.local(c)
}

// SetCover names the cover photo by its manifest id; an empty id clears it so
// the resolved cover falls back to the first item.
func (a *Albums) SetCover(albumID, manifest string) (AlbumChange, error) {
	c := a.mint(AlbumOpSetCover, albumID)
	c.Cover = manifest
	return a.local(c)
}

// AddItem appends photo to the album — at [AlbumPosStep] past the last live
// position — and returns the change. The change's (At, Writer) is the item's
// identity for a later [Albums.RemoveItem] or [Albums.MoveItem].
func (a *Albums) AddItem(albumID string, photo PhotoManifest) (AlbumChange, error) {
	var last uint64
	for _, it := range a.record(albumID).liveItems() {
		if it.pos > last {
			last = it.pos
		}
	}
	pos := last + AlbumPosStep
	if pos < last { // saturate rather than wrap to the front
		pos = math.MaxUint64
	}
	return a.AddItemAt(albumID, photo, pos)
}

// AddItemAt adds photo at an explicit position.
func (a *Albums) AddItemAt(albumID string, photo PhotoManifest, pos uint64) (AlbumChange, error) {
	c := a.mint(AlbumOpAddItem, albumID)
	p := photo
	c.Item, c.Pos = &p, pos
	return a.local(c)
}

// RemoveItem tombstones the item whose add key is item.
func (a *Albums) RemoveItem(albumID string, item WriteKey) (AlbumChange, error) {
	c := a.mint(AlbumOpRemoveItem, albumID)
	c.Base = item
	return a.local(c)
}

// MoveItem sets the position of the item whose add key is item.
func (a *Albums) MoveItem(albumID string, item WriteKey, pos uint64) (AlbumChange, error) {
	c := a.mint(AlbumOpMoveItem, albumID)
	c.Base, c.Pos = item, pos
	return a.local(c)
}

// Delete tombstones the album.
func (a *Albums) Delete(albumID string) (AlbumChange, error) {
	return a.local(a.mint(AlbumOpDelete, albumID))
}

// Apply folds changes into the set. It is the CRDT join over writes: idempotent,
// commutative and associative — the result never depends on arrival order (§43).
// A change that fails [AlbumChange.Validate] is SKIPPED, deterministically, so a
// corrupt or hostile peer cannot poison the merge; [Albums.ApplyReport] says which.
func (a *Albums) Apply(changes ...AlbumChange) { a.ApplyReport(changes...) }

// ApplyReport is [Apply] that also returns the changes it skipped as malformed, a
// diagnostic for a caller that wants to surface a peer sending garbage.
//
// A change of another kind sharing the space ([AlbumChange.Foreign]) is skipped
// silently, not reported (ADR-0113).
func (a *Albums) ApplyReport(changes ...AlbumChange) (rejected []AlbumChange) {
	for _, c := range changes {
		if c.Foreign() {
			continue // another kind's change: not ours, and not malformed (ADR-0113)
		}
		if err := c.Validate(); err != nil {
			rejected = append(rejected, c)
			continue
		}
		a.applyOne(c)
	}
	return rejected
}

func (a *Albums) applyOne(c AlbumChange) {
	if c.At > a.counter {
		a.counter = c.At
	}
	rec := a.record(c.AlbumID)
	key := posKey{At: c.At, Writer: c.Writer}
	reg := albumReg{set: true, key: key}
	switch c.Op {
	case AlbumOpCreate:
		if key.greater(rec.createKey) {
			rec.createKey = key
		}
		if c.Title != "" {
			reg.value = c.Title
			rec.title = rec.title.join(reg)
		}
	case AlbumOpSetTitle:
		reg.value = c.Title
		rec.title = rec.title.join(reg)
	case AlbumOpSetCaption:
		reg.value = c.Caption
		rec.caption = rec.caption.join(reg)
	case AlbumOpSetCover:
		reg.value = c.Cover
		rec.cover = rec.cover.join(reg)
	case AlbumOpAddItem:
		it := albumItem{photo: *c.Item, pos: c.Pos}
		if prev, ok := rec.items[key]; ok {
			it = joinItem(prev, it)
		}
		rec.items[key] = it
	case AlbumOpRemoveItem:
		rec.removed[c.Base.key()] = struct{}{}
	case AlbumOpMoveItem:
		mv := albumMove{pos: c.Pos, key: key}
		if prev, ok := rec.moves[c.Base.key()]; ok {
			mv = prev.join(mv)
		}
		rec.moves[c.Base.key()] = mv
	case AlbumOpDelete:
		if key.greater(rec.delKey) {
			rec.delKey = key
		}
	}
	a.albums[c.AlbumID] = rec
}

// joinRecord is the per-album lattice join [MergeAlbums] uses.
func joinRecord(into, from albumRecord) albumRecord {
	if from.createKey.greater(into.createKey) {
		into.createKey = from.createKey
	}
	if from.delKey.greater(into.delKey) {
		into.delKey = from.delKey
	}
	into.title = into.title.join(from.title)
	into.caption = into.caption.join(from.caption)
	into.cover = into.cover.join(from.cover)
	for k, it := range from.items {
		if prev, ok := into.items[k]; ok {
			it = joinItem(prev, it)
		}
		into.items[k] = it
	}
	for k := range from.removed {
		into.removed[k] = struct{}{}
	}
	for k, mv := range from.moves {
		if prev, ok := into.moves[k]; ok {
			mv = prev.join(mv)
		}
		into.moves[k] = mv
	}
	return into
}

// MergeAlbums joins any number of album sets into a NEW set, leaving the inputs
// untouched — per album the union of writes and the maximum of every register,
// and the maximum Lamport counter. Commutative, associative and idempotent, and
// identical to folding the same writes through Apply.
func MergeAlbums(sets ...*Albums) *Albums {
	out := NewAlbums()
	for _, s := range sets {
		if s == nil {
			continue
		}
		for id, rec := range s.albums {
			out.albums[id] = joinRecord(out.record(id), rec)
		}
		if s.counter > out.counter {
			out.counter = s.counter
		}
	}
	return out
}

// live reports whether the album currently exists: created, and not deleted since.
func (r albumRecord) live() bool {
	return !r.createKey.isZero() && r.createKey.greater(r.delKey)
}

// liveItem is one present item with its effective position.
type liveItem struct {
	key   posKey
	pos   uint64
	photo PhotoManifest
}

// liveItems derives the present items from the write set — the adds no remove
// tombstoned and the album's tombstone does not cover, each at the position its
// greatest move set — sorted by (pos, key). Pure over the set.
func (r albumRecord) liveItems() []liveItem {
	out := make([]liveItem, 0, len(r.items))
	for k, it := range r.items {
		if _, gone := r.removed[k]; gone || r.delKey.covers(k) {
			continue
		}
		pos := it.pos
		if mv, ok := r.moves[k]; ok && mv.key.greater(k) {
			pos = mv.pos
		}
		out = append(out, liveItem{key: k, pos: pos, photo: it.photo})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].pos != out[j].pos {
			return out[i].pos < out[j].pos
		}
		return out[j].key.greater(out[i].key)
	})
	return out
}

// value returns a register's value unless the album's tombstone covers the write
// that set it (a re-created album starts with clean registers).
func (r albumRecord) value(reg albumReg) string {
	if !reg.set || r.delKey.covers(reg.key) {
		return ""
	}
	return reg.value
}

// ResolvedItem is one photo in a resolved album: its identity (the add key a
// remove or move targets), its effective position, and the photo.
type ResolvedItem struct {
	Key   WriteKey
	Pos   uint64
	Photo PhotoManifest
}

// ResolvedAlbum is the read a client renders: the album's registers and its
// ordered, deduplicated items.
type ResolvedAlbum struct {
	AlbumID string
	Title   string
	Caption string
	// Cover is the effective cover manifest id: the set cover if a live item still
	// carries that manifest, else the first item's manifest, else empty.
	Cover string
	Items []ResolvedItem
}

// Resolved returns every live album, sorted by album id, as a pure function of
// the converged state — two converged replicas produce identical output. Items
// are in (Pos, At, Writer) order with duplicates by plaintext hash collapsed to
// the first occurrence.
func (a *Albums) Resolved() []ResolvedAlbum {
	out := make([]ResolvedAlbum, 0, len(a.albums))
	for id, rec := range a.albums {
		if !rec.live() {
			continue
		}
		out = append(out, rec.resolve(id))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AlbumID < out[j].AlbumID })
	return out
}

// Album returns one live album's resolved view.
func (a *Albums) Album(albumID string) (ResolvedAlbum, bool) {
	rec, ok := a.albums[albumID]
	if !ok || !rec.live() {
		return ResolvedAlbum{}, false
	}
	return rec.resolve(albumID), true
}

func (r albumRecord) resolve(id string) ResolvedAlbum {
	ra := ResolvedAlbum{AlbumID: id, Title: r.value(r.title), Caption: r.value(r.caption)}
	seen := make(map[string]bool)
	manifests := make(map[string]bool)
	for _, it := range r.liveItems() {
		if seen[it.photo.PlaintextHash] {
			continue
		}
		seen[it.photo.PlaintextHash] = true
		manifests[it.photo.Manifest] = true
		ra.Items = append(ra.Items, ResolvedItem{Key: keyOf(it.key), Pos: it.pos, Photo: it.photo})
	}
	if cover := r.value(r.cover); cover != "" && manifests[cover] {
		ra.Cover = cover
	} else if len(ra.Items) > 0 {
		ra.Cover = ra.Items[0].Photo.Manifest
	}
	return ra
}

// Encode is a canonical, deterministic serialisation of the ENTIRE set: every
// album's lifecycle keys, registers and resolved items, plus the raw write tallies
// so a hidden duplicate or a covered write still shows. Two converged sets produce
// byte-identical output. A test and parity aid, not a wire format.
func (a *Albums) Encode() string {
	var b strings.Builder
	ids := make([]string, 0, len(a.albums))
	for id := range a.albums {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	b.WriteString("albums:\n")
	for _, id := range ids {
		rec := a.albums[id]
		fmt.Fprintf(&b, "  %s created=%s tombstone=%s live=%t writes=%d removed=%d moves=%d\n",
			id, fmtKey(rec.createKey), fmtKey(rec.delKey), rec.live(), len(rec.items), len(rec.removed), len(rec.moves))
		if !rec.live() {
			continue
		}
		ra := rec.resolve(id)
		fmt.Fprintf(&b, "    title=%q caption=%q cover=%s items=%d\n", ra.Title, ra.Caption, ra.Cover, len(ra.Items))
		for i, it := range ra.Items {
			fmt.Fprintf(&b, "    %d. %s hash=%s pos=%d@%d:%s\n", i+1, it.Photo.Manifest, it.Photo.PlaintextHash, it.Pos, it.Key.At, it.Key.Writer)
		}
	}
	fmt.Fprintf(&b, "counter:%d\n", a.counter)
	return b.String()
}
