// Every HTTP response in this file is closed by the t.Cleanup that the harness
// registers, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Placement, on the wire, with a target set of two (§56, ADR-0010, ADR-0020,
// ADR-0027, M4-11).
//
// # What this file is for, and what it deliberately is not
//
// It is the WIRE contract of the placement axis once a second Full Peer exists:
// every value §56 draws a distinction between, read out of the satisfaction
// endpoint against a real database, plus the `unproven` field asserted BOTH
// ways in the same suite.
//
// It is NOT the milestone's headline. Nothing here moves a byte: the peers are
// rows and the replicas are rows. `converging` reached by a REAL transfer,
// between two processes, over mTLS, is scripts/acceptance.sh's — and it has to
// be, because the claim is about a running system and a running system is the
// only thing that can make it.
//
// # assert on the value, never on a substring of it
//
// "not_satisfied" CONTAINS "satisfied". Every check below compares the whole
// string, and the helper exists so that no future test in this file can quietly
// reach for strings.Contains instead.

const (
	// blob3Hash is bytes no peer holds. It is what tells `not_satisfied` apart
	// from `converging` — a blob on nobody is not converging on anything.
	blob3Hash = "blake3:3333333333333333333333333333333333333333333333333333333333333333"
	asset4ID  = "01990000-0000-7000-8000-0000000000a4"
)

// enrolPeerB adds a second Full Peer to the target set.
//
// A row, not a process. requiredPeers asks the peers table what the Full Peer
// set is, and this is what a second member of it looks like from where
// placement stands. peerBID and addPeer are routing_test.go's (M4-14), reused
// rather than duplicated: two fixtures for "the second peer" in one package is
// two things that can drift into disagreeing about what one is.
func (h *harness) enrolPeerB() *harness {
	h.t.Helper()
	h.addPeer(peerBID, "peer-b", "site-b", "https://127.0.0.1:8386", "reachable")
	return h
}

// wantOver creates a want through the API, so the acquisition state it needs
// is written by the code that owns it rather than by this test's idea of it.
func wantOver(t *testing.T, h *harness, workID, profileID string) string {
	t.Helper()
	body := `{"work_id":"` + workID + `","quality_profile_id":"` + profileID + `"}`
	resp := h.do(http.MethodPost, "/api/v1/desired", "", strings.NewReader(body))
	raw := h.body(resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/desired = %d: %s", resp.StatusCode, raw)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

// placementBlock reads the placement half of the satisfaction response.
type placementBlock struct {
	Satisfaction string   `json:"satisfaction"`
	Missing      []string `json:"missing"`
	Detail       string   `json:"detail"`
	Unproven     bool     `json:"unproven"`
}

func placementOf(t *testing.T, h *harness, desiredID string) placementBlock {
	t.Helper()
	resp := h.get("/api/v1/desired/" + desiredID + "/satisfaction")
	raw := h.body(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET satisfaction = %d: %s", resp.StatusCode, raw)
	}
	var got struct {
		Content struct {
			Satisfaction string `json:"satisfaction"`
		} `json:"content"`
		Placement placementBlock `json:"placement"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	// Placement is a question about THE BYTES THAT SATISFY. A test that read a
	// placement value off an unsatisfied want would be asserting on `unknown`
	// while believing it had asserted on something.
	if got.Content.Satisfaction != "satisfied" {
		t.Fatalf("content = %q, want satisfied — placement is only asked about bytes "+
			"that satisfy, so this test would be asserting on nothing", got.Content.Satisfaction)
	}
	return got.Placement
}

// assertPlacement compares the WHOLE value. "not_satisfied" contains
// "satisfied", and a substring match on an enum has shipped here before.
func assertPlacement(t *testing.T, got placementBlock, want, why string) {
	t.Helper()
	if got.Satisfaction != want {
		t.Errorf("%s: placement = %q, want %q (detail: %s)", why, got.Satisfaction, want, got.Detail)
	}
}

// `unproven` both ways, in one suite.
//
// This is the field the milestone changed, and it changed by becoming
// COMPUTED. A test that only asserted the two-peer answer would pass against a
// hard-coded `false`, which is the exact regression the single-peer half
// exists to catch — and vice versa.
func TestPlacementUnprovenIsComputedFromTheTargetSet(t *testing.T) {
	t.Run("one peer, and it is this node", func(t *testing.T) {
		h := newHarness(t).seed()
		got := placementOf(t, h, desired2ID)

		if !got.Unproven {
			t.Error("a target set of this node alone must report unproven — placement is " +
				"satisfied the moment content is, and that is not evidence that replication works")
		}
		// And the answer it is qualifying really is `satisfied`, which is what
		// makes the caveat necessary rather than decorative.
		assertPlacement(t, got, "satisfied", "one peer holding the only replica")
	})

	t.Run("two peers", func(t *testing.T) {
		h := newHarness(t).seed().enrolPeerB()
		got := placementOf(t, h, desired2ID)

		if got.Unproven {
			t.Error("with two required peers the axis is answering a real question, " +
				"so unproven must be false")
		}
	})
}

// The four answers the axis gives once there is a real target set (§56).
//
// Each is asserted on the whole value and each is reached by a DIFFERENT fact
// about the fabric, so a bug that collapsed two of them together would fail
// here rather than pass on a coincidence.
func TestPlacementAgainstATargetSetOfTwo(t *testing.T) {
	t.Run("held by one of two required peers is converging", func(t *testing.T) {
		h := newHarness(t).seed().enrolPeerB()
		got := placementOf(t, h, desired2ID)

		assertPlacement(t, got, "converging", "blob1 is present on peer-a and not on peer-b")
		// "Converging" with no list of what is missing is a status nobody can
		// act on, and the list is the half an operator uses.
		if len(got.Missing) != 1 || got.Missing[0] != peerBID {
			t.Errorf("missing = %v, want exactly [%s]", got.Missing, peerBID)
		}
	})

	t.Run("held by both is satisfied", func(t *testing.T) {
		h := newHarness(t).seed().enrolPeerB()
		h.exec(`INSERT INTO replicas
			(blob_hash, peer_id, state, bytes_present, verified_at, reported_at, updated_at)
			VALUES (?, ?, 'present', 42949672960, ?, ?, ?)`,
			blob1Hash, peerBID, seedTime, seedTime, seedTime)

		got := placementOf(t, h, desired2ID)
		assertPlacement(t, got, "satisfied", "both required peers hold verified bytes")
		if len(got.Missing) != 0 {
			t.Errorf("missing = %v, want nothing outstanding", got.Missing)
		}
		// The field stays false: two peers is two peers whether or not they
		// agree yet.
		if got.Unproven {
			t.Error("unproven must not come back once placement is satisfied — it is a " +
				"statement about the target set, not about the answer")
		}
	})

	t.Run("held by neither is not_satisfied, not converging", func(t *testing.T) {
		h := newHarness(t).seed().enrolPeerB()
		// A managed asset whose bytes no peer has ever reported holding. The
		// distinction EvaluatePlacement draws — nowhere at all is not
		// converging on anything — has to survive contact with real rows.
		h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at)
			VALUES (?, 1048576, 'video/mp4', ?)`, blob3Hash, seedTime)
		h.exec(`INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash,
				source_path, role, filename, mime, identification_source, missing_since,
				created_at, updated_at)
			VALUES (?, ?, ?, 'managed', ?, '/srv/films/BR2049-2.mp4', 'primary',
				'BR2049-2.mp4', 'video/mp4', 'path', NULL, ?, ?)`,
			asset4ID, edition2ID, libFilmsID, blob3Hash, seedTime, seedTime)

		id := wantOver(t, h, work2ID, profile2ID)
		got := placementOf(t, h, id)

		assertPlacement(t, got, "not_satisfied", "no peer holds these bytes")
		if len(got.Missing) != 2 {
			t.Errorf("missing = %v, want both required peers", got.Missing)
		}
	})

	t.Run("a linked asset is still not_applicable (ADR-0020)", func(t *testing.T) {
		h := newHarness(t).seed().enrolPeerB()
		// ADR-0020's blob-less asset does not become a placement question
		// because a second peer exists. There is nothing to replicate, and
		// calling that `satisfied` would make FULLY_SATISFIED mean "one copy,
		// on one disk, with no integrity guarantee".
		id := wantOver(t, h, work3ID, profile2ID)
		got := placementOf(t, h, id)

		assertPlacement(t, got, "not_applicable", "the satisfying asset is linked and has no blob")
		if got.Unproven {
			t.Error("unproven is about the target set, and this deployment has two peers " +
				"whether or not this particular want has bytes")
		}
	})
}
