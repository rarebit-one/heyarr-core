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

// FindingKind classifies what a check found.
type FindingKind string

// The kinds of finding a check can produce.
const (
	// KindMissing is a catalog row whose bytes are not in the store.
	KindMissing FindingKind = "missing"
	// KindSizeMismatch is a file whose length is not what the catalog recorded.
	// It is damage on its own evidence: the bytes cannot be the bytes.
	KindSizeMismatch FindingKind = "size_mismatch"
	// KindCorrupt is a file that no longer hashes to its own name. The bytes
	// are quarantined, never deleted (ADR-0018).
	KindCorrupt FindingKind = "corrupt"
	// KindUntracked is bytes in the store with no catalog row. Not damage:
	// it is what an ingest fault between the CAS write and the commit leaves,
	// and garbage collection reclaims it.
	KindUntracked FindingKind = "untracked"
	// KindOrphanTemp is a partial write left by an interrupted Put or Link.
	KindOrphanTemp FindingKind = "orphan_temp"
)

// Damage reports whether this kind means something was lost.
//
// The distinction is the whole value of the exit code. Untracked bytes and
// partial writes are waste, and waste is not an emergency; a missing or corrupt
// blob is content the operator believed they had. Reporting them at the same
// severity trains people to ignore both.
func (k FindingKind) Damage() bool {
	switch k {
	case KindMissing, KindSizeMismatch, KindCorrupt:
		return true
	case KindUntracked, KindOrphanTemp:
		return false
	default:
		return false
	}
}

// Finding is one problem a check found.
type Finding struct {
	Kind FindingKind `json:"kind"`
	// Hash is the blob concerned, in canonical form. Empty for a temp file,
	// which by definition never became addressable.
	Hash string `json:"hash,omitempty"`
	// ActualHash is what the bytes hash to now, on a corrupt finding.
	ActualHash   string `json:"actual_hash,omitempty"`
	ExpectedSize int64  `json:"expected_size,omitempty"`
	ActualSize   int64  `json:"actual_size,omitempty"`
	// Path is where the evidence is: the quarantine destination for a corrupt
	// blob, the temp file's name for an orphaned partial write.
	Path        string `json:"path,omitempty"`
	Quarantined bool   `json:"quarantined,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// Report is the outcome of a check.
type Report struct {
	Deep bool `json:"deep"`
	// BlobsInCatalog is how many blobs the catalog claimed to have.
	BlobsInCatalog int `json:"blobs_in_catalog"`
	// BlobsChecked is how many were actually examined. It exists so that
	// "no problems found" can be told apart from "nothing was looked at" —
	// a checker that examined nothing and reported success is the failure
	// mode this whole command exists to avoid.
	BlobsChecked int `json:"blobs_checked"`
	// BytesRead is how many bytes were re-hashed. Zero on a shallow check.
	BytesRead int64 `json:"bytes_read"`
	// FilesInStore is how many blob files the store holds.
	FilesInStore int       `json:"files_in_store"`
	Findings     []Finding `json:"findings"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
}

// Damage counts findings that mean content was lost.
func (r Report) Damage() int {
	var n int
	for _, f := range r.Findings {
		if f.Kind.Damage() {
			n++
		}
	}
	return n
}

// Reclaimable counts findings garbage collection would clean up.
func (r Report) Reclaimable() int { return len(r.Findings) - r.Damage() }

// Checker reconciles expected hashes against verified bytes (§57).
type Checker struct {
	opts Options
}

// NewChecker constructs a Checker.
func NewChecker(opts Options) (*Checker, error) {
	resolved, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	return &Checker{opts: resolved}, nil
}

// CheckOptions select how hard a check works.
type CheckOptions struct {
	// Deep re-hashes every blob. Shallow checks existence and length only,
	// which is seconds against a library where deep is hours — and which
	// catches a truncation or a deletion but not a rewrite in place.
	Deep bool
}

// Check sweeps the store against the catalog and reports what it found.
//
// The order is deliberate: catalog first, because a blob the catalog claims and
// the store does not have is content the operator has lost; store second,
// because bytes with no row are only waste.
func (c *Checker) Check(ctx context.Context, opts CheckOptions) (Report, error) {
	// Empty rather than nil, so the JSON shape does not change with the result.
	report := Report{Deep: opts.Deep, StartedAt: c.opts.Clock.Now(), Findings: []Finding{}}

	blobs, err := c.opts.Catalog.Blobs(ctx)
	if err != nil {
		return Report{}, err
	}
	report.BlobsInCatalog = len(blobs)

	known := make(map[string]struct{}, len(blobs))
	for _, b := range blobs {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		known[b.Hash.String()] = struct{}{}

		finding, read, err := c.checkOne(ctx, b, opts.Deep)
		if err != nil {
			return Report{}, err
		}
		report.BlobsChecked++
		report.BytesRead += read
		if finding != nil {
			report.Findings = append(report.Findings, *finding)
		}
	}

	if err := c.opts.Store.Walk(ctx, func(d cas.Descriptor) error {
		report.FilesInStore++
		if _, ok := known[d.Hash.String()]; ok {
			return nil
		}
		report.Findings = append(report.Findings, Finding{
			Kind:       KindUntracked,
			Hash:       d.Hash.String(),
			ActualSize: d.Size,
			Detail:     "bytes in the store with no catalog row — garbage collection reclaims these",
		})
		return nil
	}); err != nil {
		return Report{}, err
	}

	reaper, ok := c.opts.Store.(TempReaper)
	if ok {
		temps, err := reaper.TempFiles()
		if err != nil {
			return Report{}, err
		}
		for _, t := range temps {
			report.Findings = append(report.Findings, Finding{
				Kind:       KindOrphanTemp,
				Path:       t.Name,
				ActualSize: t.Size,
				Detail:     "a partial write left by an interrupted ingest",
			})
		}
	}

	// Stable order, so two runs over an unchanged library produce the same
	// report and a diff between them means something actually changed.
	sort.SliceStable(report.Findings, func(i, j int) bool {
		if report.Findings[i].Kind != report.Findings[j].Kind {
			return report.Findings[i].Kind < report.Findings[j].Kind
		}
		if report.Findings[i].Hash != report.Findings[j].Hash {
			return report.Findings[i].Hash < report.Findings[j].Hash
		}
		return report.Findings[i].Path < report.Findings[j].Path
	})
	report.FinishedAt = c.opts.Clock.Now()
	return report, nil
}

// checkOne examines a single catalog blob, returning the finding it produced
// and how many bytes it read.
func (c *Checker) checkOne(ctx context.Context, b Blob, deep bool) (*Finding, int64, error) {
	desc, err := c.opts.Store.Stat(ctx, b.Hash)
	switch {
	case errors.Is(err, cas.ErrNotFound):
		if err := c.opts.Catalog.MarkMissing(ctx, b.Hash, c.opts.Clock.Now()); err != nil {
			return nil, 0, err
		}
		return &Finding{
			Kind:         KindMissing,
			Hash:         b.Hash.String(),
			ExpectedSize: b.Size,
			Detail:       "the catalog has this blob but the store does not",
		}, 0, nil
	case err != nil:
		return nil, 0, err
	}

	// A length that disagrees with the catalog is already proof the bytes are
	// wrong, so escalate straight to a full verification even on a shallow
	// sweep: it costs one read of one known-bad blob, and it is what produces
	// the quarantined evidence and the digest that says how it is wrong.
	// Reporting "wrong size, go and look" instead would be a checker that finds
	// corruption and then leaves the corrupt bytes addressable.
	if desc.Size != b.Size {
		finding, read, err := c.verify(ctx, b)
		if err != nil {
			return nil, read, err
		}
		if finding == nil {
			// Verified clean at the wrong length: the catalog row is what is
			// wrong, not the bytes. Report it rather than silently agreeing.
			return &Finding{
				Kind:         KindSizeMismatch,
				Hash:         b.Hash.String(),
				ExpectedSize: b.Size,
				ActualSize:   desc.Size,
				Detail:       "the bytes verify but the catalog records a different size",
			}, read, nil
		}
		finding.ExpectedSize = b.Size
		finding.ActualSize = desc.Size
		return finding, read, nil
	}

	if !deep {
		return nil, 0, nil
	}
	return c.verify(ctx, b)
}

// verify re-hashes one blob, recording the outcome either way.
func (c *Checker) verify(ctx context.Context, b Blob) (*Finding, int64, error) {
	now := c.opts.Clock.Now()
	err := c.opts.Store.Verify(ctx, b.Hash)

	var corrupt *cas.Corruption
	switch {
	case err == nil:
		if err := c.opts.Catalog.MarkVerified(ctx, b.Hash, now); err != nil {
			return nil, 0, err
		}
		return nil, b.Size, nil

	case errors.As(err, &corrupt):
		record := Corruption{
			Hash:   corrupt.Hash,
			Actual: corrupt.Actual,
			Size:   corrupt.Size,
			Path:   corrupt.Path,
			Detail: "hash mismatch on verification",
		}
		if err := c.opts.Catalog.MarkCorrupt(ctx, record, now); err != nil {
			return nil, corrupt.Size, err
		}
		return &Finding{
			Kind:        KindCorrupt,
			Hash:        b.Hash.String(),
			ActualHash:  corrupt.Actual.String(),
			ActualSize:  corrupt.Size,
			Path:        corrupt.Path,
			Quarantined: corrupt.Path != "",
			Detail:      "stored bytes no longer hash to their own name",
		}, corrupt.Size, nil

	case errors.Is(err, cas.ErrNotFound):
		// It existed a moment ago. Something is deleting under us, which is
		// worth reporting as loss rather than swallowing.
		if err := c.opts.Catalog.MarkMissing(ctx, b.Hash, now); err != nil {
			return nil, 0, err
		}
		return &Finding{
			Kind:         KindMissing,
			Hash:         b.Hash.String(),
			ExpectedSize: b.Size,
			Detail:       "the blob disappeared during the check",
		}, 0, nil

	case errors.Is(err, cas.ErrCorrupt):
		// Corrupt, but quarantining failed, so there is no path to record.
		// Still corruption, and still not deleted.
		if err := c.opts.Catalog.MarkCorrupt(ctx, Corruption{
			Hash: b.Hash, Size: b.Size, Detail: err.Error(),
		}, now); err != nil {
			return nil, 0, err
		}
		return &Finding{
			Kind:         KindCorrupt,
			Hash:         b.Hash.String(),
			ExpectedSize: b.Size,
			Quarantined:  false,
			Detail:       err.Error(),
		}, 0, nil

	default:
		// An I/O error is not corruption. Treating a flaky disk as a hash
		// mismatch is how a healthy replica gets quarantined and, one milestone
		// later, how the last good copy gets thrown away.
		return nil, 0, fmt.Errorf("integrity: verifying %s: %w", b.Hash, err)
	}
}

// VerifyBlob re-hashes one blob and records the outcome. It is what the
// verify_blob job runs.
//
// Re-running it is harmless by construction: verifying clean bytes stamps a
// timestamp, and verifying bytes that are already quarantined finds them
// missing and says so (ADR-0008).
func (c *Checker) VerifyBlob(ctx context.Context, h hashing.Hash) (Finding, error) {
	b, err := c.opts.Catalog.Blob(ctx, h)
	if err != nil {
		return Finding{}, err
	}
	finding, _, err := c.checkOne(ctx, b, true)
	if err != nil {
		return Finding{}, err
	}
	if finding == nil {
		return Finding{}, nil
	}
	return *finding, nil
}
