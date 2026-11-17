package downloads

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
)

// #882: a grab was added stopped whenever the instance's start-added-torrents
// was off, because torrent-add sent no `paused`; and it landed wherever the
// session's download directory last pointed, because it sent no download-dir.
//
// These drive a TEST DOUBLE of the RPC that keeps a queue and applies
// Transmission's own rules to it: a torrent-add with no `paused` takes the
// session's start-added-torrents, an add of a torrent already held answers
// torrent-duplicate and leaves it as it is, and torrent-start starts one. It is
// a double, named as one (see health_test.go): the capture is of a healthy
// instance with the setting on, and turning it off to record a stranding is not
// a change to make to a live client.

// stubTorrent is one queued transfer in the double.
type stubTorrent struct {
	ID                 int64    `json:"id"`
	Name               string   `json:"name"`
	HashString         string   `json:"hashString"`
	Status             int      `json:"status"`
	PercentDone        float64  `json:"percentDone"`
	DownloadDir        string   `json:"downloadDir"`
	Labels             []string `json:"labels"`
	Error              int      `json:"error"`
	ErrorString        string   `json:"errorString"`
	ActivityDate       int64    `json:"activityDate"`
	DownloadedEver     int64    `json:"downloadedEver"`
	SecondsDownloading int64    `json:"secondsDownloading"`
}

type transmissionDouble struct {
	t          *testing.T
	startAdded bool

	mu      sync.Mutex
	queue   []*stubTorrent
	addArgs map[string]any
	started []int64
}

func newTransmissionDouble(t *testing.T, startAdded bool, seed ...*stubTorrent) (*transmissionDouble, *httptest.Server) {
	t.Helper()
	d := &transmissionDouble{t: t, startAdded: startAdded, queue: seed}
	srv := httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(srv.Close)
	return d, srv
}

func (d *transmissionDouble) serve(w http.ResponseWriter, r *http.Request) {
	const sid = "double-session-id"
	if r.Header.Get("X-Transmission-Session-Id") != sid {
		w.Header().Set("X-Transmission-Session-Id", sid)
		w.WriteHeader(http.StatusConflict)
		return
	}
	var env struct {
		Method    string         `json:"method"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := decodeJSONBody(r, &env); err != nil {
		d.t.Errorf("the double could not decode the RPC envelope: %v", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	var out map[string]any
	switch env.Method {
	case "session-get":
		out = map[string]any{
			"version": "4.0.6", "rpc-version": 17,
			"download-dir": "/downloads/complete/games/snes", "start-added-torrents": d.startAdded,
		}
	case "torrent-add":
		d.addArgs = env.Arguments
		out = d.add(env.Arguments)
	case "torrent-get":
		var list []*stubTorrent
		ids, filtered := env.Arguments["ids"].([]any)
		for _, tor := range d.queue {
			if !filtered || containsFold(ids, tor.HashString) {
				list = append(list, tor)
			}
		}
		out = map[string]any{"torrents": list}
	case "torrent-start":
		for _, raw := range env.Arguments["ids"].([]any) {
			id := int64(raw.(float64))
			d.started = append(d.started, id)
			for _, tor := range d.queue {
				if tor.ID == id {
					tor.Status, tor.ActivityDate = statusDownloading, fixedNow.Unix()
				}
			}
		}
	default:
		d.t.Errorf("the double was sent %s, which nothing here should call", env.Method)
	}
	writeRPC(w, out)
}

// add applies Transmission's rules: a known infohash is a duplicate, left as
// it is; a new one is stopped when `paused` says so, or — absent — when the
// session's start-added-torrents is off.
func (d *transmissionDouble) add(args map[string]any) map[string]any {
	fn, _ := args["filename"].(string)
	hash := magnetInfoHash(fn)
	for _, tor := range d.queue {
		if strings.EqualFold(tor.HashString, hash) {
			return map[string]any{"torrent-duplicate": map[string]any{"id": tor.ID, "name": tor.Name, "hashString": tor.HashString}}
		}
	}
	paused, given := args["paused"].(bool)
	if !given {
		paused = !d.startAdded
	}
	tor := &stubTorrent{
		ID: int64(len(d.queue) + 1), Name: "grab", HashString: hash,
		Status: statusDownloading, DownloadDir: "/downloads/complete/games/snes",
	}
	if paused {
		tor.Status = statusStopped
	}
	if dir, ok := args["download-dir"].(string); ok {
		tor.DownloadDir = dir
	}
	tor.Labels = append(tor.Labels, asStrings(args["labels"])...)
	d.queue = append(d.queue, tor)
	return map[string]any{"torrent-added": map[string]any{"id": tor.ID, "name": tor.Name, "hashString": tor.HashString}}
}

func (d *transmissionDouble) startedIDs() []int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]int64(nil), d.started...)
}

func (d *transmissionDouble) byHash(hash string) stubTorrent {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, tor := range d.queue {
		if tor.HashString == hash {
			return *tor
		}
	}
	d.t.Fatalf("the double holds no %s", hash)
	return stubTorrent{}
}

func writeRPC(w http.ResponseWriter, args map[string]any) {
	if args == nil {
		args = map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	body, _ := json.Marshal(map[string]any{"result": "success", "arguments": args})
	_, _ = w.Write(body)
}

func containsFold(list []any, s string) bool {
	for _, v := range list {
		if str, ok := v.(string); ok && strings.EqualFold(str, s) {
			return true
		}
	}
	return false
}

func asStrings(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func doubleClient(t *testing.T, url, downloadDir string) *Client {
	t.Helper()
	c, err := New(Options{
		Name: "test", Endpoint: url, Label: "heyarr", DownloadDir: downloadDir,
		Now: func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	if h := c.Check(context.Background()); !h.Healthy {
		t.Fatalf("the double did not come up healthy: %s", h.Detail)
	}
	return c
}

func magnet(hash string) secret.Value {
	return secret.Value("magnet:?xt=urn:btih:" + hash + "&dn=grab")
}

// 🔴 With start-added-torrents off, a grab still ends up downloading: the add
// says `paused: false` instead of leaving it to the instance.
func TestAGrabDownloadsWhenTheInstanceAddsStopped(t *testing.T) {
	d, srv := newTransmissionDouble(t, false)
	c := doubleClient(t, srv.URL, "")
	hash := strings.Repeat("a", 40)

	if _, err := c.Add(context.Background(), magnet(hash)); err != nil {
		t.Fatal(err)
	}
	if p, ok := d.addArgs["paused"].(bool); !ok || p {
		t.Fatalf("torrent-add sent paused=%v (present=%v), want false", d.addArgs["paused"], ok)
	}
	if got := d.byHash(hash).Status; got != statusDownloading {
		t.Fatalf("the grab is in status %d, want downloading (%d)", got, statusDownloading)
	}
}

// download_dir: unset sends nothing and the transfer takes the session's
// default, as before; set, it is sent and the transfer lands there.
func TestTheDownloadDirIsSentOnlyWhenConfigured(t *testing.T) {
	for _, tc := range []struct{ configured, want string }{
		{"", "/downloads/complete/games/snes"},
		{"/downloads/complete/heyarr", "/downloads/complete/heyarr"},
	} {
		d, srv := newTransmissionDouble(t, true)
		c := doubleClient(t, srv.URL, tc.configured)
		hash := strings.Repeat("b", 40)
		if _, err := c.Add(context.Background(), magnet(hash)); err != nil {
			t.Fatal(err)
		}
		if _, sent := d.addArgs["download-dir"]; sent != (tc.configured != "") {
			t.Errorf("configured %q: download-dir sent = %v", tc.configured, sent)
		}
		if got := d.byHash(hash).DownloadDir; got != tc.want {
			t.Errorf("configured %q: the transfer landed in %q, want %q", tc.configured, got, tc.want)
		}
	}
}

// A grab of a transfer already held answers torrent-duplicate, and Transmission
// ignores `paused` for it. One of ours that is stopped short of complete is
// started — that is how a transfer stranded before the fix recovers on the next
// grab. Not ours, errored or complete, it is left as it is.
func TestADuplicateGrabStartsOnlyOurStoppedIncompleteTransfer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tor     stubTorrent
		started bool
	}{
		{"ours, stopped, incomplete", stubTorrent{Labels: []string{"heyarr"}, PercentDone: 0.2, ActivityDate: 1}, true},
		{"not ours", stubTorrent{Labels: []string{"operator"}}, false},
		{"errored", stubTorrent{Labels: []string{"heyarr"}, Error: 3, ErrorString: "No data found"}, false},
		{"complete", stubTorrent{Labels: []string{"heyarr"}, PercentDone: 1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hash := strings.Repeat("c", 40)
			seed := tc.tor
			seed.ID, seed.Name, seed.HashString, seed.Status = 7, "stranded", hash, statusStopped
			d, srv := newTransmissionDouble(t, false, &seed)
			c := doubleClient(t, srv.URL, "")

			got, err := c.Add(context.Background(), magnet(hash))
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != hash {
				t.Fatalf("the duplicate grab returned %q, want the existing transfer", got.ID)
			}
			if started := len(d.startedIDs()) == 1; started != tc.started {
				t.Fatalf("started = %v (torrent-start ids %v), want %v", started, d.startedIDs(), tc.started)
			}
		})
	}
}

// The poll's half: Transfers reports a transfer Held only when it is ours,
// stopped, incomplete, without an error, and has NEVER RUN — no activity,
// nothing downloaded, no time downloading. A transfer an operator paused after
// it started has run, and is not Heyarr's to restart.
func TestOnlyATransferThatNeverRanIsHeld(t *testing.T) {
	never := stubTorrent{Labels: []string{"heyarr"}, Status: statusStopped}
	cases := map[string]struct {
		edit func(*stubTorrent)
		held bool
	}{
		"added stopped, never ran":      {func(*stubTorrent) {}, true},
		"paused after activity":         {func(s *stubTorrent) { s.ActivityDate = fixedNow.Unix() }, false},
		"paused after fetching":         {func(s *stubTorrent) { s.DownloadedEver = 1 << 20 }, false},
		"paused after time downloading": {func(s *stubTorrent) { s.SecondsDownloading = 30 }, false},
		"stopped by an error":           {func(s *stubTorrent) { s.Error, s.ErrorString = 3, "No data found" }, false},
		"complete":                      {func(s *stubTorrent) { s.PercentDone = 1 }, false},
		"downloading":                   {func(s *stubTorrent) { s.Status = statusDownloading }, false},
		"queued in the client's queue":  {func(s *stubTorrent) { s.Status = 3 }, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tor := never
			tor.ID, tor.HashString = 1, strings.Repeat("d", 40)
			tc.edit(&tor)
			_, srv := newTransmissionDouble(t, false, &tor)
			c := doubleClient(t, srv.URL, "")

			list, err := c.Transfers(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 1 {
				t.Fatalf("%d transfers, want 1", len(list))
			}
			if list[0].Held != tc.held {
				t.Fatalf("held = %v, want %v", list[0].Held, tc.held)
			}
		})
	}
}

// Start starts a held transfer, does nothing to one that is no longer held,
// and refuses one that is not ours — the same refusal Remove makes.
func TestStartStartsOnlyOurHeldTransfer(t *testing.T) {
	heldHash, ranHash, foreignHash := strings.Repeat("e", 40), strings.Repeat("f", 40), strings.Repeat("1", 40)
	d, srv := newTransmissionDouble(t, false,
		&stubTorrent{ID: 1, HashString: heldHash, Labels: []string{"heyarr"}, Status: statusStopped},
		&stubTorrent{ID: 2, HashString: ranHash, Labels: []string{"heyarr"}, Status: statusStopped, ActivityDate: 1},
		&stubTorrent{ID: 3, HashString: foreignHash, Labels: []string{"operator"}, Status: statusStopped},
	)
	c := doubleClient(t, srv.URL, "")
	ctx := context.Background()

	if err := c.Start(ctx, heldHash); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx, ranHash); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx, foreignHash); !errors.Is(err, ErrNotOurs) {
		t.Fatalf("starting a foreign transfer returned %v, want ErrNotOurs", err)
	}
	if got := d.startedIDs(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("torrent-start ids %v, want only the held transfer (1)", got)
	}
	if got := d.byHash(heldHash).Status; got != statusDownloading {
		t.Fatalf("the held transfer is in status %d after Start, want downloading", got)
	}
}

// start-added-torrents off is a WARNING in the health detail, not unhealthy:
// Heyarr starts its own grabs regardless, and an operator looking at a stopped
// transfer should find the setting named here.
func TestStartAddedTorrentsOffIsAHealthWarning(t *testing.T) {
	for _, startAdded := range []bool{true, false} {
		_, srv := newTransmissionDouble(t, startAdded)
		c, err := New(Options{Name: "test", Endpoint: srv.URL, Now: func() time.Time { return fixedNow }})
		if err != nil {
			t.Fatal(err)
		}
		h := c.Check(context.Background())
		if !h.Healthy {
			t.Fatalf("start-added-torrents=%v reported unhealthy: %s", startAdded, h.Detail)
		}
		if warned := strings.Contains(h.Detail, "start-added-torrents is off"); warned == startAdded {
			t.Errorf("start-added-torrents=%v: detail %q", startAdded, h.Detail)
		}
	}
}
