//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/resources"
)

// GET /api/v1/capabilities — the fleet-wide read path ADR-0023 said was missing
// (§6, §75, ADR-0037, M5-112).
//
// Advertisements are seeded straight into the table rather than through a
// worker, because the two properties being asserted here are properties of the
// READ: an expired advertisement is not honoured, and `?capability=` is an
// exact match. Both must hold whatever wrote the row.

// seedAdvertisement writes one worker's advertisement, with an explicit expiry
// relative to the harness's fixed clock.
func (h *harness) seedAdvertisement(
	t *testing.T, workerID, peerID, peerName string, expiresIn time.Duration, names ...string,
) {
	t.Helper()
	stamp := fixedTime.Format(time.RFC3339Nano)
	expires := fixedTime.Add(expiresIn).Format(time.RFC3339Nano)
	for _, name := range names {
		if _, err := h.db.Writer().Exec(`
			INSERT INTO worker_capabilities
				(worker_id, capability, peer_id, peer_name, source, proved_at, expires_at, detail)
			VALUES (?, ?, ?, ?, 'probe', ?, ?, 'encoded a test pattern')`,
			workerID, name, peerID, peerName, stamp, expires); err != nil {
			t.Fatalf("seeding %s for %s: %v", name, workerID, err)
		}
	}
}

func (h *harness) capabilities(t *testing.T, query string) resources.CapabilitiesResponse {
	t.Helper()
	resp := h.get("/api/v1/capabilities" + query)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/capabilities%s: %s", query, resp.Status)
	}
	var out resources.CapabilitiesResponse
	if err := json.Unmarshal(h.body(resp), &out); err != nil {
		t.Fatalf("decoding the capability view: %v", err)
	}
	return out
}

// The fleet question, across more than one node.
func TestTheCapabilityViewAnswersWhichNodesHoldACapability(t *testing.T) {
	h := newHarness(t)
	h.seedAdvertisement(t, "worker-a", "peer-a", "node-a", time.Hour,
		"ffmpeg", "ffmpeg.encoder.hevc.qsv")
	h.seedAdvertisement(t, "worker-b", "peer-b", "node-b", time.Hour,
		"ffmpeg", "ffmpeg.encoder.hevc.qsv", "ffmpeg.encoder.av1.qsv")

	all := h.capabilities(t, "")
	if len(all.Holders) != 2 {
		t.Fatalf("the fleet has %d holders, want 2", len(all.Holders))
	}
	want := []string{"ffmpeg", "ffmpeg.encoder.av1.qsv", "ffmpeg.encoder.hevc.qsv"}
	if !reflect.DeepEqual(all.Available, want) {
		t.Errorf("available = %v, want %v", all.Available, want)
	}

	only := h.capabilities(t, "?capability=ffmpeg.encoder.av1.qsv")
	if len(only.Holders) != 1 {
		t.Fatalf("%d holders can encode AV1 on QSV, want 1", len(only.Holders))
	}
	if got := only.Holders[0].PeerName; got != "node-b" {
		t.Errorf("the AV1 holder is %q, want %q", got, "node-b")
	}
	if only.Capability != "ffmpeg.encoder.av1.qsv" {
		t.Errorf("the response does not echo the question: %q", only.Capability)
	}
}

// A stale advertisement is not honoured. The worker is gone; nothing deleted
// its rows, because the deaths that matter do not get to run a shutdown hook.
func TestAStaleAdvertisementIsNotHonoured(t *testing.T) {
	h := newHarness(t)
	// Expired an hour ago.
	h.seedAdvertisement(t, "worker-dead", "peer-a", "node-a", -time.Hour,
		"ffmpeg", "ffmpeg.encoder.hevc.qsv")
	// Alive, so the assertion below cannot pass because the endpoint returns
	// nothing at all.
	h.seedAdvertisement(t, "worker-alive", "peer-b", "node-b", time.Hour, "ffmpeg")

	all := h.capabilities(t, "")
	if len(all.Holders) != 1 {
		t.Fatalf("%d holders, want 1 — the dead worker's advertisement is still standing", len(all.Holders))
	}
	if got := all.Holders[0].WorkerID; got != "worker-alive" {
		t.Errorf("the surviving holder is %q, want %q", got, "worker-alive")
	}
	if !reflect.DeepEqual(all.Available, []string{"ffmpeg"}) {
		t.Errorf("available = %v; a dead worker still contributes to the union", all.Available)
	}

	stale := h.capabilities(t, "?capability=ffmpeg.encoder.hevc.qsv")
	if len(stale.Holders) != 0 {
		t.Errorf("a stale advertisement answered the routing question: %+v", stale.Holders)
	}
}

// The prefix trap over HTTP. `ffmpeg` is a prefix of `ffmpeg.encoder.hevc`, and
// a filter that matched prefixes would answer "which nodes can encode HEVC"
// with every node that has the binary.
func TestTheCapabilityFilterIsExactAndNotAPrefix(t *testing.T) {
	h := newHarness(t)
	h.seedAdvertisement(t, "worker-binary-only", "peer-a", "node-a", time.Hour, "ffmpeg")
	h.seedAdvertisement(t, "worker-encoder", "peer-b", "node-b", time.Hour, "ffmpeg.encoder.hevc")

	hevc := h.capabilities(t, "?capability=ffmpeg.encoder.hevc")
	if len(hevc.Holders) != 1 || hevc.Holders[0].WorkerID != "worker-encoder" {
		t.Errorf("asking for ffmpeg.encoder.hevc matched %+v", hevc.Holders)
	}

	binary := h.capabilities(t, "?capability=ffmpeg")
	if len(binary.Holders) != 1 || binary.Holders[0].WorkerID != "worker-binary-only" {
		t.Errorf("asking for ffmpeg matched %+v; a node that only advertises "+
			"ffmpeg.encoder.hevc does not have the binary capability", binary.Holders)
	}

	partial := h.capabilities(t, "?capability=ffmpeg.encoder")
	if len(partial.Holders) != 0 {
		t.Errorf("a partial dotted segment matched %+v", partial.Holders)
	}
}

// A fleet where nothing has advertised is a real answer, and it must marshal as
// empty arrays rather than nulls — null reads as "we could not find out".
func TestAFleetThatHasAdvertisedNothingIsAnAnswer(t *testing.T) {
	h := newHarness(t)
	body := string(h.body(h.get("/api/v1/capabilities")))
	for _, want := range []string{`"holders":[]`, `"available":[]`} {
		if !strings.Contains(body, want) {
			t.Errorf("the empty response does not contain %s: %s", want, body)
		}
	}
}

// The source travels with the capability, because it is what says whether the
// claim is re-verified at all.
func TestTheSourceOfEachCapabilityIsReported(t *testing.T) {
	h := newHarness(t)
	h.seedAdvertisement(t, "worker-a", "peer-a", "node-a", time.Hour, "ffmpeg.encoder.hevc.qsv")

	all := h.capabilities(t, "")
	if len(all.Holders) != 1 || len(all.Holders[0].Capabilities) != 1 {
		t.Fatalf("expected one holder with one capability, got %+v", all.Holders)
	}
	held := all.Holders[0].Capabilities[0]
	if held.Source != "probe" {
		t.Errorf("source = %q, want %q", held.Source, "probe")
	}
	if held.Name != "ffmpeg.encoder.hevc.qsv" {
		t.Errorf("name = %q", held.Name)
	}
	if !held.ProvedAt.Equal(fixedTime) {
		t.Errorf("proved_at = %s, want %s", held.ProvedAt, fixedTime)
	}
	if !all.Holders[0].ExpiresAt.Equal(fixedTime.Add(time.Hour)) {
		t.Errorf("expires_at = %s", all.Holders[0].ExpiresAt)
	}
}
