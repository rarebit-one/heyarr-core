package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// ADR-0025's degrade path, asserted by JOB STATE rather than by a log line.
//
// The claim: on a node with no indexer configured, a search job stays PENDING
// AND VISIBLE. It does not fail, it does not enter a retry backoff, and it does
// not disappear — because failing a job whose provider simply is not configured
// yet would lose work that a later configuration change would have completed.
//
// This is capability routing's second real user, after the media toolchain
// (ADR-0023), and the first one where the capability comes from configuration
// rather than from a binary on PATH.

func TestASearchJobStaysPendingWithoutAnIndexer(t *testing.T) {
	q, _ := newQueue(t)
	ctx := context.Background()

	// A search that requires an indexer, exactly as M3-12 will enqueue it.
	enqueued, err := q.Enqueue(ctx, EnqueueOptions{
		Type:               acquisition.SearchJobType,
		Payload:            acquisition.SearchPayload{DesiredItemID: "a-want"},
		DedupeKey:          acquisition.SearchDedupeKey("a-want"),
		RequiredCapability: providers.CapabilityIndexer.JobCapability(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// A worker on a node with NO providers advertises nothing — which is
	// exactly what providers.Registry.JobCapabilities returns for an empty
	// registry.
	bare := providers.New(nil).JobCapabilities()
	if len(bare) != 0 {
		t.Fatalf("a node with no providers advertises %v", bare)
	}

	_, err = q.Claim(ctx, ClaimOptions{Owner: "bare-worker", Capabilities: bare})
	if !errors.Is(err, ErrNoWork) {
		t.Fatalf("a worker with no indexer claimed the search: %v", err)
	}

	// The job is PENDING, not failed and not dead. Asserted by reading its
	// state back, because "it was not claimed" and "it was claimed and failed"
	// look identical from the claim call alone.
	after, err := q.Get(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != Pending {
		t.Errorf("state = %s, want pending — a job whose provider is not configured "+
			"must wait rather than fail", after.State)
	}
	if after.Attempts != 0 {
		t.Errorf("attempts = %d; an unclaimed job has not been tried", after.Attempts)
	}

	// And it is VISIBLE. "Pending forever, by design" is also a way to be
	// confused for hours, so the mitigation is that it can be found — here in
	// the queue's own counts, and over HTTP at /api/v1/jobs.
	stats, err := q.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats[Pending] != 1 {
		t.Errorf("pending = %d, want 1; the waiting job must be countable, which is "+
			"the whole mitigation for waiting forever by design", stats[Pending])
	}
}

// The same job, on a node that HAS an indexer, is claimed. Without this the
// test above would pass against a queue that never claimed anything.
func TestASearchJobIsClaimedWithAnIndexer(t *testing.T) {
	q, _ := newQueue(t)
	ctx := context.Background()

	enqueued, err := q.Enqueue(ctx, EnqueueOptions{
		Type:               acquisition.SearchJobType,
		Payload:            acquisition.SearchPayload{DesiredItemID: "a-want"},
		RequiredCapability: providers.CapabilityIndexer.JobCapability(),
	})
	if err != nil {
		t.Fatal(err)
	}

	reg := providers.New(nil)
	if err := reg.Register(
		providers.NewFake("an-indexer", providers.CapabilityIndexer)); err != nil {
		t.Fatal(err)
	}

	claimed, err := q.Claim(ctx, ClaimOptions{
		Owner: "capable-worker", Capabilities: reg.JobCapabilities(),
	})
	if err != nil {
		t.Fatalf("a worker with an indexer must claim the search: %v", err)
	}
	if claimed.ID != enqueued.ID {
		t.Errorf("claimed %s, enqueued %s", claimed.ID, enqueued.ID)
	}
}

// A download client is the third user of the same mechanism, and it must be
// routed INDEPENDENTLY: a node with an indexer and no download client claims
// searches and leaves acquisitions waiting.
func TestCapabilitiesRouteIndependently(t *testing.T) {
	q, _ := newQueue(t)
	ctx := context.Background()

	search, err := q.Enqueue(ctx, EnqueueOptions{
		Type:               acquisition.SearchJobType,
		Payload:            acquisition.SearchPayload{DesiredItemID: "a-want"},
		RequiredCapability: providers.CapabilityIndexer.JobCapability(),
	})
	if err != nil {
		t.Fatal(err)
	}
	acquire, err := q.Enqueue(ctx, EnqueueOptions{
		Type:               "acquire_release",
		Payload:            map[string]string{"desired_item_id": "a-want"},
		RequiredCapability: providers.CapabilityDownload.JobCapability(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Indexer only.
	reg := providers.New(nil)
	if err := reg.Register(
		providers.NewFake("an-indexer", providers.CapabilityIndexer)); err != nil {
		t.Fatal(err)
	}

	claimed, err := q.Claim(ctx, ClaimOptions{
		Owner: "indexer-only", Capabilities: reg.JobCapabilities(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != search.ID {
		t.Fatalf("claimed %s; a node with only an indexer takes the search", claimed.Type)
	}

	// Nothing else is claimable: the acquisition waits.
	if _, err := q.Claim(ctx, ClaimOptions{
		Owner: "indexer-only", Capabilities: reg.JobCapabilities(),
	}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("an indexer-only node claimed an acquisition: %v", err)
	}
	after, err := q.Get(ctx, acquire.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != Pending {
		t.Errorf("the acquisition is %s, want pending", after.State)
	}
}
