package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

// ---------------------------------------------------------------------------
// --json golden shapes, one per command
// ---------------------------------------------------------------------------
//
// The --json output is a contract with whatever scripts it. Every command has a
// golden file so that a field renamed, dropped or re-nested is a failing test
// rather than a broken pipeline somebody finds a week later. Identifiers the
// test does not control and timestamps are normalised (see normalise, which
// token_test.go also uses); everything else is asserted byte for byte.

func TestLibraryAddJSONShape(t *testing.T) {
	h := newAPIHarness(t)
	out := h.mustRun("library", "add", "films", "--content-type", "movie",
		"--root", "/srv/films", "--json")
	testutil.Golden(t, "testdata/library_add.json", []byte(normalise(out)))
}

func TestLibraryListJSONShape(t *testing.T) {
	h := newAPIHarness(t).seed()
	out := h.mustRun("library", "list", "--json")
	testutil.Golden(t, "testdata/library_list.json", []byte(normalise(out)))
}

func TestWorksListJSONShape(t *testing.T) {
	h := newAPIHarness(t).seed()
	out := h.mustRun("works", "list", "--json")
	testutil.Golden(t, "testdata/works_list.json", []byte(normalise(out)))
}

func TestWorksShowJSONShape(t *testing.T) {
	h := newAPIHarness(t).seed()
	out := h.mustRun("works", "show", work1ID, "--json")
	testutil.Golden(t, "testdata/works_show.json", []byte(normalise(out)))
}

func TestAssetsListJSONShape(t *testing.T) {
	h := newAPIHarness(t).seed()
	out := h.mustRun("assets", "list", "--json")
	testutil.Golden(t, "testdata/assets_list.json", []byte(normalise(out)))
}

func TestJobsListJSONShape(t *testing.T) {
	h := newAPIHarness(t).seed()
	out := h.mustRun("jobs", "list", "--json")
	testutil.Golden(t, "testdata/jobs_list.json", []byte(normalise(out)))
}

func TestJobsShowJSONShape(t *testing.T) {
	h := newAPIHarness(t).seed()
	out := h.mustRun("jobs", "show", jobDeadID, "--json")
	testutil.Golden(t, "testdata/jobs_show.json", []byte(normalise(out)))
}

func TestJobsRetryJSONShape(t *testing.T) {
	h := newAPIHarness(t).seed()
	out := h.mustRun("jobs", "retry", jobDeadID, "--json")
	testutil.Golden(t, "testdata/jobs_retry.json", []byte(normalise(out)))
}

func TestPeersListJSONShape(t *testing.T) {
	h := newAPIHarness(t).seed()
	out := h.mustRun("peers", "list", "--json")
	testutil.Golden(t, "testdata/peers_list.json", []byte(normalise(out)))
}

func TestBlobsStatJSONShape(t *testing.T) {
	h := newAPIHarness(t).seed()
	out := h.mustRun("blobs", "stat", blob1Hash, "--json")
	testutil.Golden(t, "testdata/blobs_stat.json", []byte(normalise(out)))
}

// blobContent is fixed, so its BLAKE3 digest is fixed too and the golden files
// can carry the real hash rather than a placeholder. A hash normalised away is
// a hash nothing is asserting.
const blobContent = "the very same bytes"

func TestBlobsCatJSONShape(t *testing.T) {
	h := newAPIHarness(t)
	desc := h.putBlob(blobContent)
	target := filepath.Join(h.dataDir, "out.bin")

	out := h.mustRun("blobs", "cat", desc.Hash.String(), "--output", target, "--json")
	testutil.Golden(t, "testdata/blobs_cat.json", []byte(h.normalisePaths(normalise(out))))

	got, err := os.ReadFile(target) // #nosec G304 -- a path this test just wrote
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != blobContent {
		t.Errorf("the file holds %q, want %q", got, blobContent)
	}
}

func TestBlobsVerifyJSONShape(t *testing.T) {
	h := newAPIHarness(t)
	desc := h.putBlob(blobContent)
	out := h.mustRun("blobs", "verify", desc.Hash.String(), "--json")
	testutil.Golden(t, "testdata/blobs_verify.json", []byte(normalise(out)))
}

func TestScanQueuedJSONShape(t *testing.T) {
	h := newAPIHarness(t).seed()
	out := h.mustRun("scan", "films", "--json")
	testutil.Golden(t, "testdata/scan_queued.json", []byte(normalise(out)))
}

func TestEventsTailJSONShape(t *testing.T) {
	h := newAPIHarness(t).seed()
	ctx := context.Background()
	for _, e := range []struct{ kind, subject, id string }{
		{"content.library.created", "library", libFilmsID},
		{"content.asset.created", "asset", asset1ID},
		{"blob.created", "blob", blob1Hash},
	} {
		if _, err := h.events.Emit(ctx, e.kind, e.subject, e.id,
			map[string]any{"id": e.id}); err != nil {
			t.Fatal(err)
		}
	}

	out := h.mustRun("events", "tail", "--after", "0", "--limit", "3", "--json")
	testutil.Golden(t, "testdata/events_tail.json", []byte(normalise(out)))
}

// ---------------------------------------------------------------------------
// Behaviour
// ---------------------------------------------------------------------------

// A problem document is the server explaining itself. Rendering it as a status
// code throws away the half a human needed.
func TestAProblemDocumentIsRenderedAsItsDetail(t *testing.T) {
	h := newAPIHarness(t).seed()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "an unknown work",
			args: []string{"works", "show", "01990000-0000-7000-8000-00000000zzzz"},
			want: "no work with that identifier",
		},
		{
			name: "an unknown blob",
			args: []string{"blobs", "stat", blob2Hash[:len(blob2Hash)-1] + "3"},
			want: "no blob is recorded with the hash",
		},
		{
			name: "an unusable filter value",
			args: []string{"jobs", "list", "--state", "confused"},
			want: "state must be one of",
		},
		{
			name: "a library with no roots to scan",
			args: []string{"scan", "books"},
			want: "has no enabled roots",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := h.run(tt.args...)
			if err == nil {
				t.Fatalf("heyarr %s succeeded", strings.Join(tt.args, " "))
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
			// The status code alone is what a lazy client prints instead.
			if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "status") {
				t.Errorf("the error reads like a status code rather than an explanation: %q", err)
			}
		})
	}
}

// A malformed hash and an absent one are different mistakes, and the CLI must
// say which. Collapsing them makes a script retry a request that can never
// succeed.
func TestAMalformedHashAndAnAbsentOneAreDifferentAnswers(t *testing.T) {
	h := newAPIHarness(t).seed()

	_, _, err := h.run("blobs", "stat", "not-a-hash")
	if err == nil {
		t.Fatal("a malformed hash was accepted")
	}
	if !strings.Contains(err.Error(), "blake3:") {
		t.Errorf("the error does not say what a hash looks like: %q", err)
	}

	absent := "blake3:" + strings.Repeat("9", 64)
	_, _, err = h.run("blobs", "stat", absent)
	if err == nil {
		t.Fatal("an absent blob was reported as present")
	}
	if !strings.Contains(err.Error(), "no blob is recorded") {
		t.Errorf("an absent blob was not reported as absent: %q", err)
	}
}

// A listing must follow the cursors. One page and a silent stop is the failure
// this whole client exists to not have: it returns rows and no indication that
// there are more.
func TestAListingFollowsPaginationCursorsToTheEnd(t *testing.T) {
	h := newAPIHarness(t).seed()

	// Three works, two per page, so the walk must make at least two requests.
	out := h.mustRun("works", "list", "--json", "--page-size", "2")
	var works []client.Work
	if err := json.Unmarshal([]byte(out), &works); err != nil {
		t.Fatalf("works list --json is not JSON: %v\n%s", err, out)
	}
	if len(works) != 3 {
		t.Fatalf("the listing returned %d works, want 3 — it stopped at a page boundary\n%s",
			len(works), out)
	}
	seen := map[string]bool{}
	for _, w := range works {
		if seen[w.ID] {
			t.Errorf("the work %s was returned twice", w.ID)
		}
		seen[w.ID] = true
	}
	for _, want := range []string{work1ID, work2ID, work3ID} {
		if !seen[want] {
			t.Errorf("the work %s was skipped", want)
		}
	}

	// And --limit stops early rather than being ignored.
	out = h.mustRun("works", "list", "--json", "--page-size", "2", "--limit", "1")
	works = nil
	if err := json.Unmarshal([]byte(out), &works); err != nil {
		t.Fatal(err)
	}
	if len(works) != 1 {
		t.Errorf("--limit 1 returned %d works", len(works))
	}
}

// An empty listing is [], never null: the difference shows up as a nil
// dereference in somebody's script rather than here.
func TestAnEmptyListingIsAnEmptyArray(t *testing.T) {
	h := newAPIHarness(t)
	for _, args := range [][]string{
		{"library", "list", "--json"},
		{"works", "list", "--json"},
		{"assets", "list", "--json"},
		{"jobs", "list", "--json"},
		{"peers", "list", "--json"},
	} {
		out := h.mustRun(args...)
		if strings.TrimSpace(out) != "[]" {
			t.Errorf("heyarr %s printed %q, want []", strings.Join(args, " "), strings.TrimSpace(out))
		}
	}
}

// ---------------------------------------------------------------------------
// --wait
// ---------------------------------------------------------------------------

// The acceptance criterion, and the reason the flag exists: a CLI that exits 0
// when the work failed is worse than no CLI.
func TestScanWaitExitsNonZeroWhenTheJobDies(t *testing.T) {
	h := newAPIHarness(t).seed()

	type result struct {
		stdout string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		out, _, err := h.run("scan", "films", "--wait", "--json",
			"--poll-interval", "50ms", "--wait-timeout", "60s")
		done <- result{out, err}
	}()

	// Wait for the job to exist rather than sleeping and hoping: the scan is
	// enqueued by another goroutine, and a fixed wait here is a bet on machine
	// speed that CI eventually loses.
	h.killJob(h.awaitScanJob())

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatalf("scan --wait exited 0 with a dead scan job\n%s", got.stdout)
		}
		if !errors.Is(got.err, ErrScanFailed) {
			t.Errorf("error = %v, want it to wrap ErrScanFailed", got.err)
		}
		if !strings.Contains(got.err.Error(), "dead") {
			t.Errorf("the error does not say the job is dead: %v", got.err)
		}
		testutil.Golden(t, "testdata/scan_wait_dead.json", []byte(normalise(got.stdout)))
	case <-time.After(60 * time.Second):
		t.Fatal("scan --wait never returned after its job died")
	}
}

// The other half of the contract: it exits 0 when the work actually succeeded.
func TestScanWaitExitsZeroWhenTheJobSucceeds(t *testing.T) {
	h := newAPIHarness(t).seed()

	type result struct {
		stdout string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		out, _, err := h.run("scan", "films", "--wait", "--json",
			"--poll-interval", "50ms", "--wait-timeout", "60s")
		done <- result{out, err}
	}()

	id := h.awaitScanJob()
	ctx := context.Background()
	claimed, err := h.jobs.Claim(ctx, jobs.ClaimOptions{
		Owner: "test-worker", Types: []string{"scan_library"}, LeaseTTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("claiming the scan job: %v", err)
	}
	if claimed.ID != id {
		t.Fatalf("claimed %s, want %s", claimed.ID, id)
	}
	if err := h.jobs.Complete(ctx, claimed.ID, "test-worker"); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("scan --wait exited non-zero for a scan that succeeded: %v\n%s", got.err, got.stdout)
		}
		testutil.Golden(t, "testdata/scan_wait_succeeded.json", []byte(normalise(got.stdout)))
	case <-time.After(60 * time.Second):
		t.Fatal("scan --wait never returned after its job succeeded")
	}
}

// A job that is already terminal must return immediately. Waiting for an event
// that was emitted an hour ago is a hang, and it is the most common way to
// wait on a job that finished quickly.
func TestWaitingOnAnAlreadyFinishedJobReturnsImmediately(t *testing.T) {
	h := newAPIHarness(t).seed()
	c := h.client(t)

	// No poll backstop at all: if this passes, it passed on the state read,
	// which is the thing being asserted.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	result, err := c.WaitForJobs(ctx, []string{jobDeadID}, client.WaitOptions{PollInterval: 0})
	if err != nil {
		t.Fatalf("waiting on a finished job: %v", err)
	}
	if !result.Failed() || result.Dead != 1 {
		t.Errorf("result = %+v, want one dead job", result)
	}
}

// The ordering that makes --wait correct: subscribe, THEN look. AfterSubscribe
// runs in the window between the two, so this test makes the job finish
// precisely there. With the subscription opened after the state read instead,
// the transition happens while nothing is listening and the wait never ends —
// which is what the deadline below catches.
func TestWaitSubscribesBeforeItReadsState(t *testing.T) {
	h := newAPIHarness(t).seed()
	c := h.client(t)

	queued, err := h.jobs.Enqueue(context.Background(), jobs.EnqueueOptions{
		Type: "scan_library", Payload: map[string]string{"root_id": rootID},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, err := c.WaitForJobs(ctx, []string{queued.ID}, client.WaitOptions{
		// Zero, deliberately: with a backstop poll this test would pass even
		// with the subscription opened last, and would be asserting nothing.
		PollInterval:   0,
		AfterSubscribe: func() { h.killJob(queued.ID) },
	})
	if err != nil {
		t.Fatalf("the wait did not end: %v", err)
	}
	if result.Dead != 1 {
		t.Errorf("result = %+v, want the job that died in the window to be reported dead", result)
	}
}

// A dropped-events notice must not be read as "the job is fine". The wait
// re-reads the job rather than assuming, which is what this asserts: with a
// subscriber buffer of one, the flood of events below is guaranteed to drop
// some, and the wait must still end with the right answer.
func TestWaitSurvivesADroppedEventsNotice(t *testing.T) {
	h := newAPIHarness(t, func(o *harnessOptions) { o.streamBuffer = 1 })
	h.seed()
	c := h.client(t)

	queued, err := h.jobs.Enqueue(context.Background(), jobs.EnqueueOptions{
		Type: "scan_library", Payload: map[string]string{"root_id": rootID},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	flooded := make(chan struct{})
	go func() {
		defer close(flooded)
		for i := 0; i < 200; i++ {
			if _, err := h.events.Emit(ctx, "system.scan.progress", "scan_run", rootID,
				map[string]any{"root_id": rootID, "state": "running"}); err != nil {
				return
			}
		}
		h.killJob(queued.ID)
	}()

	result, err := c.WaitForJobs(ctx, []string{queued.ID}, client.WaitOptions{
		PollInterval: 50 * time.Millisecond,
	})
	<-flooded
	if err != nil {
		t.Fatalf("the wait did not survive a gap notice: %v", err)
	}
	if result.Dead != 1 {
		t.Errorf("result = %+v, want the dead job reported", result)
	}
}

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

// The documented precedence, asserted rather than described: --token, then
// $HEYARR_TOKEN, then --token-file, then <data_dir>/cli.token.
func TestTokenPrecedence(t *testing.T) {
	h := newAPIHarness(t, withAPIAuth)
	h.seed()
	ctx := context.Background()

	good, err := h.tokens.Create(ctx, "cli", []auth.Scope{auth.ScopeRead}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Nothing at all is refused, and says so rather than showing an empty list.
	if _, _, err := h.run("peers", "list"); err == nil {
		t.Error("an unauthenticated call succeeded against an authenticated API")
	}

	// The flag.
	h.mustRun("peers", "list", "--token", good.Secret)

	// The environment, with no flag.
	t.Setenv(TokenEnvVar, good.Secret)
	h.mustRun("peers", "list")

	// The flag beats the environment: a wrong environment must not break an
	// explicit credential.
	t.Setenv(TokenEnvVar, "heyarr_pat_not_a_token")
	h.mustRun("peers", "list", "--token", good.Secret)

	// The file, once the environment is out of the way.
	t.Setenv(TokenEnvVar, "")
	file := filepath.Join(h.dataDir, "token.txt")
	if err := os.WriteFile(file, []byte(good.Secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.mustRun("peers", "list", "--token-file", file)

	// And the ambient default in the data directory.
	if err := os.WriteFile(filepath.Join(h.dataDir, DefaultTokenFile), []byte(good.Secret), 0o600); err != nil {
		t.Fatal(err)
	}
	h.mustRun("peers", "list")

	// A token file named explicitly and not readable is a mistake worth
	// stopping for, unlike the ambient default not existing.
	if _, _, err := h.run("peers", "list", "--token-file", filepath.Join(h.dataDir, "nope")); err == nil {
		t.Error("a missing --token-file was ignored")
	}
}

// No command may take a credential as a positional argument: it would be in
// the shell history, in `ps`, and in the CI log.
func TestNoCommandTakesATokenPositionally(t *testing.T) {
	root := NewRootCommand(Options{})
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		use := cmd.Use
		if strings.Contains(use, "<token>") || strings.Contains(use, "[token]") {
			t.Errorf("%s takes a token as an argument: %q", cmd.CommandPath(), use)
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root)
}

// ---------------------------------------------------------------------------
// Blob bytes
// ---------------------------------------------------------------------------

// The resume idiom: Range: bytes=<offset>- plus If-Range with the blob's own
// validator. A 206 continues the file; a 200 means the range was not honoured
// and the partial file must be discarded rather than appended to.
func TestBlobsCatResumesFromAPartialFile(t *testing.T) {
	h := newAPIHarness(t)
	desc := h.putBlob(blobContent)
	target := filepath.Join(h.dataDir, "partial.bin")

	// Half a download, as an interrupted transfer would have left it.
	head := blobContent[:8]
	if err := os.WriteFile(target, []byte(head), 0o600); err != nil {
		t.Fatal(err)
	}

	out := h.mustRun("blobs", "cat", desc.Hash.String(), "--output", target, "--resume", "--json")
	var summary catOutput
	if err := json.Unmarshal([]byte(out), &summary); err != nil {
		t.Fatalf("blobs cat --json is not JSON: %v\n%s", err, out)
	}
	if !summary.Resumed {
		t.Errorf("the transfer did not resume: %+v", summary)
	}
	if summary.StartedFrom != int64(len(head)) {
		t.Errorf("started from %d, want %d", summary.StartedFrom, len(head))
	}
	if summary.BytesWritten != int64(len(blobContent)-len(head)) {
		t.Errorf("wrote %d bytes, want the %d that were missing",
			summary.BytesWritten, len(blobContent)-len(head))
	}

	// The whole point: the file on disk is the blob, byte for byte.
	got, err := os.ReadFile(target) // #nosec G304 -- a path this test just wrote
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != blobContent {
		t.Fatalf("the resumed file is %q, want %q", got, blobContent)
	}

	// And a resume against a file that is already complete asks for a range
	// past the end, which the server refuses rather than silently duplicating
	// bytes.
	if _, _, err := h.run("blobs", "cat", desc.Hash.String(),
		"--output", target, "--resume", "--json"); err == nil {
		t.Error("resuming a complete file was reported as more work done")
	}
	after, err := os.ReadFile(target) // #nosec G304 -- a path this test just wrote
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != blobContent {
		t.Errorf("a refused resume changed the file: %q", after)
	}
}

// Mixing a JSON summary and the bytes on stdout would produce neither.
func TestBlobsCatRefusesToMixJSONAndBytesOnStdout(t *testing.T) {
	h := newAPIHarness(t)
	desc := h.putBlob(blobContent)
	if _, _, err := h.run("blobs", "cat", desc.Hash.String(), "--json"); err == nil {
		t.Error("--json without --output was accepted")
	}
}

// Verification happens on the bytes as received (invariant 1). Corrupt the
// stored bytes behind the CAS's back and the CLI must notice, because it
// hashes what arrived rather than asking the server whether it is happy.
func TestBlobsVerifyFailsOnBytesThatDoNotHashToTheirName(t *testing.T) {
	h := newAPIHarness(t)
	desc := h.putBlob(blobContent)
	h.mustRun("blobs", "verify", desc.Hash.String(), "--json")

	corruptBlobFile(t, h, desc.Hash.String())

	out, _, err := h.run("blobs", "verify", desc.Hash.String(), "--json")
	if err == nil {
		t.Fatalf("verify passed on corrupt bytes\n%s", out)
	}
	if !strings.Contains(err.Error(), "did not verify") {
		t.Errorf("error = %q, want it to say the bytes did not verify", err)
	}
}

// The unix socket is the default transport, so at least one end-to-end run
// goes over it rather than over the TCP listener every other test uses.
func TestTheCLITalksOverTheUnixSocket(t *testing.T) {
	h := newAPIHarness(t).seed()
	socket := h.serveUnix(t)

	out, _, err := run(t, context.Background(),
		"--config", h.configPath, "--addr", "unix://"+socket, "peers", "list", "--json")
	if err != nil {
		t.Fatalf("peers list over the socket: %v", err)
	}
	var peers []client.Peer
	if err := json.Unmarshal([]byte(out), &peers); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(peers) != 1 || !peers[0].IsSelf {
		t.Errorf("peers over the socket = %+v, want the one self peer", peers)
	}
}
