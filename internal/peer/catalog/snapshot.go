package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"
)

// Kind names how a snapshot payload was produced.
//
// It is on the wire, and on the peer's stored metadata, because a full rebuild
// is the drift corrector: "when did this peer last take the slow, safe path?"
// is an operational question, and it is unanswerable if the two paths are
// indistinguishable after the fact.
const (
	// KindFull carries every covered row. It is the drift corrector — the same
	// discipline M4-07 applies to inventory reporting.
	KindFull = "full"
	// KindIncremental carries the rows that changed at or after a watermark,
	// plus the complete id set of each covered table so that deletions are
	// derivable. See [Snapshot] for why the id sets are there.
	KindIncremental = "incremental"
)

// Meta is what a snapshot knows about itself: where it came from, which one it
// is, and when.
//
// All three fields are load-bearing rather than diagnostic. Without
// ControllerID a snapshot restored from another deployment's backup (§51, §82)
// is indistinguishable from this one's. Without Version there is no way to
// refuse a stale apply. Without GeneratedAt, M7 cannot say how old its answer
// is — and §53's "conservative rather than unavailable" collapses into
// "confident and wrong".
type Meta struct {
	// ControllerID is the peer id of the controller whose catalogue this is.
	ControllerID string `json:"controller_id"`
	// Version increases monotonically for a given peer. It is allocated by the
	// controller, which is the single writer (ADR-0003) and therefore the only
	// place monotonicity can actually be guaranteed.
	Version int64 `json:"version"`
	// GeneratedAt is when the controller READ the catalogue — not when the
	// peer applied it, and not when a row was last written.
	GeneratedAt time.Time `json:"generated_at"`
	// Kind is KindFull or KindIncremental.
	Kind string `json:"kind"`
	// Watermark is the high-water mark the NEXT incremental refresh should ask
	// from. It is a separate field from GeneratedAt because the source
	// deliberately selects rows at or after it: re-sending a row is harmless
	// (every apply is an upsert), dropping one is not.
	Watermark time.Time `json:"watermark"`
}

// Age reports how old the snapshot is at the given instant.
//
// A negative age is possible when the controller's clock is ahead of the
// peer's, and it is returned as measured rather than clamped to zero: an
// operator who sees "-3m" has learned something true about their deployment,
// and an operator who sees "0s" has been told a comfortable lie.
func (m Meta) Age(now time.Time) time.Duration { return now.Sub(m.GeneratedAt) }

// Validate reports whether this metadata could describe a real snapshot.
//
// Version 0 is refused explicitly. "No snapshot" is the absence of a snapshot
// (see [ErrNoSnapshot]) and must never be spellable as a snapshot at version
// zero — the two answers mean different things to M7 and the schema, the wire
// and this check all agree on that.
func (m Meta) Validate() error {
	switch {
	case m.ControllerID == "":
		return errors.New("catalog: a snapshot must name the controller it came from")
	case m.Version <= 0:
		return fmt.Errorf("catalog: a snapshot version must be positive, got %d — "+
			"absent is the absence of a snapshot, never version zero", m.Version)
	case m.GeneratedAt.IsZero():
		return errors.New("catalog: a snapshot must record when it was generated")
	case m.Kind != KindFull && m.Kind != KindIncremental:
		return fmt.Errorf("catalog: a snapshot kind must be %q or %q, got %q", KindFull, KindIncremental, m.Kind)
	}
	return nil
}

// Library is one library, as the snapshot holds it (library organisation).
type Library struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	ContentType string    `json:"content_type"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
}

// LibraryRoot is one root beneath a library.
type LibraryRoot struct {
	ID         string    `json:"id"`
	LibraryID  string    `json:"library_id"`
	Path       string    `json:"path"`
	IngestMode string    `json:"ingest_mode"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
}

// Work is one semantic work (§11).
type Work struct {
	ID          string    `json:"id"`
	ContentType string    `json:"content_type"`
	WorkKey     string    `json:"work_key"`
	Title       string    `json:"title"`
	SortTitle   string    `json:"sort_title"`
	Year        *int64    `json:"year"`
	Attributes  string    `json:"attributes"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Edition is one edition of a work.
type Edition struct {
	ID          string    `json:"id"`
	WorkID      string    `json:"work_id"`
	Label       string    `json:"label"`
	EditionType string    `json:"edition_type"`
	Language    *string   `json:"language"`
	Attributes  string    `json:"attributes"`
	CreatedAt   time.Time `json:"created_at"`
}

// Blob is one byte identity (§13, ADR-0005). The snapshot carries the identity
// and the shape of the bytes, never the bytes: those are the CAS's, which the
// peer already holds in full (§80).
type Blob struct {
	Hash        string    `json:"hash"`
	Size        int64     `json:"size"`
	MIME        *string   `json:"mime"`
	Chunked     bool      `json:"chunked"`
	FirstSeenAt time.Time `json:"first_seen_at"`
}

// Asset is one asset and its blob mapping.
type Asset struct {
	ID                   string     `json:"id"`
	EditionID            string     `json:"edition_id"`
	LibraryID            *string    `json:"library_id"`
	SourceClass          string     `json:"source_class"`
	BlobHash             *string    `json:"blob_hash"`
	SourcePath           *string    `json:"source_path"`
	Fingerprint          *string    `json:"fingerprint"`
	Role                 string     `json:"role"`
	Filename             *string    `json:"filename"`
	MIME                 *string    `json:"mime"`
	IdentificationSource string     `json:"identification_source"`
	MissingSince         *time.Time `json:"missing_since"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

// Snapshot is one payload: metadata, rows, and — for an incremental refresh —
// the complete id set of each covered table.
//
// # Why an incremental payload carries every id
//
// Deletion. A payload of "rows that changed since T" says nothing about rows
// that stopped existing, and a snapshot that silently keeps a deleted work
// forever is not fresh, it is haunted. The alternatives were tombstones in the
// control plane — a new table, a retention policy and a class of bug where the
// tombstone is collected before the last peer has seen it — or shipping every
// id, which is 36 bytes per row against a work row's title, attributes JSON
// and timestamps.
//
// Shipping the ids is the cheap correct answer, and it has a property the
// tombstone design does not: an incremental refresh and a full rebuild of the
// same catalogue state converge on byte-identical contents, because the id set
// makes the incremental path's end state a function of the catalogue rather
// than of the history of refreshes.
type Snapshot struct {
	Meta Meta `json:"meta"`

	Libraries    []Library     `json:"libraries"`
	LibraryRoots []LibraryRoot `json:"library_roots"`
	Works        []Work        `json:"works"`
	Editions     []Edition     `json:"editions"`
	Blobs        []Blob        `json:"blobs"`
	Assets       []Asset       `json:"assets"`

	// IDs is the complete id set per covered table, present only on an
	// incremental payload. A full payload does not need it: its rows ARE the
	// complete set. Keys are the table names used by the snapshot schema.
	IDs map[string][]string `json:"ids,omitempty"`
}

// Rows counts every row carried, across every covered table.
func (s *Snapshot) Rows() int {
	return len(s.Libraries) + len(s.LibraryRoots) + len(s.Works) +
		len(s.Editions) + len(s.Blobs) + len(s.Assets)
}

// Covered names the snapshot's tables in dependency order: a parent before
// every child that references it.
//
// Applying in this order and pruning in reverse is what lets the snapshot
// store keep foreign keys ON. That is not decoration — a snapshot with a
// dangling edition_id is worthless to M7 in exactly the situation M7 exists
// for, and the schema is the half of the check that also holds when a row
// arrives through a repair by hand.
func Covered() []string {
	return []string{
		"snapshot_libraries",
		"snapshot_library_roots",
		"snapshot_works",
		"snapshot_editions",
		"snapshot_blobs",
		"snapshot_assets",
	}
}

// digestWriter canonicalises rows into a hash.
//
// Canonical means: table name, then rows sorted by primary key, then each
// field in a fixed order, each length-prefixed. Length prefixing matters —
// without it the fields "ab","c" and "a","bc" hash identically, and a digest
// that cannot tell those apart is a digest that will one day fail to notice a
// title moving into a sort title.
type digestWriter struct{ h io.Writer }

func (d digestWriter) str(s string) {
	_, _ = io.WriteString(d.h, strconv.Itoa(len(s)))
	_, _ = io.WriteString(d.h, ":")
	_, _ = io.WriteString(d.h, s)
}

func (d digestWriter) ptr(s *string) {
	if s == nil {
		_, _ = io.WriteString(d.h, "~")
		return
	}
	d.str(*s)
}

func (d digestWriter) num(n int64) { d.str(strconv.FormatInt(n, 10)) }

func (d digestWriter) numPtr(n *int64) {
	if n == nil {
		_, _ = io.WriteString(d.h, "~")
		return
	}
	d.num(*n)
}

func (d digestWriter) boolean(b bool) { d.str(strconv.FormatBool(b)) }

func (d digestWriter) stamp(t time.Time) { d.str(t.UTC().Format(time.RFC3339Nano)) }

func (d digestWriter) stampPtr(t *time.Time) {
	if t == nil {
		_, _ = io.WriteString(d.h, "~")
		return
	}
	d.stamp(*t)
}

// ContentDigest fingerprints what a snapshot CONTAINS, deliberately excluding
// its metadata.
//
// Excluding the metadata is the point. "An incremental refresh and a full
// rebuild of the same catalogue state produce identical snapshots" cannot be
// asserted over the whole payload, because the two necessarily have different
// versions and different generation times — the useful and assertable claim is
// that their CONTENTS are identical, and that is what this hashes.
func (s *Snapshot) ContentDigest() string {
	h := sha256.New()
	d := digestWriter{h: h}
	d.str("heyarr-catalog-snapshot-v1")

	libraries := append([]Library(nil), s.Libraries...)
	sort.Slice(libraries, func(i, j int) bool { return libraries[i].ID < libraries[j].ID })
	d.str("libraries")
	for _, l := range libraries {
		d.str(l.ID)
		d.str(l.Name)
		d.str(l.ContentType)
		d.boolean(l.Enabled)
		d.stamp(l.CreatedAt)
	}

	roots := append([]LibraryRoot(nil), s.LibraryRoots...)
	sort.Slice(roots, func(i, j int) bool { return roots[i].ID < roots[j].ID })
	d.str("library_roots")
	for _, r := range roots {
		d.str(r.ID)
		d.str(r.LibraryID)
		d.str(r.Path)
		d.str(r.IngestMode)
		d.boolean(r.Enabled)
		d.stamp(r.CreatedAt)
	}

	works := append([]Work(nil), s.Works...)
	sort.Slice(works, func(i, j int) bool { return works[i].ID < works[j].ID })
	d.str("works")
	for _, w := range works {
		d.str(w.ID)
		d.str(w.ContentType)
		d.str(w.WorkKey)
		d.str(w.Title)
		d.str(w.SortTitle)
		d.numPtr(w.Year)
		d.str(w.Attributes)
		d.stamp(w.CreatedAt)
		d.stamp(w.UpdatedAt)
	}

	editions := append([]Edition(nil), s.Editions...)
	sort.Slice(editions, func(i, j int) bool { return editions[i].ID < editions[j].ID })
	d.str("editions")
	for _, e := range editions {
		d.str(e.ID)
		d.str(e.WorkID)
		d.str(e.Label)
		d.str(e.EditionType)
		d.ptr(e.Language)
		d.str(e.Attributes)
		d.stamp(e.CreatedAt)
	}

	blobs := append([]Blob(nil), s.Blobs...)
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].Hash < blobs[j].Hash })
	d.str("blobs")
	for _, b := range blobs {
		d.str(b.Hash)
		d.num(b.Size)
		d.ptr(b.MIME)
		d.boolean(b.Chunked)
		d.stamp(b.FirstSeenAt)
	}

	assets := append([]Asset(nil), s.Assets...)
	sort.Slice(assets, func(i, j int) bool { return assets[i].ID < assets[j].ID })
	d.str("assets")
	for _, a := range assets {
		d.str(a.ID)
		d.str(a.EditionID)
		d.ptr(a.LibraryID)
		d.str(a.SourceClass)
		d.ptr(a.BlobHash)
		d.ptr(a.SourcePath)
		d.ptr(a.Fingerprint)
		d.str(a.Role)
		d.ptr(a.Filename)
		d.ptr(a.MIME)
		d.str(a.IdentificationSource)
		d.stampPtr(a.MissingSince)
		d.stamp(a.CreatedAt)
		d.stamp(a.UpdatedAt)
	}

	return hex.EncodeToString(h.Sum(nil))
}
