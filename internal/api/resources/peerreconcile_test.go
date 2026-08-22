// Every HTTP response here is closed by the t.Cleanup the harness registers,
// which bodyclose cannot see through — the same exemption harness_test.go
// carries for the same reason.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
)

// Convergence on demand (§19, §57, M4-08).
//
// The wire contract only. What the cycle DECIDES is tested against a real
// fabric in internal/worker; what this asserts is that the endpoint queues the
// right job for the right peer, refuses an unknown one, and cannot be made to
// queue two.

func queuedJob(t *testing.T, resp *http.Response) map[string]string {
	t.Helper()
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

// countJobs is the assertion that matters for dedupe: the queue, not the
// response.
func (h *harness) countJobs(t *testing.T, jobType string) int {
	t.Helper()
	var n int
	if err := h.db.Reader().QueryRow(
		`SELECT count(*) FROM jobs WHERE type = ?`, jobType).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReconcilingAPeerQueuesACycleForThatPeer(t *testing.T) {
	h := newHarness(t).seedSelf()
	h.exec(`INSERT INTO peers (id, name, site, mode, is_self, enrolled_at, created_at)
		VALUES ('01990000-0000-7000-8000-0000000000b2', 'site-b', 'site-b', 'full', 0, ?, ?)`,
		seedTime, seedTime)

	resp := h.do(http.MethodPost, "/api/v1/peers/site-b/reconcile", "", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want %d — the endpoint enqueues, it does not reconcile",
			resp.StatusCode, http.StatusAccepted)
	}
	body := queuedJob(t, resp)
	// The RESOLVED id, never the path parameter. The route accepts a name too,
	// and a job scoped to a name would diff against a peer set keyed by id and
	// silently match nothing — a cycle that succeeds having done nothing.
	if body["peer_id"] != "01990000-0000-7000-8000-0000000000b2" {
		t.Fatalf("peer_id = %q, want the resolved id rather than the name", body["peer_id"])
	}
	if body["job_id"] == "" {
		t.Fatal("no job_id in the response")
	}
	if body["status"] != "queued" {
		t.Fatalf("status = %q, want %q", body["status"], "queued")
	}

	var payload, key string
	if err := h.db.Reader().QueryRow(
		`SELECT payload, coalesce(dedupe_key, '') FROM jobs WHERE id = ?`,
		body["job_id"]).Scan(&payload, &key); err != nil {
		t.Fatal(err)
	}
	var p replication.ReconcilePeerPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatal(err)
	}
	if p.PeerID != "01990000-0000-7000-8000-0000000000b2" {
		t.Fatalf("the job is scoped to %q, want the resolved peer id", p.PeerID)
	}
	want := replication.ScopedReconcilePeerDedupeKey("01990000-0000-7000-8000-0000000000b2")
	if key != want {
		t.Fatalf("dedupe key = %q, want %q", key, want)
	}
}

// Asking twice while a cycle is queued yields one job — the same guarantee the
// beat relies on, reached from the other side.
func TestReconcilingAPeerTwiceQueuesOneCycle(t *testing.T) {
	h := newHarness(t).seedSelf()
	h.exec(`INSERT INTO peers (id, name, site, mode, is_self, enrolled_at, created_at)
		VALUES ('01990000-0000-7000-8000-0000000000b2', 'site-b', 'site-b', 'full', 0, ?, ?)`,
		seedTime, seedTime)

	first := queuedJob(t, h.do(http.MethodPost, "/api/v1/peers/site-b/reconcile", "", nil))
	if h.countJobs(t, replication.ReconcilePeerJobType) != 1 {
		t.Fatal("the first request queued no cycle; the dedupe assertion below would be vacuous")
	}
	second := queuedJob(t, h.do(http.MethodPost, "/api/v1/peers/site-b/reconcile", "", nil))
	if got := h.countJobs(t, replication.ReconcilePeerJobType); got != 1 {
		t.Fatalf("%d cycles queued for two requests, want 1", got)
	}
	if second["job_id"] != first["job_id"] {
		t.Fatalf("the second request answered with a different job (%s then %s); "+
			"the caller asked for a cycle and one is already coming",
			first["job_id"], second["job_id"])
	}
}

// An unknown peer is a 404 here rather than a job that runs, finds no such
// Full Peer and succeeds having done nothing.
func TestReconcilingAnUnknownPeerIsNotFound(t *testing.T) {
	h := newHarness(t).seedSelf()

	resp := h.do(http.MethodPost, "/api/v1/peers/nobody/reconcile", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	if got := h.countJobs(t, replication.ReconcilePeerJobType); got != 0 {
		t.Fatalf("%d cycles queued for an unknown peer, want 0", got)
	}
}
