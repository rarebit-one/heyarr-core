package controller

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/downloads"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The download poll beat (#247), against a real database and a real queue.
//
// These tests exist for the reason healthbeat_test.go gives, and this is the
// second time that reason has applied: the thing that was missing was not a
// behaviour inside a function, it was that NOTHING CALLED ONE. A unit test of
// enqueueDownloadPoll would have passed happily throughout the entire period
// the job was never scheduled — `downloads.PollJobType` was declared, its
// handler registered, its dedupe key declared beside it, and no enqueuer
// existed.
//
// So the first two tests drive the REAL controller and assert on the REAL job
// table, and they are the pair the sabotage is aimed at. The second is not
// optional: without it, a beat that fired unconditionally would pass the first
// one perfectly.

// downloadTestConfig is testConfig with a provider list.
func downloadTestConfig(t *testing.T, entries ...providers.Entry) config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.Providers = entries
	return cfg
}

// fakeDownloadClient is the smallest configuration that resolves to one.
//
// `type: fake` with an explicit download capability, and a path map because
// downloads.Constructor reads its local side as the directory to write into.
func fakeDownloadClient(dir string) providers.Entry {
	return providers.Entry{
		Name:         "acceptance-downloads",
		Type:         "fake",
		Capabilities: []string{"download"},
		PathMap: []providers.PathMapping{
			{Remote: "/downloads/complete", Local: dir},
		},
	}
}

// countPollJobs reads the job table directly, for the reason countHealthJobs
// does: the claim under test is whether a row EXISTS at all, which is precisely
// what a caller of Enqueue cannot establish about itself.
//
// A database whose migrations have not finished counts as zero rather than
// failing. waitForMigratedDatabase polls for the FILE and then sleeps 50ms to
// let the migration commit, which is a heuristic — and under -race it is not
// always enough, so the first read raced the schema and reported "no such
// table: jobs". That is not the finding this test is for, and turning it into a
// failure would send the reader to the beat rather than to the clock. Zero is
// also the truthful answer: a database with no jobs table holds no poll jobs,
// and the caller is polling to a deadline anyway.
func countPollJobs(t *testing.T, reader *sql.DB) int {
	t.Helper()
	var n int
	err := reader.QueryRow(
		`SELECT count(*) FROM jobs WHERE type = ?`, downloads.PollJobType,
	).Scan(&n)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return 0
		}
		t.Fatal(err)
	}
	return n
}

// 🔴 A controller with a download client enqueues a poll at startup.
//
// The whole issue, as one assertion against a running controller. Before this
// beat existed a transfer handed to a download client was never asked about
// again, so `QUEUED -> downloaded -> ingest` could not happen on a running node.
func TestAControllerWithADownloadClientEnqueuesAPollAtStartup(t *testing.T) {
	cfg := downloadTestConfig(t, fakeDownloadClient(t.TempDir()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- New(cfg, discard()).Run(ctx) }()
	waitForMigratedDatabase(t, cfg.Database.Path)

	db, err := sqlite.Open(context.Background(), sqlite.Options{Path: cfg.Database.Path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Polled to a deadline, watching the controller's own exit while polling —
	// #220's lesson. A controller that failed to START would otherwise report
	// here as "nothing schedules the poll", which is a claim about scheduling
	// code that is working perfectly.
	deadline := time.Now().Add(10 * time.Second)
	for countPollJobs(t, db.Reader()) == 0 {
		select {
		case err := <-done:
			t.Fatalf("the controller exited before enqueueing a poll: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("no download poll was enqueued — nothing schedules the pass that " +
				"observes a finished transfer, so QUEUED never reaches downloaded (#247)")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 🔴 A controller with NO download client enqueues nothing.
//
// The other half, and the one that stops the test above passing against a beat
// that fires unconditionally. It is also the behaviour that keeps a permanently
// unclaimable job out of the queue: the poll handler is registered with
// RequiredCapability download, so on a node with no client nothing can claim it
// and an enqueued job would simply wait forever, as a permanent unexplained row
// in GET /api/v1/jobs.
func TestAControllerWithNoDownloadClientEnqueuesNoPoll(t *testing.T) {
	cfg := downloadTestConfig(t) // no providers at all
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- New(cfg, discard()).Run(ctx) }()
	waitForMigratedDatabase(t, cfg.Database.Path)

	db, err := sqlite.Open(context.Background(), sqlite.Options{Path: cfg.Database.Path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Long enough that the startup enqueue would certainly have happened. It
	// cannot be proven absent forever, and a longer wait buys nothing: the
	// startup pass is enqueued before the listeners bind.
	time.Sleep(2 * time.Second)
	select {
	case err := <-done:
		t.Fatalf("the controller exited: %v", err)
	default:
	}
	if n := countPollJobs(t, db.Reader()); n != 0 {
		t.Errorf("%d download poll(s) enqueued on a node with no download client — the "+
			"poll handler requires the download capability, so nothing can ever claim "+
			"them and they stay queued forever", n)
	}
}

// A second poll is the same poll while the first is live.
//
// The dedupe key is what stops a slow pass piling up behind itself, which is
// the failure a naive timer produces exactly when the client is already
// struggling.
func TestASecondDownloadPollIsTheSamePassWhileTheFirstIsLive(t *testing.T) {
	db, queue := healthQueue(t)
	ctx := t.Context()

	if err := enqueueDownloadPoll(ctx, queue); err != nil {
		t.Fatal(err)
	}
	if err := enqueueDownloadPoll(ctx, queue); err != nil {
		t.Fatal(err)
	}
	if n := countPollJobs(t, db.Reader()); n != 1 {
		t.Errorf("%d poll jobs after two enqueues, want 1 — a pass already queued is the "+
			"same pass (%s)", n, downloads.PollDedupeKey)
	}
}

// 🔴 The kind is read from RESOLVED capabilities, not from the type string.
//
// A `type: fake` entry is an indexer, a download client, or both, entirely
// according to its capabilities list. Matching on the type would be wrong in
// both directions: it would start a beat for the fake indexer the acceptance
// demo has configured since M3, and it would miss a real client whose kind this
// function had not been taught.
func TestADownloadClientIsRecognisedByCapabilityAndNotByType(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name  string
		entry providers.Entry
		want  bool
	}{
		{
			name:  "a fake declaring download",
			entry: fakeDownloadClient(dir),
			want:  true,
		},
		{
			name: "a fake declaring only indexer, which the demo has always had",
			entry: providers.Entry{
				Name: "acceptance-indexer", Type: "fake",
				Capabilities: []string{"indexer"},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasDownloadClient([]providers.Entry{tc.entry}, discard()); got != tc.want {
				t.Errorf("hasDownloadClient = %v, want %v", got, tc.want)
			}
		})
	}
	if hasDownloadClient(nil, discard()) {
		t.Error("an empty provider list reports a download client")
	}
}

// The interval is the latency between a download finishing and ingest starting,
// so it is asserted to stay well inside the health beat's minute.
//
// Not a style preference: downloadbeat.go's argument for fifteen seconds is
// that a minute of a finished file sitting in a directory reads as a broken
// pipeline rather than as a cadence. If somebody raises this to match the
// health beat, that argument has been dropped rather than revisited.
func TestTheDownloadPollIsFasterThanTheHealthBeat(t *testing.T) {
	if downloadPollInterval >= providerHealthInterval {
		t.Errorf("the download poll interval is %s and the health beat's is %s — the poll "+
			"prices a local RPC and IS the wait before ingest, so it is meant to be the "+
			"shorter of the two", downloadPollInterval, providerHealthInterval)
	}
	if downloadPollInterval < time.Second {
		t.Errorf("the download poll interval is %s, which re-enumerates a client's whole "+
			"queue faster than it can answer", downloadPollInterval)
	}
}

// The beat stops with its context, so a shut-down controller leaves no goroutine
// enqueueing into a closed database.
func TestTheDownloadBeatStopsWithItsContext(t *testing.T) {
	db, queue := healthQueue(t)
	ctx, cancel := context.WithCancel(context.Background())

	startDownloadPoll(ctx, []providers.Entry{fakeDownloadClient(t.TempDir())}, queue, discard())
	cancel()

	before := countPollJobs(t, db.Reader())
	time.Sleep(downloadPollInterval + 200*time.Millisecond)
	if after := countPollJobs(t, db.Reader()); after != before {
		t.Errorf("the beat enqueued after its context was cancelled: %d then %d", before, after)
	}
}
