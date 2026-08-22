package integrity

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// Grace window defaults.
const (
	// DefaultGrace is how long a blob must have been unreferenced before its
	// bytes may be reclaimed. A week is chosen so that a mistake made on a
	// Friday is still reversible on the following Monday — ADR-0018's whole
	// argument is that the window is what makes a wrong delete survivable.
	DefaultGrace = 7 * 24 * time.Hour
	// DefaultTempGrace is how old a partial write must be before it is swept.
	// It only has to outlast the longest plausible in-flight ingest, and an
	// interrupted 60 GB hash is hours, not days.
	DefaultTempGrace = 24 * time.Hour
)

// Window is a grace window. It marshals as the duration string an operator
// typed ("168h0m0s") rather than as a count of nanoseconds, because the JSON
// output of an administrative command is read by people at least as often as by
// scripts.
type Window time.Duration

// MarshalJSON implements json.Marshaler.
func (w Window) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Duration(w).String() + `"`), nil
}

// String implements fmt.Stringer.
func (w Window) String() string { return time.Duration(w).String() }

// Candidate is a blob a sweep considered.
type Candidate struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
	// UnreferencedSince is when the blob was first observed with no references.
	UnreferencedSince time.Time `json:"unreferenced_since,omitzero"`
	// EligibleAt is when its grace window expires.
	EligibleAt time.Time `json:"eligible_at,omitzero"`
}

// TempRemoval is a partial write a sweep swept.
type TempRemoval struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Collection is the outcome of a sweep.
type Collection struct {
	// DryRun records whether anything was actually removed. It is first
	// because it is the field that decides how to read every other one.
	DryRun bool   `json:"dry_run"`
	Grace  Window `json:"grace"`
	// Considered is how many catalog blobs the sweep examined.
	//
	// It exists to make "garbage collection removed nothing" falsifiable. That
	// assertion passes trivially against a collector that never ran, never
	// loaded anything, or silently failed — which is the shape of vacuous test
	// this repo has already caught three times. A sweep that reclaimed nothing
	// having considered nothing is not the same result.
	Considered int `json:"considered"`
	// Referenced is how many were held by at least one asset.
	Referenced int `json:"referenced"`
	// Marked are blobs whose grace window started on this pass. They are never
	// reclaimed by the same pass that marks them.
	Marked []Candidate `json:"marked"`
	// Waiting are unreferenced blobs still inside their window.
	Waiting []Candidate `json:"waiting"`
	// Reclaimed are blobs whose bytes were freed, or would have been.
	Reclaimed []Candidate `json:"reclaimed"`
	// Spared are blobs the durability precondition declined to reclaim, each
	// with the reason it declined (ADR-0018, M4-12).
	//
	// It sits next to Reclaimed rather than in a log line because those two
	// slices are the sweep's actual output: an operator who ran `gc --apply`
	// and got nothing back needs to know whether that is a healthy library or
	// a peer that has been down since Tuesday, and a refusal nobody can read
	// is an outage nobody can diagnose.
	Spared []Sparing `json:"spared"`
	// Refusals are conditions that stopped the WHOLE sweep rather than one
	// blob — a controller that could not be reached (§53), a collector with no
	// way to ask a peer anything, a catalog too empty to be trusted against
	// the store in front of it.
	Refusals []SweepRefusal `json:"refusals"`
	// Untracked are store files with no catalog row that were old enough to
	// remove.
	Untracked []Candidate `json:"untracked"`
	// UntrackedSpared are untracked files the non-vacuity guard declined to
	// unlink. They are reported per file for the same reason Spared is.
	UntrackedSpared []Sparing `json:"untracked_spared"`
	// UntrackedWaiting counts untracked files too recent to touch — most
	// likely an ingest that has written its bytes and not yet committed.
	UntrackedWaiting int           `json:"untracked_waiting"`
	TempRemoved      []TempRemoval `json:"temp_removed"`
	BytesReclaimed   int64         `json:"bytes_reclaimed"`
	StartedAt        time.Time     `json:"started_at"`
	FinishedAt       time.Time     `json:"finished_at"`
}

// Collector reclaims bytes nothing references (ADR-0018).
type Collector struct {
	opts Options
}

// NewCollector constructs a Collector.
func NewCollector(opts Options) (*Collector, error) {
	resolved, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	return &Collector{opts: resolved}, nil
}

// CollectOptions configure one sweep.
type CollectOptions struct {
	// Apply reclaims. Its zero value does not, which is the point: ADR-0018
	// makes garbage collection dry-run by default, and a struct literal that
	// forgot a field must not be the one that deletes a library.
	Apply bool
	// Grace overrides DefaultGrace. Zero means the default; a negative value
	// is refused rather than treated as "immediately".
	Grace time.Duration
	// TempGrace overrides DefaultTempGrace.
	TempGrace time.Duration
}

// Collect sweeps unreferenced bytes.
//
// It is mark-and-sweep rather than delete-on-sight, and the mark is persisted:
// one pass observes that a blob has no references and records when, a later
// pass past the grace window reclaims it. Two passes are the price of the
// window meaning anything — nothing records the moment an asset stopped
// pointing at a blob, so the only honest start for the clock is when Heyarr
// first noticed.
//
// Re-running it converges rather than accumulating (ADR-0008): marking is
// idempotent, and a blob that regains a reference has its mark cleared and
// starts a fresh window.
func (c *Collector) Collect(ctx context.Context, opts CollectOptions) (Collection, error) {
	grace := opts.Grace
	switch {
	case grace == 0:
		grace = DefaultGrace
	case grace < 0:
		return Collection{}, fmt.Errorf("integrity: a negative grace window (%s) would reclaim bytes "+
			"before they were unreferenced; pass a small positive duration if you mean "+
			"\"reclaim without waiting\"", grace)
	}
	tempGrace := opts.TempGrace
	if tempGrace == 0 {
		tempGrace = DefaultTempGrace
	}
	if tempGrace < 0 {
		return Collection{}, fmt.Errorf("integrity: a negative temporary-file grace window (%s) is not meaningful", tempGrace)
	}

	now := c.opts.Clock.Now()
	// Empty slices rather than nil, so the JSON shape is stable: a consumer
	// that has to handle both [] and null for the same field will eventually
	// handle only one of them.
	out := Collection{
		DryRun: !opts.Apply, Grace: Window(grace), StartedAt: now,
		Marked: []Candidate{}, Waiting: []Candidate{}, Reclaimed: []Candidate{},
		Untracked: []Candidate{}, TempRemoved: []TempRemoval{},
		Spared: []Sparing{}, Refusals: []SweepRefusal{}, UntrackedSpared: []Sparing{},
	}
	cutoff := now.Add(-grace)

	blobs, err := c.opts.Catalog.Blobs(ctx)
	if err != nil {
		return Collection{}, err
	}
	out.Considered = len(blobs)

	known := make(map[string]struct{}, len(blobs))
	var toMark, toClear []hashing.Hash
	var eligible []Blob

	for _, b := range blobs {
		known[b.Hash.String()] = struct{}{}
		if b.References > 0 {
			out.Referenced++
			if !b.UnreferencedSince.IsZero() {
				toClear = append(toClear, b.Hash)
			}
			continue
		}
		switch {
		case b.UnreferencedSince.IsZero():
			toMark = append(toMark, b.Hash)
			out.Marked = append(out.Marked, candidate(b, now, grace))
		case b.UnreferencedSince.After(cutoff):
			out.Waiting = append(out.Waiting, candidate(b, b.UnreferencedSince, grace))
		default:
			eligible = append(eligible, b)
		}
	}

	if opts.Apply {
		if err := c.opts.Catalog.ClearUnreferenced(ctx, toClear); err != nil {
			return Collection{}, err
		}
		if err := c.opts.Catalog.MarkUnreferenced(ctx, toMark, now); err != nil {
			return Collection{}, err
		}
	}

	// ADR-0018's second precondition, evaluated between eligibility and the
	// unlink (M4-12). Everything above this point decided that these bytes are
	// garbage HERE; nothing above it knows whether they exist anywhere else,
	// and a full peer that garbage-collects the only surviving copy has failed
	// at the one thing it exists to do.
	gate, err := c.gate(ctx)
	if err != nil {
		return Collection{}, err
	}
	out.Refusals = append(out.Refusals, gate.refusals...)

	for _, b := range eligible {
		if err := ctx.Err(); err != nil {
			return Collection{}, err
		}
		ev, spared, err := c.establish(ctx, gate, b, now, opts.Apply)
		if err != nil {
			return Collection{}, err
		}
		if spared != nil {
			out.Spared = append(out.Spared, *spared)
			continue
		}
		if opts.Apply {
			// Before the delete, not after. Catalog.Reclaim removes the
			// `blobs` row and replicas.blob_hash is ON DELETE CASCADE, so the
			// transaction that authorises this unlink also destroys the record
			// of who else held the blob. Evidence written afterwards would
			// have nothing left to describe (migration 00028).
			if err := c.opts.Catalog.RecordDurability(ctx, ev); err != nil {
				return Collection{}, err
			}
			if err := c.reclaim(ctx, b.Hash, b.Size, true, now); err != nil {
				return Collection{}, err
			}
		}
		out.Reclaimed = append(out.Reclaimed, candidate(b, b.UnreferencedSince, grace))
		out.BytesReclaimed += b.Size
	}

	if err := c.collectUntracked(ctx, &out, known, cutoff, now, opts.Apply, gate); err != nil {
		return Collection{}, err
	}
	if err := c.collectTemp(&out, now.Add(-tempGrace), opts.Apply); err != nil {
		return Collection{}, err
	}

	out.FinishedAt = c.opts.Clock.Now()
	c.opts.Logger.Info("garbage collection swept",
		"dry_run", out.DryRun, "considered", out.Considered, "referenced", out.Referenced,
		"marked", len(out.Marked), "waiting", len(out.Waiting), "reclaimed", len(out.Reclaimed),
		"untracked", len(out.Untracked), "temp_removed", len(out.TempRemoved),
		"spared", len(out.Spared), "untracked_spared", len(out.UntrackedSpared),
		"refusals", len(out.Refusals), "bytes_reclaimed", out.BytesReclaimed)
	return out, nil
}

// reclaim removes the catalog row first and the bytes second.
//
// The order is the recoverable one. Row first, bytes second, and a crash
// between them leaves untracked bytes that the next sweep reclaims. Bytes
// first, row second, and the same crash leaves the catalog pointing at content
// that no longer exists — which is indistinguishable from real data loss, and
// which fsck will correctly report as damage.
func (c *Collector) reclaim(ctx context.Context, h hashing.Hash, size int64, tracked bool, at time.Time) error {
	if tracked {
		if err := c.opts.Catalog.Reclaim(ctx, h, size, true, at); err != nil {
			return err
		}
	}
	if err := c.opts.Store.Delete(ctx, h); err != nil && !errors.Is(err, cas.ErrNotFound) {
		return err
	}
	if !tracked {
		return c.opts.Catalog.Reclaim(ctx, h, size, false, at)
	}
	return nil
}

// collectUntracked reclaims bytes with no catalog row — the orphan an ingest
// fault between the CAS write and the commit leaves behind (M1-10).
//
// The age check is not tidiness. Ingest writes bytes and then commits, so
// between those two moments a perfectly healthy ingest looks exactly like an
// orphan. The listing this sweep started from is also a snapshot, so anything
// old enough to remove is re-checked against the catalog immediately before
// unlinking. Neither closes the race completely — that needs a lease, which
// arrives with Milestone 4's placement preconditions — but together they make
// it require a commit inside the final round trip rather than anywhere in a
// multi-minute sweep.
func (c *Collector) collectUntracked(ctx context.Context, out *Collection,
	known map[string]struct{}, cutoff, now time.Time, apply bool, g gate,
) error {
	var (
		candidates []cas.Descriptor
		walked     int
	)
	if err := c.opts.Store.Walk(ctx, func(d cas.Descriptor) error {
		walked++
		if _, ok := known[d.Hash.String()]; ok {
			return nil
		}
		if !d.ModTime.Before(cutoff) {
			out.UntrackedWaiting++
			return nil
		}
		candidates = append(candidates, d)
		return nil
	}); err != nil {
		return err
	}
	if len(candidates) == 0 {
		return nil
	}

	// The non-vacuity guard, and the sweep-wide refusals, both apply here
	// before a single file is unlinked. Untracked collection is the more
	// dangerous of the two paths, not the less: a tracked blob is protected by
	// a refcount, a persisted mark, a grace window and ON DELETE RESTRICT,
	// whereas an untracked one is protected by the absence of a row — and
	// absence is what a wrong or empty database produces in bulk.
	if refusal := c.vacuity(out, len(candidates), walked); refusal != nil {
		out.Refusals = append(out.Refusals, *refusal)
		spareAll(out, candidates, *refusal)
		return nil
	}
	if len(g.refusals) > 0 {
		spareAll(out, candidates, g.refusals[0])
		return nil
	}

	hashes := make([]hashing.Hash, 0, len(candidates))
	for _, d := range candidates {
		hashes = append(hashes, d.Hash)
	}
	appeared, err := c.opts.Catalog.Known(ctx, hashes)
	if err != nil {
		return err
	}

	for _, d := range candidates {
		if appeared[d.Hash.String()] {
			// A row arrived while we were walking. These bytes are somebody's
			// content now.
			out.UntrackedWaiting++
			continue
		}
		if apply {
			if err := c.reclaim(ctx, d.Hash, d.Size, false, now); err != nil {
				return err
			}
		}
		out.Untracked = append(out.Untracked, Candidate{Hash: d.Hash.String(), Size: d.Size})
		out.BytesReclaimed += d.Size
	}
	return nil
}

func (c *Collector) collectTemp(out *Collection, cutoff time.Time, apply bool) error {
	reaper, ok := c.opts.Store.(TempReaper)
	if !ok {
		return nil
	}
	files, err := reaper.TempFiles()
	if err != nil {
		return err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	for _, f := range files {
		if !f.ModTime.Before(cutoff) {
			continue
		}
		if apply {
			if err := reaper.RemoveTemp(f.Name); err != nil {
				return err
			}
		}
		out.TempRemoved = append(out.TempRemoved, TempRemoval{Name: f.Name, Size: f.Size})
		out.BytesReclaimed += f.Size
	}
	return nil
}

func candidate(b Blob, since time.Time, grace time.Duration) Candidate {
	c := Candidate{Hash: b.Hash.String(), Size: b.Size}
	if !since.IsZero() {
		c.UnreferencedSince = since.UTC()
		c.EligibleAt = since.Add(grace).UTC()
	}
	return c
}
