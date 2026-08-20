package worker

import (
	"context"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media"
)

// Capability routing has been in the queue and the runtime since M1-05 and
// M1-09, and until Milestone 2 nothing advertised a capability: the worker
// built its runtime with an empty set, so no job could ever require one. These
// tests cover the join — that what a worker advertises is what it actually
// resolved — because each half being correct in isolation is exactly how a
// wiring bug survives.

// A node with no toolchain advertises nothing and leaves the jobs that need it
// alone. This is ADR-0023's degrade path as a whole rather than as two halves.
func TestANodeWithNoToolchainAdvertisesNothingAndClaimsNothingThatNeedsIt(t *testing.T) {
	toolchain, err := media.Resolve(t.Context(), media.NoToolchain())
	if err != nil {
		t.Fatalf("resolving on a bare node failed, which would make FFmpeg mandatory: %v", err)
	}
	if got := toolchain.Capabilities(); len(got) != 0 {
		t.Fatalf("a bare node advertises %v", got)
	}

	q := newFakeQueue(
		jobs.Job{ID: "p1", Type: "probe", RequiredCapability: media.CapabilityFFprobe},
		jobs.Job{ID: "t1", Type: "transcode", RequiredCapability: media.CapabilityFFmpeg},
		jobs.Job{ID: "h1", Type: "hash_blob"},
	)
	reg := NewRegistry()
	reg.Register("probe", Registration{
		RequiredCapability: media.CapabilityFFprobe,
		Handler:            HandlerFunc(func(context.Context, jobs.Job) error { return nil }),
	})
	reg.Register("transcode", Registration{
		RequiredCapability: media.CapabilityFFmpeg,
		Handler:            HandlerFunc(func(context.Context, jobs.Job) error { return nil }),
	})
	reg.RegisterFunc("hash_blob", func(context.Context, jobs.Job) error { return nil })

	cfg := fastConfig("bare")
	cfg.Capabilities = toolchain.Capabilities()
	rt, err := NewRuntime(cfg, q, reg, discard())
	if err != nil {
		t.Fatal(err)
	}
	runFor(t, rt, func() bool { return len(q.completedIDs()) == 1 })

	if got := q.completedIDs(); len(got) != 1 || got[0] != "h1" {
		t.Errorf("completed = %v, want only the job that needs no toolchain", got)
	}
	// Pending, not failed. A job whose handler is simply not deployed here must
	// wait for a worker that has it, not burn its attempts and land in a dead
	// letter that an `apt install` would have made unnecessary.
	for _, id := range []string{"p1", "t1"} {
		if q.failure(id) != nil {
			t.Errorf("%s failed rather than being left pending: %v", id, q.failure(id))
		}
	}
}

// The inverse, because a capability that is never withheld and a capability
// that is never granted are both untested. A node that resolved the toolchain
// must actually claim the work.
func TestANodeWithTheToolchainClaimsTheJobsThatNeedIt(t *testing.T) {
	q := newFakeQueue(
		jobs.Job{ID: "p1", Type: "probe", RequiredCapability: media.CapabilityFFprobe},
		jobs.Job{ID: "t1", Type: "transcode", RequiredCapability: media.CapabilityFFmpeg},
	)
	reg := NewRegistry()
	reg.Register("probe", Registration{
		RequiredCapability: media.CapabilityFFprobe,
		Handler:            HandlerFunc(func(context.Context, jobs.Job) error { return nil }),
	})
	reg.Register("transcode", Registration{
		RequiredCapability: media.CapabilityFFmpeg,
		Handler:            HandlerFunc(func(context.Context, jobs.Job) error { return nil }),
	})

	cfg := fastConfig("equipped")
	// What a resolved toolchain produces, spelled the same way the worker
	// spells it — if these constants ever drift from the job registrations,
	// this test is what notices.
	cfg.Capabilities = []string{media.CapabilityFFprobe, media.CapabilityFFmpeg}
	rt, err := NewRuntime(cfg, q, reg, discard())
	if err != nil {
		t.Fatal(err)
	}
	runFor(t, rt, func() bool { return len(q.completedIDs()) == 2 })

	if got := q.completedIDs(); len(got) != 2 {
		t.Errorf("completed = %v, want both toolchain jobs", got)
	}
}
