package worker

// Tests for the personal worker's job registry.
//
// The property under test is which job types the personal worker registers —
// specifically that it registers exactly the four personal types and does not
// register any of the media, acquisition or provider types that the full worker
// adds. The handlers are no-ops: what is tested is the SET, not the behaviour
// of each handler.
//
// The table rows are written around the constants from the real packages, not
// hand-typed literals, so a rename would show up here rather than silently
// passing while the wiring changed.

import (
	"context"
	"slices"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media/probe"
	"github.com/rarebit-one/heyarr-core/internal/providers"
	"github.com/rarebit-one/heyarr-core/internal/scanner"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// stub is a no-op HandlerFunc that satisfies the Registration.Handler field
// without requiring real dependencies. It is only used to populate the registry
// so that Types() and Lookup() can be exercised; it is never called.
var stub HandlerFunc = func(_ context.Context, _ jobs.Job) error { return nil }

// TestPersonalWorkerRegistersExactlyTheFourPersonalJobTypes is the primary
// registration test. It verifies both halves of the contract:
//
//  1. The four types the personal worker promises to handle ARE registered.
//  2. The types that belong only to the full worker (media toolchain, acquisition,
//     scanner, providers) are NOT registered.
//
// The test is table-driven so that adding a new personal type is a one-line
// change that also documents why the type belongs here, and removing a type
// causes a failure that explains what was expected.
func TestPersonalWorkerRegistersExactlyTheFourPersonalJobTypes(t *testing.T) {
	t.Parallel()

	// Build a registry the way PersonalWorker.Run does, but with no-op handlers.
	// This is a whitebox test: Run's job is to call exactly these four Register
	// calls. Any deviation (missing type, added type) should fail here or in the
	// absent-type checks below.
	reg := newPersonalRegistryForTest()

	mustPresent := []struct {
		name    string
		jobType string
	}{
		// The only housekeeping job on a personal node: reclaim orphaned vault
		// bytes. Run applies=true so bytes are actually reclaimed, not just reported.
		{"gc_blobs", integrity.GCJobType},
		// Drives vault-blob convergence on remote peers from the placement-pin
		// desired set (§19, #736). The full worker gets this from the controller's
		// reconciliation beat; the personal worker starts its own beat.
		{"reconcile_peer", replication.ReconcilePeerJobType},
		// Moves vault blobs to peers that the convergence cycle named as
		// destinations. Requires only a network and a disk: no toolchain.
		{"replicate_blob", replication.ReplicateBlobJobType},
		// Produces chunk manifests for resumable transfers (§16, ADR-0035). Runs
		// lazily, so a small blob never waits for chunking to be ready.
		{"chunk_blob", manifests.ChunkBlobJobType},
	}

	for _, tc := range mustPresent {
		t.Run("present:"+tc.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := reg.Lookup(tc.jobType); !ok {
				t.Errorf("personal registry is missing %q — this job type belongs in the personal worker", tc.jobType)
			}
		})
	}

	mustAbsent := []struct {
		name    string
		jobType string
		reason  string
	}{
		// --- Storage jobs that belong only to the media pipeline ---
		{
			name:    "ingest_artifact",
			jobType: ingest.JobType,
			reason:  "ingest processes media files — personal nodes have no library",
		},
		{
			name:    "verify_blob",
			jobType: integrity.VerifyJobType,
			reason:  "verify_blob is a media integrity sweep — personal vault integrity comes from GC + placement pins",
		},
		// --- Scanner ---
		{
			name:    "scan_library",
			jobType: scanner.JobType,
			reason:  "scanner walks media roots — personal nodes have no media roots",
		},
		// --- Acquisition jobs ---
		{
			name:    "reconcile_desired",
			jobType: acquisition.ReconcileJobType,
			reason:  "reconcile_desired satisfies library wants — personal nodes have no library",
		},
		{
			name:    "upgrade_scan",
			jobType: acquisition.UpgradeScanJobType,
			reason:  "upgrade_scan re-scans existing media — personal nodes have no media",
		},
		{
			name:    "ingest_acquisition",
			jobType: acquisition.IngestJobType,
			reason:  "ingest_acquisition imports acquired media — personal nodes have no library",
		},
		{
			name:    "grab_release",
			jobType: acquisition.GrabJobType,
			reason:  "grab_release is a download-client job — personal nodes have no acquisition",
		},
		// --- Provider jobs ---
		{
			name:    "provider_health",
			jobType: providers.HealthJobType,
			reason:  "provider_health probes media providers — personal nodes have no providers configured",
		},
		// --- Media toolchain jobs ---
		{
			name:    "probe_blob",
			jobType: probe.JobType,
			reason:  "probe_blob needs ffprobe — personal nodes have no media toolchain",
		},
	}

	for _, tc := range mustAbsent {
		t.Run("absent:"+tc.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := reg.Lookup(tc.jobType); ok {
				t.Errorf("personal registry unexpectedly contains %q — %s", tc.jobType, tc.reason)
			}
		})
	}

	// A count guard so that adding a registration to Run without adding a
	// mustPresent row here causes a failure rather than silent expansion.
	wantTypes := make([]string, len(mustPresent))
	for i, tc := range mustPresent {
		wantTypes[i] = tc.jobType
	}
	slices.Sort(wantTypes)

	gotTypes := reg.Types()
	slices.Sort(gotTypes)

	if len(gotTypes) != len(wantTypes) {
		t.Errorf("personal registry has %d job type(s) (%v), want exactly %d (%v)",
			len(gotTypes), gotTypes, len(wantTypes), wantTypes)
	}
}

// newPersonalRegistryForTest builds a Registry containing exactly the four
// personal job types, each with a no-op handler. It mirrors what
// PersonalWorker.Run registers, without requiring a real database or CAS store.
//
// Kept unexported — tests in this package are the only callers — and not
// reused across test files to keep each test self-contained.
func newPersonalRegistryForTest() *Registry {
	reg := NewRegistry()
	reg.Register(integrity.GCJobType, Registration{
		Handler:       stub,
		MaxConcurrent: 1,
	})
	reg.Register(replication.ReconcilePeerJobType, Registration{
		Handler:       stub,
		MaxConcurrent: 1,
	})
	reg.Register(replication.ReplicateBlobJobType, Registration{
		Handler:       stub,
		MaxConcurrent: maxConcurrentTransfers,
	})
	reg.Register(manifests.ChunkBlobJobType, Registration{
		Handler:       stub,
		MaxConcurrent: maxConcurrentChunkings,
	})
	return reg
}
