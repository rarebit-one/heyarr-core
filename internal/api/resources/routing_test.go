// Every HTTP response here is closed by the harness's t.Cleanup.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/domain/routing"
)

// Read routing over the real API (§31, §32, M4-14).
//
// The domain package proves the preference exhaustively as a pure function.
// What these prove is the join — real peer rows, real replica states, M4-10's
// stored health verdict and a real second peer answering on a real socket —
// because each of those being right in isolation is how a wiring bug survives.

const (
	peerBID = "01990000-0000-7000-8000-000000000p02"
	peerCID = "01990000-0000-7000-8000-000000000p03"
)

// addPeer enrols a second peer the way the peers table holds one.
func (h *harness) addPeer(id, name, site, endpoint, healthState string) {
	h.t.Helper()
	h.exec(`INSERT INTO peers (id, name, site, mode, endpoint, is_self, health, last_seen_at, created_at)
		VALUES (?, ?, ?, 'full', ?, 0, ?, ?, ?)`,
		id, name, site, endpoint, healthState, seedTime, seedTime)
}

func (h *harness) putReplica(blobHash, peerIDArg, state string) {
	h.t.Helper()
	h.exec(`INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, verified_at, updated_at)
		VALUES (?, ?, ?, 0, ?, ?)
		ON CONFLICT (blob_hash, peer_id) DO UPDATE SET state = excluded.state`,
		blobHash, peerIDArg, state, seedTime, seedTime)
}

func (h *harness) dropReplica(blobHash, peerIDArg string) {
	h.t.Helper()
	h.exec(`DELETE FROM replicas WHERE blob_hash = ? AND peer_id = ?`, blobHash, peerIDArg)
}

// routingOf is the routing block of a plan, decoded.
type routingBlock struct {
	PeerID   string      `json:"peer_id"`
	Reason   *planReason `json:"reason"`
	Rejected []rejection `json:"rejected"`
}

type rejection struct {
	PeerID  string       `json:"peer_id"`
	Name    string       `json:"name"`
	Site    string       `json:"site"`
	Reasons []planReason `json:"reasons"`
}

// codesFor returns the rejection codes recorded against one peer.
func (r routingBlock) codesFor(peer string) []string {
	for _, rej := range r.Rejected {
		if rej.PeerID == peer {
			out := make([]string, 0, len(rej.Reasons))
			for _, reason := range rej.Reasons {
				out = append(out, reason.Code)
			}
			return out
		}
	}
	return nil
}

func (r routingBlock) names() []string {
	out := make([]string, 0, len(r.Rejected))
	for _, rej := range r.Rejected {
		out = append(out, rej.Name)
	}
	return out
}

// hasCode is an equality check over a list. It is NOT a substring check:
// "not_satisfied" contains "satisfied", and that shipped here once.
func hasCode(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}

// routedPlan asks the plan endpoint and returns the routing block, failing on
// a missing one — a plan with no routing block is a plan that cannot be
// audited.
func (h *harness) routedPlan(t *testing.T, assetID, deviceID string) (plan, routingBlock) {
	t.Helper()
	resp, p := h.plan(t, assetID, deviceID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	if p.Routing == nil {
		t.Fatal("the plan carries no routing block")
	}
	return p, *p.Routing
}

// ---------------------------------------------------------------------------

// The acceptance case, and it asserts BOTH halves from the SAME client: with a
// local replica it is routed locally, and with that replica removed and
// nothing else changed it is routed cross-site AND SAYS SO. One client, two
// states, two different peer ids.
func TestTheSameClientIsRoutedLocallyAndThenCrossSite(t *testing.T) {
	h := newHarness(t).seed()
	h.addPeer(peerBID, "peer-b", "site-b", "http://peer-b:7777", "reachable")
	h.putReplica(blob1Hash, peerBID, "present")

	// State 1: the local replica exists.
	_, routed := h.routedPlan(t, asset1ID, device1ID)
	if routed.PeerID != peerID {
		t.Fatalf("routed to %q, want the local peer %q", routed.PeerID, peerID)
	}
	if routed.Reason == nil || routed.Reason.Code != routing.SelectedSiteLocal {
		t.Errorf("reason = %+v, want %q", routed.Reason, routing.SelectedSiteLocal)
	}
	// The remote peer was eligible and lost on locality, which is how an
	// operator can see the fallback existed before it was needed.
	if got := routed.codesFor(peerBID); !hasCode(got, routing.RejectSiteLocalPreferred) {
		t.Errorf("peer-b codes = %v, want %q", got, routing.RejectSiteLocalPreferred)
	}

	// State 2: the same client, the same device, the same asset — and no local
	// replica.
	h.dropReplica(blob1Hash, peerID)

	p, routed := h.routedPlan(t, asset1ID, device1ID)
	if routed.PeerID != peerBID {
		t.Fatalf("routed to %q, want the cross-site peer %q", routed.PeerID, peerBID)
	}
	if routed.Reason == nil || routed.Reason.Code != routing.SelectedCrossSiteFallback {
		t.Fatalf("reason = %+v, want %q — a fallback nobody recorded is a fallback nobody notices",
			routed.Reason, routing.SelectedCrossSiteFallback)
	}
	if !strings.Contains(routed.Reason.Detail, "site-b") {
		t.Errorf("the fallback reason does not say where it fell back to: %q", routed.Reason.Detail)
	}
	if !p.Remote {
		t.Error("the plan does not report the chosen replica as remote")
	}
	if !p.has("remote_replica_only") {
		t.Errorf("the plan does not carry §31's cross-site note: %+v", p.Reasons)
	}
	if got := routed.codesFor(peerID); !hasCode(got, routing.RejectNoReplica) {
		t.Errorf("this node's codes = %v, want %q", got, routing.RejectNoReplica)
	}

	// State 3: a THIRD peer at the client's own site, which is not this node.
	//
	// It exists so that locality is the only thing separating the answer.
	// peer-c's id sorts AFTER peer-b's, and neither is this node, so nothing
	// but §31 can prefer it — which is what makes this half fail if the
	// locality preference is ever made a no-op. With peer-c absent, this node
	// would win on the "same machine" tie-break and the assertion would pass
	// on an implementation that had stopped looking at sites entirely.
	h.addPeer(peerCID, "peer-c", "site-a", "http://peer-c:7777", "reachable")
	h.putReplica(blob1Hash, peerCID, "present")

	_, routed = h.routedPlan(t, asset1ID, device1ID)
	if routed.PeerID != peerCID {
		t.Fatalf("routed to %q, want the same-site peer %q — §31 must beat every other "+
			"ordering, including the id tie-break", routed.PeerID, peerCID)
	}
	if routed.Reason == nil || routed.Reason.Code != routing.SelectedSiteLocal {
		t.Errorf("reason = %+v, want %q", routed.Reason, routing.SelectedSiteLocal)
	}
	if got := routed.codesFor(peerBID); !hasCode(got, routing.RejectSiteLocalPreferred) {
		t.Errorf("peer-b codes = %v, want %q", got, routing.RejectSiteLocalPreferred)
	}
}

// Health as a FILTER, not as decoration. An unhealthy peer at the client's own
// site holds the bytes; a healthy one at another site also does. Locality must
// lose, because routing a read to a machine that has been off since Tuesday is
// a client waiting out a TCP timeout while a healthy peer holds the same bytes.
func TestAnUnhealthyLocalPeerLosesToAHealthyRemoteOne(t *testing.T) {
	h := newHarness(t).seed()
	// This node is reachable by inspection, so the unhealthy LOCAL peer has to
	// be a different machine at the same site — which is also the realistic
	// shape: a site with two Full Peers, one of them down.
	h.dropReplica(blob1Hash, peerID)
	h.addPeer(peerCID, "peer-c", "site-a", "http://peer-c:7777", "unreachable")
	h.putReplica(blob1Hash, peerCID, "present")
	h.addPeer(peerBID, "peer-b", "site-b", "http://peer-b:7777", "reachable")
	h.putReplica(blob1Hash, peerBID, "present")

	_, routed := h.routedPlan(t, asset1ID, device1ID)
	if routed.PeerID != peerBID {
		t.Fatalf("routed to %q, want the healthy remote peer %q — an unhealthy peer at the "+
			"client's own site must not win on locality", routed.PeerID, peerBID)
	}
	if got := routed.codesFor(peerCID); !hasCode(got, routing.RejectPeerUnhealthy) {
		t.Errorf("peer-c codes = %v, want %q", got, routing.RejectPeerUnhealthy)
	}
	// An unprobed peer is not a healthy one either: 'unknown' is deliberately
	// not a synonym for reachable (migration 00020).
	h.exec(`UPDATE peers SET health = 'unknown' WHERE id = ?`, peerCID)
	_, routed = h.routedPlan(t, asset1ID, device1ID)
	if routed.PeerID != peerBID {
		t.Errorf("an unprobed peer was routed to: %q", routed.PeerID)
	}
	if got := routed.codesFor(peerCID); !hasCode(got, routing.RejectPeerUnhealthy) {
		t.Errorf("peer-c codes = %v, want %q", got, routing.RejectPeerUnhealthy)
	}
}

// A replica that is not 'present' is not a source, whatever else is true of
// the peer holding it.
func TestAPendingOrCorruptReplicaIsNotASource(t *testing.T) {
	for _, state := range []string{"pending", "corrupt", "missing"} {
		t.Run(state, func(t *testing.T) {
			h := newHarness(t).seed()
			h.putReplica(blob1Hash, peerID, state)
			h.addPeer(peerBID, "peer-b", "site-b", "http://peer-b:7777", "reachable")
			h.putReplica(blob1Hash, peerBID, "present")

			_, routed := h.routedPlan(t, asset1ID, device1ID)
			if routed.PeerID != peerBID {
				t.Fatalf("routed to %q, want %q — a %s replica is not readable bytes",
					routed.PeerID, peerBID, state)
			}
			codes := routed.codesFor(peerID)
			if !hasCode(codes, routing.RejectReplicaNotUsable) {
				t.Errorf("this node's codes = %v, want %q", codes, routing.RejectReplicaNotUsable)
			}
			if hasCode(codes, routing.RejectNoReplica) {
				t.Errorf("a %s replica was reported as no replica at all: %v", state, codes)
			}
		})
	}
}

// The refusal is the deliverable. No healthy peer holds it, and the answer
// names EVERY peer considered and why each was rejected — through both the
// plan endpoint's structure and the playback endpoint's problem document,
// because an operator reading a support ticket has only the second.
func TestNoHealthyPeerHoldsItIsARefusalThatNamesEveryPeer(t *testing.T) {
	h := newHarness(t).seed()
	h.putReplica(blob1Hash, peerID, "corrupt")
	h.addPeer(peerBID, "peer-b", "site-b", "http://peer-b:7777", "reachable") // healthy, no bytes
	h.addPeer(peerCID, "peer-c", "site-a", "http://peer-c:7777", "unreachable")
	h.putReplica(blob1Hash, peerCID, "present") // has the bytes, is down

	p, routed := h.routedPlan(t, asset1ID, device1ID)
	if p.Decision != "unplayable" {
		t.Fatalf("decision = %q, want unplayable", p.Decision)
	}
	if routed.PeerID != "" {
		t.Fatalf("a source was selected: %q", routed.PeerID)
	}
	if len(routed.Rejected) != 3 {
		t.Fatalf("%d peers named, want all 3 considered: %v", len(routed.Rejected), routed.names())
	}
	for peer, want := range map[string]string{
		peerID:  routing.RejectReplicaNotUsable,
		peerBID: routing.RejectNoReplica,
		peerCID: routing.RejectPeerUnhealthy,
	} {
		if got := routed.codesFor(peer); !hasCode(got, want) {
			t.Errorf("peer %s codes = %v, want %q among them", peer, got, want)
		}
	}

	// And the same content through POST /playback, where a client that pressed
	// play gets a problem document rather than a plan.
	resp, _ := h.startPlayback(t, asset1ID, device1ID, "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	body := string(h.body(resp))
	for _, want := range []string{
		"peer-a", "peer-b", "peer-c",
		"corrupt", "holds no replica", "unreachable",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, body)
		}
	}
}

// §32: the controller stays out of the content data path. The URL points at
// the SELECTED PEER, and fetching it moves bytes without the controller
// hearing about it.
func TestTheContentURLPointsAtTheSelectedPeerAndNotTheController(t *testing.T) {
	want := []byte("the bytes that live on peer-b, and nowhere near the controller")

	var peerHits int
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/blobs/"+blob1Hash+"/content" {
			http.NotFound(w, r)
			return
		}
		peerHits++
		_, _ = w.Write(want)
	}))
	defer peer.Close()

	h := newHarness(t).seed()
	h.addPeer(peerBID, "peer-b", "site-b", peer.URL, "reachable")
	h.putReplica(blob1Hash, peerBID, "present")
	h.dropReplica(blob1Hash, peerID)

	// ------------------------------------------------------------------
	// First: prove the observation can catch a controller-mediated fetch.
	//
	// Without this step the zero asserted below would also be produced by a
	// counter that never counted anything, and the test would pass on a build
	// where nothing was measured. So fetch the blob path against the
	// CONTROLLER's origin — which is exactly what a proxying implementation
	// would have handed back — and watch the counter move.
	// ------------------------------------------------------------------
	h.resetControllerRequests()
	fetch(t, h.http.URL+"/api/v1/blobs/"+blob1Hash+"/content")
	if got := h.controllerRequests(); got != 1 {
		t.Fatalf("the observation caught %d controller requests for a controller-mediated fetch, "+
			"want 1 — the assertion below would be meaningless", got)
	}

	resp, got := h.startPlayback(t, asset1ID, device1ID, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	if got.Plan.Routing == nil || got.Plan.Routing.PeerID != peerBID {
		t.Fatalf("routed to %+v, want %q", got.Plan.Routing, peerBID)
	}

	// The host is the peer's, and it is not the controller's.
	parsed, err := url.Parse(got.ContentURL)
	if err != nil {
		t.Fatalf("content_url %q is not a URL: %v", got.ContentURL, err)
	}
	peerHost := mustHost(t, peer.URL)
	controllerHost := mustHost(t, h.http.URL)
	if parsed.Host != peerHost {
		t.Errorf("content_url host = %q, want the selected peer %q", parsed.Host, peerHost)
	}
	if parsed.Host == controllerHost {
		t.Errorf("content_url points at the controller (%q); §32 keeps it out of the data path",
			controllerHost)
	}

	// And fetching it moves the bytes, with the controller hearing nothing.
	h.resetControllerRequests()
	body := fetch(t, got.ContentURL)
	if !bytes.Equal(body, want) {
		t.Errorf("fetched %q, want the peer's bytes", body)
	}
	if peerHits != 1 {
		t.Errorf("the peer served %d requests, want 1", peerHits)
	}
	if n := h.controllerRequests(); n != 0 {
		t.Errorf("the controller served %d requests for a direct peer fetch; it is in the data path", n)
	}
}

// The bound expires. A URL handed out with a credential that never expires is
// a credential that leaks permanently.
func TestTheDirectURLsCredentialExpiresAndAnExpiredOneIsRefused(t *testing.T) {
	h := newHarness(t, withAuth).seed()
	client := h.mint("client", auth.ScopeRead, auth.ScopeWrite)

	resp := h.do(http.MethodPost, "/api/v1/playback", client.Secret,
		strings.NewReader(fmt.Sprintf(`{"asset_id":%q,"device_id":%q}`, asset1ID, device1ID)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	var got startedPlayback
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatal(err)
	}

	if r := h.do(http.MethodGet, "/api/v1/assets", got.Token, nil); r.StatusCode != http.StatusOK {
		t.Fatalf("the credential does not work before it expires: %d", r.StatusCode)
	}

	// Past the expiry the response carried. Nothing sleeps: the clock is
	// injected (ADR-0017).
	h.clock.t = fixedTime.Add(2*time.Hour + time.Second)
	if r := h.do(http.MethodGet, "/api/v1/assets", got.Token, nil); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("an expired playback credential was accepted: %d", r.StatusCode)
	}
}

func fetch(t *testing.T, target string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}
