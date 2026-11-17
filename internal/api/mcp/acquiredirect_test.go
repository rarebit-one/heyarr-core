//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package mcp_test

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/providers"
	"github.com/rarebit-one/heyarr-core/internal/worker"
)

// An operator handing over a URL as a want's direct release (#836, point 2).

// routedClient is a fake download client that declares its protocol, and —
// for the http-like one — that it fetches a plain http(s) URL.
type routedClient struct {
	*providers.Fake
	protocol acquisition.Protocol
	http     bool
}

func (c routedClient) Protocol() acquisition.Protocol { return c.protocol }
func (c routedClient) FetchesHTTP() bool              { return c.http }

// clients registers a torrent-like and an http-like client in the given order.
func clients(t *testing.T, httpFirst bool) (*providers.Registry, *providers.Fake, *providers.Fake) {
	t.Helper()
	torrent := providers.NewFake("transmission", providers.CapabilityDownload)
	direct := providers.NewFake("http", providers.CapabilityDownload)
	order := []routedClient{
		{torrent, acquisition.ProtocolTorrent, false},
		{direct, acquisition.ProtocolDirect, true},
	}
	if httpFirst {
		order[0], order[1] = order[1], order[0]
	}
	reg := providers.New(nil)
	for _, c := range order {
		if err := reg.Register(c); err != nil {
			t.Fatal(err)
		}
	}
	return reg, torrent, direct
}

// runQueuedGrab runs the grab_release job the hand-over queued, through the
// worker's own handler over its own catalog — the two roles share only the
// database (invariant 4).
func (h *harness) runQueuedGrab(reg *providers.Registry) {
	h.t.Helper()
	var payload string
	if err := h.db.Reader().QueryRow(
		`SELECT payload FROM jobs WHERE type = ?`, acquisition.GrabJobType).Scan(&payload); err != nil {
		h.t.Fatalf("no grab was queued: %v", err)
	}
	clock := &fixedClock{t: time.Date(2026, 8, 1, 0, 1, 0, 0, time.UTC)}
	eventLog, err := events.New(events.Options{Writer: h.db.Writer(), Reader: h.db.Reader(), Clock: clock})
	if err != nil {
		h.t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{
		DB: h.db, Events: eventLog, PeerName: "test", PeerSite: "test-site", Clock: clock,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := worker.GrabReleaseHandler(reg, cat, slog.New(slog.DiscardHandler))(
		h.t.Context(), jobs.Job{Type: acquisition.GrabJobType, Payload: json.RawMessage(payload)}); err != nil {
		h.t.Fatal(err)
	}
}

// transferProvider is the client get_acquisition_status names for the want.
func (h *harness) transferProvider(id string) (phase, provider string) {
	h.t.Helper()
	var out struct {
		Phase    string `json:"phase"`
		Transfer *struct {
			Provider string `json:"provider"`
		} `json:"transfer"`
	}
	h.call("", "get_acquisition_status", `{"desired_item_id":"`+id+`"}`).structured(h.t, &out)
	if out.Transfer == nil {
		return out.Phase, ""
	}
	return out.Phase, out.Transfer.Provider
}

// The issue's acceptance, with both clients configured in either order: a
// direct release handed over as a URL goes to http, a torrent release chosen
// from the want's candidates goes to the torrent client, and each status names
// the client that took it.
func TestAHandedOverURLGoesToHTTPAndATorrentToTheTorrentClientInEitherOrder(t *testing.T) {
	t.Parallel()
	const link = "https://files.example/roms/Some%20Game%20(Europe).zip?token=s3cret"
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"

	for _, order := range []struct {
		name      string
		httpFirst bool
	}{{"torrent_first", false}, {"http_first", true}} {
		t.Run(order.name+"/direct", func(t *testing.T) {
			t.Parallel()
			reg, torrent, direct := clients(t, order.httpFirst)
			h := newHarnessWith(t, false, reg)
			id := h.wantOne("")

			var out struct {
				CandidateID string `json:"candidate_id"`
				Provider    string `json:"provider"`
				Title       string `json:"title"`
				Accepted    bool   `json:"accepted"`
			}
			h.call("", "acquire_release",
				`{"desired_item_id":"`+id+`","url":"`+link+`"}`).structured(t, &out)
			if out.Provider != acquisition.OperatorProvider || !out.Accepted ||
				!strings.HasPrefix(out.CandidateID, "url:") || out.Title != "Some Game (Europe).zip" {
				t.Fatalf("acquire_release answered %+v", out)
			}
			if strings.Contains(out.CandidateID+out.Title, "s3cret") {
				t.Errorf("the link's token reached the answer: %+v", out)
			}

			var protocol, source, evaluation string
			var selected int
			if err := h.db.Reader().QueryRow(`SELECT protocol, source, selected, evaluation
				FROM release_candidates WHERE desired_item_id = ?`, id).Scan(
				&protocol, &source, &selected, &evaluation); err != nil {
				t.Fatal(err)
			}
			if protocol != string(acquisition.ProtocolDirect) || source != link || selected != 1 {
				t.Errorf("recorded protocol=%q source=%q selected=%d, want a selected direct release of the link",
					protocol, source, selected)
			}
			if !strings.Contains(evaluation, "release/direct-operator") {
				t.Errorf("the release does not say an operator supplied it: %s", evaluation)
			}

			h.runQueuedGrab(reg)
			if phase, provider := h.transferProvider(id); provider != "http" || phase != "queued" {
				t.Errorf("status: phase %q, transfer by %q; want queued, by http", phase, provider)
			}
			if got := torrent.Added(); len(got) != 0 {
				t.Errorf("the torrent client was handed the direct link (%d sources)", len(got))
			}
			if got := direct.Added(); len(got) != 1 || got[0].Reveal() != link {
				t.Errorf("the http client was handed %d sources, want the link once", len(got))
			}
		})

		t.Run(order.name+"/torrent", func(t *testing.T) {
			t.Parallel()
			reg, torrent, direct := clients(t, order.httpFirst)
			h := newHarnessWith(t, false, reg)
			id := h.wantOne("")
			h.exec(`UPDATE acquisition_state SET phase = 'candidates_found' WHERE desired_item_id = ?`, id)
			h.exec(`INSERT INTO release_candidates
				(id, desired_item_id, search_id, provider, candidate_id, title, attributes,
				 evaluation, accepted, score, terminal, selected, overridden, override_detail,
				 searched_at, created_at, source, protocol)
				VALUES ('rc-1', ?, 'search-1', 'torznab', 'infohash:0123', 'Some.Game.EUR', '{}',
				 '{"candidate_id":"infohash:0123","accepted":true,"score":0,"reasons":[]}',
				 1, 0, 0, 0, 0, '', ?, ?, ?, 'torrent')`, id, stamp, stamp, magnet)

			h.call("", "acquire_release", `{"desired_item_id":"`+id+`","candidate_id":"infohash:0123"}`)
			h.runQueuedGrab(reg)
			if phase, provider := h.transferProvider(id); provider != "transmission" || phase != "queued" {
				t.Errorf("status: phase %q, transfer by %q; want queued, by transmission", phase, provider)
			}
			if got := direct.Added(); len(got) != 0 {
				t.Errorf("the http client was handed the magnet (%d sources)", len(got))
			}
			if got := torrent.Added(); len(got) != 1 || got[0].Reveal() != magnet {
				t.Errorf("the torrent client was handed %d sources, want the magnet once", len(got))
			}
		})
	}
}

// What a hand-over refuses, and that a refusal records nothing: a magnet or a
// .torrent stays a torrent release (it is not fetched as a file), a node with
// no http client cannot fetch the link at all, and a want with something in
// flight is not overridden by one.
func TestAcquireReleaseRefusesAURLItCannotTakeAsADirectRelease(t *testing.T) {
	t.Parallel()
	torrentOnly := func(t *testing.T) *providers.Registry {
		t.Helper()
		reg := providers.New(nil)
		if err := reg.Register(routedClient{
			providers.NewFake("transmission", providers.CapabilityDownload), acquisition.ProtocolTorrent, false,
		}); err != nil {
			t.Fatal(err)
		}
		return reg
	}
	both := func(t *testing.T) *providers.Registry {
		t.Helper()
		reg, _, _ := clients(t, false)
		return reg
	}
	for _, tc := range []struct {
		name, url, want string
		reg             func(*testing.T) *providers.Registry
		phase           string
	}{
		{
			"a magnet", "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
			"torrent release", both, "",
		},
		{
			"a .torrent link", "https://tracker.example/dl/1/Some.Game.torrent?passkey=abc",
			"torrent release", both, "",
		},
		{"not http", "ftp://files.example/game.zip", "http(s) URL", both, ""},
		{"no http client", "https://files.example/game.zip", "no http download client", torrentOnly, ""},
		{"a want already downloading", "https://files.example/game.zip", "MISSING", both, "downloading"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarnessWith(t, false, tc.reg(t))
			id := h.wantOne("")
			if tc.phase != "" {
				h.exec(`UPDATE acquisition_state SET phase = ? WHERE desired_item_id = ?`, tc.phase, id)
			}
			resp := h.call("", "acquire_release", `{"desired_item_id":"`+id+`","url":"`+tc.url+`"}`)
			if resp.Body.Error == nil {
				t.Fatalf("the hand-over was accepted: %s", resp.Raw)
			}
			if !strings.Contains(resp.Body.Error.Message, tc.want) {
				t.Errorf("the refusal does not say %q: %q", tc.want, resp.Body.Error.Message)
			}
			if strings.Contains(resp.Body.Error.Message, "passkey=abc") {
				t.Errorf("the refusal echoes the link: %q", resp.Body.Error.Message)
			}
			var n int
			if err := h.db.Reader().QueryRow(`SELECT
				(SELECT count(*) FROM release_candidates WHERE desired_item_id = ?) +
				(SELECT count(*) FROM jobs WHERE type = ?)`, id, acquisition.GrabJobType).Scan(&n); err != nil &&
				err != sql.ErrNoRows {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("a refused hand-over left %d candidates or grabs behind", n)
			}
		})
	}
}

// A hand-over names one release: a candidate or a URL, never both.
func TestAcquireReleaseTakesACandidateOrAURLNotBoth(t *testing.T) {
	t.Parallel()
	reg, _, _ := clients(t, false)
	h := newHarnessWith(t, false, reg)
	id := h.wantOne("")
	resp := h.call("", "acquire_release",
		`{"desired_item_id":"`+id+`","candidate_id":"c1","url":"https://files.example/game.zip"}`)
	if resp.Body.Error == nil || !strings.Contains(resp.Body.Error.Message, "not both") {
		t.Fatalf("both a candidate and a url were accepted, or refused unclearly: %s", resp.Raw)
	}
}
