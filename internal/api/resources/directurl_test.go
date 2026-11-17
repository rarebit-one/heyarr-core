//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// POST /desired/{id}/select with a url: an operator's direct release (#836).
// The routing to the http client and the status that names it are proved end
// to end in internal/api/mcp, through the same AcquireURL; these hold the HTTP
// surface to it.

// httpLike is a fake download client that fetches a plain http(s) URL.
type httpLike struct{ *providers.Fake }

func (httpLike) Protocol() acquisition.Protocol { return acquisition.ProtocolDirect }
func (httpLike) FetchesHTTP() bool              { return true }

func withHTTPClient(t *testing.T) harnessOption {
	t.Helper()
	reg := providers.New(nil)
	if err := reg.Register(httpLike{providers.NewFake("http", providers.CapabilityDownload)}); err != nil {
		t.Fatal(err)
	}
	return withProviders(reg)
}

// restWant puts the seeded want that holds nothing into MISSING, the one state
// a direct release may be handed over in.
func (h *harness) restWant() string {
	h.exec(`UPDATE acquisition_state SET phase = 'idle' WHERE desired_item_id = ?`, desired2ID)
	return desired2ID
}

func TestAURLIsSelectedAsTheWantsDirectRelease(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withHTTPClient(t)).seed()
	id := h.restWant()

	resp := h.doStable(http.MethodPost, "/api/v1/desired/"+id+"/select",
		strings.NewReader(`{"url":"https://files.example/roms/game.zip?token=s3cret"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, h.body(resp))
	}
	body := h.body(resp)
	var chosen struct {
		CandidateID string `json:"candidate_id"`
		Provider    string `json:"provider"`
		Title       string `json:"title"`
		Accepted    bool   `json:"accepted"`
		Selected    bool   `json:"selected"`
		Reasons     []struct {
			Rule string `json:"rule"`
		} `json:"reasons"`
	}
	if err := json.Unmarshal(body, &chosen); err != nil {
		t.Fatal(err)
	}
	if chosen.Provider != "operator" || chosen.Title != "game.zip" || !chosen.Accepted || !chosen.Selected {
		t.Errorf("selected %+v", chosen)
	}
	if len(chosen.Reasons) != 1 || chosen.Reasons[0].Rule != "release/direct-operator" {
		t.Errorf("reasons = %+v, want the operator's provenance alone", chosen.Reasons)
	}
	if strings.Contains(string(body), "s3cret") {
		t.Errorf("the link's token reached the response: %s", body)
	}
	if n := h.countRows(t, `SELECT count(*) FROM release_candidates
		WHERE desired_item_id = ? AND protocol = 'direct' AND selected = 1`, id); n != 1 {
		t.Errorf("%d selected direct releases recorded, want 1", n)
	}
	if n := h.countRows(t, `SELECT count(*) FROM acquisition_state
		WHERE desired_item_id = ? AND phase = 'selected'`, id); n != 1 {
		t.Error("the want did not reach SELECTED")
	}
	if n := h.countRows(t, `SELECT count(*) FROM jobs WHERE type = 'grab_release'`); n != 1 {
		t.Errorf("%d grabs queued, want 1", n)
	}
}

func TestDirectURLRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body string
		noHTTP     bool
		held       bool
		status     int
		want       string
	}{
		{
			"a magnet", `{"url":"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}`,
			false, false, 400, "select its candidate instead",
		},
		{
			"a .torrent", `{"url":"https://tracker.example/1/game.torrent?passkey=abc"}`,
			false, false, 400, "torrent release",
		},
		{"not http", `{"url":"file:///srv/game.zip"}`, false, false, 400, "http(s) URL"},
		{
			"both", `{"url":"https://files.example/game.zip","candidate_id":"good"}`,
			false, false, 400, "not both",
		},
		{
			"no http client", `{"url":"https://files.example/game.zip"}`,
			true, false, 400, "no http download client",
		},
		{
			"a want that holds bytes", `{"url":"https://files.example/game.zip"}`,
			false, true, 409, "PLACEMENT_CONVERGING",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := []harnessOption{withHTTPClient(t)}
			if tc.noHTTP {
				opts = []harnessOption{withProviders(providers.New(nil))}
			}
			h := newHarness(t, opts...).seed()
			id := h.restWant()
			if tc.held {
				id = desired1ID // idle and managed: it holds satisfying bytes
			}
			resp := h.doStable(http.MethodPost, "/api/v1/desired/"+id+"/select", strings.NewReader(tc.body))
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.status, h.body(resp))
			}
			if detail := problemDetail(t, h, resp); !strings.Contains(detail, tc.want) ||
				strings.Contains(detail, "passkey") {
				t.Errorf("the refusal should mention %q and not the link; got: %s", tc.want, detail)
			}
			if n := h.countRows(t, `SELECT count(*) FROM jobs WHERE type = 'grab_release'`); n != 0 {
				t.Errorf("a refused hand-over queued %d grabs", n)
			}
		})
	}
}
