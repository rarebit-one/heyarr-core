package downloads

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The qBittorrent client, driven against a fake of its Web API v2.
//
// ADR-0026: a download client's merge-path test is a fake of its protocol, and
// its live exercise is opt-in against a real instance (TestLiveQBittorrent).
// The fake here answers with the shapes qBittorrent actually returns — "Ok."
// text from add, a JSON array from torrents/info, a session cookie from login —
// so the client's real transport and parsing are exercised, not stubbed.

const (
	qbUser   = "admin"
	qbPass   = "hunter2"
	qbSID    = "test-session-id"
	qbCat    = "heyarr"
	qbMagnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=Beacon+Hill"
	qbHash   = "0123456789abcdef0123456789abcdef01234567"
)

// fakeQB is a configurable stand-in for a qBittorrent Web API.
type fakeQB struct {
	requireAuth   bool
	webapiVersion string
	appVersion    string
	addFails      bool
	torrents      []qbTorrent
	addedURLs     []string // sources handed to torrents/add
	// registerHash models qBittorrent fetching a .torrent (which carries no
	// infohash in the source): when set, a non-magnet add registers a torrent
	// with this hash, so a category-diff resolve has something new to find.
	registerHash string
}

func newFakeQB() *fakeQB {
	return &fakeQB{requireAuth: true, webapiVersion: "2.9.2", appVersion: "v4.6.0"}
}

func (f *fakeQB) authed(r *http.Request) bool {
	if !f.requireAuth {
		return true
	}
	c, err := r.Cookie("SID")
	return err == nil && c.Value == qbSID
}

func (f *fakeQB) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if f.requireAuth && (r.FormValue("username") != qbUser || r.FormValue("password") != qbPass) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("Fails."))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: qbSID})
		_, _ = w.Write([]byte("Ok."))
	})

	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !f.authed(r) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("/api/v2/app/webapiVersion", guard(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(f.webapiVersion))
	}))
	mux.HandleFunc("/api/v2/app/version", guard(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(f.appVersion))
	}))

	mux.HandleFunc("/api/v2/torrents/add", guard(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if f.addFails {
			_, _ = w.Write([]byte("Fails."))
			return
		}
		src := r.FormValue("urls")
		cat := r.FormValue("category")
		f.addedURLs = append(f.addedURLs, src)
		if h := magnetInfoHash(src); h != "" {
			f.torrents = append(f.torrents, qbTorrent{
				Hash: h, Name: magnetName(src), State: "downloading",
				Progress: 0, Size: 1000, Category: cat, SavePath: "/downloads",
			})
		} else if f.registerHash != "" {
			f.torrents = append(f.torrents, qbTorrent{
				Hash: f.registerHash, Name: "fetched", State: "downloading",
				Progress: 0, Size: 10, Category: cat, SavePath: "/downloads",
			})
		}
		_, _ = w.Write([]byte("Ok."))
	}))

	mux.HandleFunc("/api/v2/torrents/info", guard(func(w http.ResponseWriter, r *http.Request) {
		cat := r.URL.Query().Get("category")
		hashes := r.URL.Query().Get("hashes")
		out := make([]qbTorrent, 0, len(f.torrents))
		for _, tr := range f.torrents {
			if cat != "" && !strings.EqualFold(tr.Category, cat) {
				continue
			}
			if hashes != "" && !strings.EqualFold(tr.Hash, hashes) {
				continue
			}
			out = append(out, tr)
		}
		_ = json.NewEncoder(w).Encode(out)
	}))

	mux.HandleFunc("/api/v2/torrents/delete", guard(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		del := r.FormValue("hashes")
		kept := f.torrents[:0]
		for _, tr := range f.torrents {
			if !strings.EqualFold(tr.Hash, del) {
				kept = append(kept, tr)
			}
		}
		f.torrents = kept
		w.WriteHeader(http.StatusOK)
	}))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func qbClient(t *testing.T, endpoint string) *QBClient {
	t.Helper()
	c, err := NewQBittorrent(QBOptions{
		Name: "qb", Endpoint: endpoint, Username: qbUser, Password: qbPass, Label: qbCat,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestQBCheckHealthy(t *testing.T) {
	f := newFakeQB()
	c := qbClient(t, f.server(t).URL)
	h := c.Check(context.Background())
	if !h.Healthy {
		t.Fatalf("expected healthy, got %+v", h)
	}
	if h.Version != "v4.6.0" {
		t.Errorf("version = %q, want the app version v4.6.0", h.Version)
	}
}

func TestQBCheckWrongCredential(t *testing.T) {
	f := newFakeQB()
	srv := f.server(t)
	c, _ := NewQBittorrent(QBOptions{Name: "qb", Endpoint: srv.URL, Username: qbUser, Password: "wrong", Label: qbCat})
	h := c.Check(context.Background())
	if h.Healthy {
		t.Fatal("a wrong credential must not be healthy")
	}
	if !strings.Contains(h.Detail, "credential") {
		t.Errorf("detail should name the credential, got %q", h.Detail)
	}
}

func TestQBCheckUnreachable(t *testing.T) {
	// A port that refuses, the ADR-0025 case: unhealthy, not a startup failure.
	c := qbClient(t, "http://127.0.0.1:9")
	h := c.Check(context.Background())
	if h.Healthy {
		t.Fatal("an unreachable instance must not be healthy")
	}
}

func TestQBCheckWebAPITooOld(t *testing.T) {
	f := newFakeQB()
	f.webapiVersion = "1.9" // below the 2.0 floor
	c := qbClient(t, f.server(t).URL)
	h := c.Check(context.Background())
	if h.Healthy {
		t.Fatal("a Web API below the floor must not be healthy")
	}
	if !strings.Contains(h.Detail, "1.9") || !strings.Contains(h.Detail, qbMinAPIVersion) {
		t.Errorf("detail must name both versions, got %q", h.Detail)
	}
}

func TestQBCheckAuthBypassed(t *testing.T) {
	// An instance with the Web UI auth turned off (a localhost whitelist): no
	// credential configured, no cookie required. A supported deployment.
	f := newFakeQB()
	f.requireAuth = false
	c, _ := NewQBittorrent(QBOptions{Name: "qb", Endpoint: f.server(t).URL, Label: qbCat})
	if h := c.Check(context.Background()); !h.Healthy {
		t.Fatalf("an auth-bypassed instance should be healthy, got %+v", h)
	}
}

func TestQBAddMagnetResolvesByHash(t *testing.T) {
	f := newFakeQB()
	c := qbClient(t, f.server(t).URL)
	tr, err := c.Add(context.Background(), secret.Value(qbMagnet))
	if err != nil {
		t.Fatal(err)
	}
	if tr.ID != qbHash {
		t.Fatalf("transfer id = %q, want the infohash %q", tr.ID, qbHash)
	}
	if len(f.addedURLs) != 1 || f.addedURLs[0] != qbMagnet {
		t.Errorf("the magnet was not handed to torrents/add: %v", f.addedURLs)
	}
}

func TestQBAddRefusesNonTorrent(t *testing.T) {
	f := newFakeQB()
	c := qbClient(t, f.server(t).URL)
	for _, src := range []string{
		"https://example.test/file.mkv",
		"https://example.test/release.nzb",
		"http://example.test/download?id=7",
	} {
		_, err := c.Add(context.Background(), secret.Value(src))
		if !errors.Is(err, ErrNotTorrentSource) {
			t.Errorf("Add(%q) = %v, want ErrNotTorrentSource (so it composes)", src, err)
		}
	}
	if len(f.addedURLs) != 0 {
		t.Errorf("a refused source must never reach torrents/add, got %v", f.addedURLs)
	}
}

func TestQBAddTorrentURLResolvesByCategoryDiff(t *testing.T) {
	f := newFakeQB()
	// A pre-existing torrent in our category, so the resolve must pick the NEW
	// one, not this.
	f.torrents = append(f.torrents, qbTorrent{Hash: "aaaa", Name: "old", Category: qbCat})
	// qBittorrent fetches the .torrent and it appears AFTER the add.
	f.registerHash = "bbbb"
	c := qbClient(t, f.server(t).URL)

	tr, err := c.Add(context.Background(), secret.Value("https://tracker.test/x.torrent"))
	if err != nil {
		t.Fatal(err)
	}
	if tr.ID != "bbbb" {
		t.Fatalf("transfer id = %q, want the newly-appeared hash bbbb", tr.ID)
	}
}

func TestQBTransfersFilterAndMap(t *testing.T) {
	f := newFakeQB()
	f.torrents = []qbTorrent{
		{Hash: "ours1", Name: "Ours Complete", State: "stalledUP", Progress: 1, Size: 500, Completed: 500, ContentPath: "/downloads/ours.mkv", Category: qbCat},
		{Hash: "ours2", Name: "Ours Downloading", State: "downloading", Progress: 0.5, Size: 800, Completed: 400, Category: qbCat},
		{Hash: "theirs", Name: "Operator's own", State: "uploading", Progress: 1, Category: "movies"},
	}
	c := qbClient(t, f.server(t).URL)
	got, err := c.Transfers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d transfers, want only our 2 (the operator's is invisible)", len(got))
	}
	byID := map[string]providers.Transfer{}
	for _, tr := range got {
		byID[tr.ID] = tr
	}
	done := byID["ours1"]
	if !done.Done || done.Path != "/downloads/ours.mkv" || done.BytesDone != 500 {
		t.Errorf("completed transfer wrong: %+v", done)
	}
	inflight := byID["ours2"]
	if inflight.Done || inflight.Path != "" {
		t.Errorf("an in-flight transfer must not be done and must report no path yet: %+v", inflight)
	}
}

func TestQBRemoveRefusesForeign(t *testing.T) {
	f := newFakeQB()
	f.torrents = []qbTorrent{{Hash: "theirs", Name: "not ours", Category: "movies"}}
	c := qbClient(t, f.server(t).URL)
	err := c.Remove(context.Background(), "theirs", true)
	if !errors.Is(err, ErrNotOurs) {
		t.Fatalf("removing a foreign transfer = %v, want ErrNotOurs", err)
	}
	if len(f.torrents) != 1 {
		t.Error("a refused remove must not delete anything")
	}
}

func TestQBRemoveOurs(t *testing.T) {
	f := newFakeQB()
	f.torrents = []qbTorrent{{Hash: "ours", Name: "ours", Category: qbCat}}
	c := qbClient(t, f.server(t).URL)
	if err := c.Remove(context.Background(), "ours", true); err != nil {
		t.Fatal(err)
	}
	if len(f.torrents) != 0 {
		t.Error("removing our transfer should delete it")
	}
}

func TestQBAddFails(t *testing.T) {
	f := newFakeQB()
	f.addFails = true
	c := qbClient(t, f.server(t).URL)
	_, err := c.Add(context.Background(), secret.Value(qbMagnet))
	if !errors.Is(err, ErrRPCFailure) {
		t.Fatalf("a rejected source = %v, want ErrRPCFailure", err)
	}
}

func TestIsTorrentSource(t *testing.T) {
	yes := []string{"magnet:?xt=urn:btih:abc", "https://x.test/a.torrent", "https://x.test/a.torrent?passkey=z", "MAGNET:?xt=urn:btih:AB"}
	no := []string{"https://x.test/a.mkv", "https://x.test/a.nzb", "http://x.test/get?id=1", ""}
	for _, s := range yes {
		if !isTorrentSource(s) {
			t.Errorf("isTorrentSource(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if isTorrentSource(s) {
			t.Errorf("isTorrentSource(%q) = true, want false", s)
		}
	}
}

func TestMagnetInfoHash(t *testing.T) {
	if got := magnetInfoHash(qbMagnet); got != qbHash {
		t.Errorf("magnetInfoHash = %q, want %q", got, qbHash)
	}
	// A base32 (v1) or non-40-char hash is left unresolved rather than guessed.
	if got := magnetInfoHash("magnet:?xt=urn:btih:MFRGGZDFMZTWQ2LK"); got != "" {
		t.Errorf("a base32 hash should be left unresolved, got %q", got)
	}
	if got := magnetInfoHash("https://x.test/a.torrent"); got != "" {
		t.Errorf("a non-magnet has no infohash, got %q", got)
	}
}

func TestCompareDotted(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.9.2", "2.0", 1},
		{"2.0", "2.0", 0},
		{"1.9", "2.0", -1},
		{"2.10", "2.9", 1},
		{"2.9.1beta", "2.9.1", 0},
	}
	for _, tc := range cases {
		if got := compareDotted(tc.a, tc.b); got != tc.want {
			t.Errorf("compareDotted(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
