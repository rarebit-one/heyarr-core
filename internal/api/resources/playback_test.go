//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/auth"
)

type startedPlayback struct {
	SessionID  string `json:"session_id"`
	Plan       plan   `json:"plan"`
	ContentURL string `json:"content_url"`
	Token      string `json:"token"`
	ExpiresAt  string `json:"expires_at"`
}

func (h *harness) startPlayback(t *testing.T, assetID, deviceID, verb string) (*http.Response, startedPlayback) {
	t.Helper()
	body := fmt.Sprintf(`{"asset_id":%q,"device_id":%q`, assetID, deviceID)
	if verb != "" {
		body += fmt.Sprintf(`,"verb":%q`, verb)
	}
	body += "}"
	resp := h.do(http.MethodPost, "/api/v1/playback", "", strings.NewReader(body))
	var out startedPlayback
	if resp.StatusCode == http.StatusCreated {
		if err := json.Unmarshal(h.body(resp), &out); err != nil {
			t.Fatal(err)
		}
	}
	return resp, out
}

// One call: plan, session, and somewhere to play from.
func TestStartingAPlaybackReturnsASessionAndAURL(t *testing.T) {
	h := newHarness(t).seed()

	resp, got := h.startPlayback(t, asset1ID, device1ID, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	if got.SessionID == "" {
		t.Error("no session was opened")
	}
	if got.Plan.Decision != "direct" {
		t.Errorf("decision = %q", got.Plan.Decision)
	}
	// §32 and ADR-0013: the ordinary blob endpoint, not a playback-specific
	// route and not a controller proxy.
	if !strings.HasPrefix(got.ContentURL, "/api/v1/blobs/") ||
		!strings.HasSuffix(got.ContentURL, "/content") {
		t.Errorf("content_url = %q, want the ordinary blob endpoint", got.ContentURL)
	}
	if strings.Contains(got.ContentURL, "playback") || strings.Contains(got.ContentURL, "stream") {
		t.Errorf("a playback-specific byte route appeared: %q", got.ContentURL)
	}
	if got.Token == "" || got.ExpiresAt == "" {
		t.Errorf("no credential was issued: %+v", got)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "/consumption/sessions/") {
		t.Errorf("Location = %q, want the session", loc)
	}

	// The session is real and followable.
	var session struct {
		State string `json:"state"`
		Verb  string `json:"verb"`
	}
	if err := json.Unmarshal(h.body(h.get("/api/v1/consumption/sessions/"+got.SessionID)), &session); err != nil {
		t.Fatal(err)
	}
	if session.State != "created" {
		t.Errorf("state = %q, want created — bytes are not moving until the client starts", session.State)
	}
}

// The credential actually works against the blob endpoint, and expires.
// Issuing a token nobody tried is issuing a claim.
func TestThePlaybackCredentialWorksAndIsScopedToRead(t *testing.T) {
	h := newHarness(t, withAuth).seed()
	writer := h.mint("client", auth.ScopeRead, auth.ScopeWrite)

	resp := h.do(http.MethodPost, "/api/v1/playback", writer.Secret,
		strings.NewReader(fmt.Sprintf(`{"asset_id":%q,"device_id":%q}`, asset1ID, device1ID)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	var got startedPlayback
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatal(err)
	}

	// It reads.
	if r := h.do(http.MethodGet, "/api/v1/assets", got.Token, nil); r.StatusCode != http.StatusOK {
		t.Errorf("the playback credential cannot read: %d", r.StatusCode)
	}
	// And only reads. A playback token that could register a device or open
	// another session is a playback token that has become a client credential.
	r := h.do(http.MethodPost, "/api/v1/devices", got.Token,
		strings.NewReader(`{"device_key":"sneaky","name":"Sneaky"}`))
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("the playback credential wrote something: %d", r.StatusCode)
	}
}

// Everything that is not DIRECT is a refusal, and the refusal is as much the
// deliverable as the success. A client that cannot distinguish "not supported
// for you" from "the server is broken" retries the wrong one forever.
func TestANonDirectPlanIsRefusedWithItsRationaleAndOpensNoSession(t *testing.T) {
	h := newHarness(t).seed()
	// A probe the seeded device refuses.
	h.exec(`INSERT INTO blob_probes
		(blob_hash, container, format_long, duration_seconds, bitrate_bps,
		 streams, bytes_read, materialised, probed_at)
		VALUES (?, 'mov,mp4,m4a', '', 120.0, 8000000, ?, 4096, 0, ?)`,
		blob1Hash,
		`[{"index":0,"type":"video","codec":"av1","width":1920,"height":1080},
		  {"index":1,"type":"audio","codec":"aac","channels":2}]`, seedTime)

	before := h.count(t, `SELECT count(*) FROM consumption_sessions`)

	resp, _ := h.startPlayback(t, asset1ID, device1ID, "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	body := string(h.body(resp))
	// The rationale, not a bare 501.
	for _, want := range []string{"transcode", "av1", "/api/v1/playback/plan"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, body)
		}
	}
	// A session for a playback that cannot happen is state nobody will clean
	// up, and it would appear in "continue watching" for something nobody
	// watched.
	if after := h.count(t, `SELECT count(*) FROM consumption_sessions`); after != before {
		t.Errorf("a refused playback opened %d sessions", after-before)
	}
}

// The verb is derived from what the probe found, not from the extension: a
// .mkv holding only audio is a legitimate thing, and calling it "watching"
// puts it in the wrong row of every continue-watching list.
func TestTheVerbIsDerivedFromTheProbe(t *testing.T) {
	for _, tc := range []struct {
		name     string
		streams  string
		verb     string
		wantVerb string
	}{
		{"video plays as watching", `[{"index":0,"type":"video","codec":"h264"},
			{"index":1,"type":"audio","codec":"aac"}]`, "", "watch"},
		{"audio only plays as listening", `[{"index":0,"type":"audio","codec":"aac"}]`, "", "listen"},
		{"an explicit verb wins", `[{"index":0,"type":"audio","codec":"aac"}]`, "read", "read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t).seed()
			h.exec(`INSERT INTO blob_probes
				(blob_hash, container, format_long, duration_seconds, bitrate_bps,
				 streams, bytes_read, materialised, probed_at)
				VALUES (?, 'mov,mp4,m4a', '', 60.0, 1000000, ?, 4096, 0, ?)`,
				blob1Hash, tc.streams, seedTime)

			resp, got := h.startPlayback(t, asset1ID, device1ID, tc.verb)
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
			}
			var session struct {
				Verb string `json:"verb"`
			}
			if err := json.Unmarshal(
				h.body(h.get("/api/v1/consumption/sessions/"+got.SessionID)), &session); err != nil {
				t.Fatal(err)
			}
			if session.Verb != tc.wantVerb {
				t.Errorf("verb = %q, want %q", session.Verb, tc.wantVerb)
			}
		})
	}
}

// Unprobed media still plays. This is the ADR-0023 case end to end: a node
// with no ffprobe must still be able to play its library.
func TestUnprobedMediaStillPlays(t *testing.T) {
	h := newHarness(t).seed()
	resp, got := h.startPlayback(t, asset1ID, device1ID, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	if !got.Plan.has("no_probe") {
		t.Errorf("the plan does not declare that it is a guess: %+v", got.Plan.Reasons)
	}
	if got.ContentURL == "" {
		t.Error("no content_url for an unprobed asset")
	}
}

func TestStartPlaybackRefusals(t *testing.T) {
	h := newHarness(t).seed()
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"no asset", fmt.Sprintf(`{"device_id":%q}`, device1ID), 400},
		{"no device", fmt.Sprintf(`{"asset_id":%q}`, asset1ID), 400},
		{"an unknown verb", fmt.Sprintf(
			`{"asset_id":%q,"device_id":%q,"verb":"skim"}`, asset1ID, device1ID), 400},
		{"an unknown asset", fmt.Sprintf(`{"asset_id":"nope","device_id":%q}`, device1ID), 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(http.MethodPost, "/api/v1/playback", "", strings.NewReader(tc.body))
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d: %s", resp.StatusCode, tc.status, h.body(resp))
			}
		})
	}
}

// A credential that never expires is a credential that leaks permanently.
//
// The expiry is asserted here rather than assumed because a sabotage that
// passed `nil` for the expiry passed every other test in this file and the
// whole acceptance demo: `expires_at` was still rendered, as a zero time, and
// nothing looked at it. internal/auth already proves an expired token is
// rejected (M1-13); what was missing was proof that playback SETS one.
func TestThePlaybackCredentialExpires(t *testing.T) {
	h := newHarness(t).seed()

	_, got := h.startPlayback(t, asset1ID, device1ID, "")

	want := fixedTime.Add(2 * time.Hour).UTC()
	parsed, err := time.Parse(time.RFC3339Nano, got.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at %q is not a timestamp: %v", got.ExpiresAt, err)
	}
	if parsed.IsZero() {
		t.Fatal("expires_at is the zero time; the credential never expires")
	}
	if !parsed.Equal(want) {
		t.Errorf("expires_at = %s, want %s", parsed, want)
	}

	// And the stored credential carries it, rather than the response merely
	// claiming it does.
	var expires sql.NullString
	if err := h.db.Reader().QueryRowContext(t.Context(),
		`SELECT expires_at FROM api_tokens WHERE name = ?`,
		"playback "+got.SessionID).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if !expires.Valid || expires.String == "" {
		t.Error("the stored playback credential has no expiry")
	}
}
