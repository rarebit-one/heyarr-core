package downloads

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The NZBGet client, driven against a fake of its JSON-RPC API.
//
// ADR-0026: a download client's merge-path test is a fake of its protocol, and
// its live exercise is opt-in against a real instance (TestLiveNZBGet). The fake
// here answers with the shapes NZBGet actually returns — a version string, a
// listgroups array with split Lo/Hi sizes, a history array with a "CATEGORY/…"
// status and a DestDir, an append returning the new integer NZBID, and a 401 for
// a wrong basic credential — so the client's real transport and parsing are
// exercised, not stubbed.

const (
	nzbUser = "nzbget"
	nzbPass = "tegbzn6789"
	nzbCat  = "heyarr"
	nzbURL  = "https://indexer.test/getnzb/abc.nzb?apikey=secret"
)

// fakeNZBGet is a configurable stand-in for an NZBGet JSON-RPC API.
type fakeNZBGet struct {
	requireAuth bool
	version     string
	addFails    bool
	groups      []nzbGroup
	history     []nzbHistory
	addedURLs   []string // the NZBContent (nzb URL) handed to append
	nextID      int64    // the NZBID append assigns
}

func newFakeNZBGet() *fakeNZBGet {
	return &fakeNZBGet{requireAuth: true, version: "21.1", nextID: 41}
}

func (f *fakeNZBGet) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.requireAuth {
			u, p, ok := r.BasicAuth()
			if !ok || u != nzbUser || p != nzbPass {
				http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
				return
			}
		}
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "version":
			writeResult(w, f.version)
		case "listgroups":
			writeResult(w, f.groups)
		case "history":
			writeResult(w, f.history)
		case "append":
			if f.addFails {
				writeResult(w, 0)
				return
			}
			var filename, content, category string
			_ = json.Unmarshal(req.Params[0], &filename)
			_ = json.Unmarshal(req.Params[1], &content)
			_ = json.Unmarshal(req.Params[2], &category)
			f.addedURLs = append(f.addedURLs, content)
			f.nextID++
			// NZBGet reports the NZBName as NZBFilename with a trailing .nzb
			// stripped, which is exactly what the client matches a re-add on.
			f.groups = append(f.groups, nzbGroup{
				NZBID: f.nextID, NZBName: strings.TrimSuffix(filename, ".nzb"),
				Category: category, Status: "DOWNLOADING",
				FileSizeLo: 100 * 1024 * 1024, RemainingSizeLo: 100 * 1024 * 1024,
			})
			writeResult(w, f.nextID)
		case "editqueue":
			var command string
			var ids []int64
			_ = json.Unmarshal(req.Params[0], &command)
			_ = json.Unmarshal(req.Params[3], &ids)
			f.edit(command, ids)
			writeResult(w, true)
		default:
			http.Error(w, "unknown method", http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeResult(w http.ResponseWriter, result any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"version": "1.1", "result": result, "id": 1})
}

func (f *fakeNZBGet) edit(command string, ids []int64) {
	remove := func(id int64) bool {
		for _, want := range ids {
			if want == id {
				return true
			}
		}
		return false
	}
	if strings.HasPrefix(command, "Group") {
		kept := f.groups[:0]
		for _, g := range f.groups {
			if !remove(g.NZBID) {
				kept = append(kept, g)
			}
		}
		f.groups = kept
		return
	}
	kept := f.history[:0]
	for _, h := range f.history {
		if !remove(h.NZBID) {
			kept = append(kept, h)
		}
	}
	f.history = kept
}

func nzbClient(t *testing.T, endpoint string) *NZBGetClient {
	t.Helper()
	c, err := NewNZBGet(NZBGetOptions{
		Name: "nzb", Endpoint: endpoint, Username: nzbUser, Password: nzbPass, Label: nzbCat,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNZBGetCheckHealthy(t *testing.T) {
	f := newFakeNZBGet()
	c := nzbClient(t, f.server(t).URL)
	h := c.Check(context.Background())
	if !h.Healthy {
		t.Fatalf("expected healthy, got %+v", h)
	}
	if h.Version != "21.1" {
		t.Errorf("version = %q, want the app version 21.1", h.Version)
	}
}

func TestNZBGetCheckWrongCredential(t *testing.T) {
	f := newFakeNZBGet()
	srv := f.server(t)
	c, _ := NewNZBGet(NZBGetOptions{Name: "nzb", Endpoint: srv.URL, Username: nzbUser, Password: "wrong", Label: nzbCat})
	h := c.Check(context.Background())
	if h.Healthy {
		t.Fatal("a wrong credential must not be healthy")
	}
	if !strings.Contains(h.Detail, "credential") {
		t.Errorf("detail should name the credential, got %q", h.Detail)
	}
}

func TestNZBGetCheckUnreachable(t *testing.T) {
	// A port that refuses, the ADR-0025 case: unhealthy, not a startup failure.
	c := nzbClient(t, "http://127.0.0.1:9")
	h := c.Check(context.Background())
	if h.Healthy {
		t.Fatal("an unreachable instance must not be healthy")
	}
	if strings.Contains(h.Detail, "credential") {
		t.Errorf("an unreachable instance is not a credential problem, got %q", h.Detail)
	}
}

func TestNZBGetCheckAuthRelaxed(t *testing.T) {
	// An instance with the control password blank (a trusted network): no
	// credential configured, none required. A supported deployment.
	f := newFakeNZBGet()
	f.requireAuth = false
	c, _ := NewNZBGet(NZBGetOptions{Name: "nzb", Endpoint: f.server(t).URL, Label: nzbCat})
	if h := c.Check(context.Background()); !h.Healthy {
		t.Fatalf("a password-relaxed instance should be healthy, got %+v", h)
	}
}

func TestNZBGetAddNZBResolvesByNZBID(t *testing.T) {
	f := newFakeNZBGet()
	c := nzbClient(t, f.server(t).URL)
	tr, err := c.Add(context.Background(), secret.Value(nzbURL))
	if err != nil {
		t.Fatal(err)
	}
	if tr.ID != "42" {
		t.Fatalf("transfer id = %q, want the NZBID 42", tr.ID)
	}
	if len(f.addedURLs) != 1 || f.addedURLs[0] != nzbURL {
		t.Errorf("the nzb URL was not handed to append: %v", f.addedURLs)
	}
}

func TestNZBGetAddRefusesNonUsenet(t *testing.T) {
	f := newFakeNZBGet()
	c := nzbClient(t, f.server(t).URL)
	for _, src := range []string{
		"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
		"https://example.test/release.torrent",
		"https://example.test/file.mkv",
		"http://example.test/download?id=7",
	} {
		_, err := c.Add(context.Background(), secret.Value(src))
		if !errors.Is(err, ErrNotUsenetSource) {
			t.Errorf("Add(%q) = %v, want ErrNotUsenetSource (so it composes)", src, err)
		}
	}
	if len(f.addedURLs) != 0 {
		t.Errorf("a refused source must never reach append, got %v", f.addedURLs)
	}
}

func TestNZBGetAddIsIdempotent(t *testing.T) {
	f := newFakeNZBGet()
	c := nzbClient(t, f.server(t).URL)
	first, err := c.Add(context.Background(), secret.Value(nzbURL))
	if err != nil {
		t.Fatal(err)
	}
	// A re-run of the same job (invariant 9) must not enqueue a second download.
	second, err := c.Add(context.Background(), secret.Value(nzbURL))
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Errorf("a re-add returned a different transfer: %q then %q", first.ID, second.ID)
	}
	if len(f.addedURLs) != 1 {
		t.Errorf("a re-add must not hit append again, got %d adds", len(f.addedURLs))
	}
}

func TestNZBGetTransfersMergeFilterAndMap(t *testing.T) {
	f := newFakeNZBGet()
	f.groups = []nzbGroup{
		{NZBID: 1, NZBName: "Ours Downloading", Category: nzbCat, Status: "DOWNLOADING", FileSizeLo: 100 * 1024 * 1024, RemainingSizeLo: 40 * 1024 * 1024},
		{NZBID: 2, NZBName: "Operator's own", Category: "tv", Status: "DOWNLOADING", FileSizeLo: 10 * 1024 * 1024, RemainingSizeLo: 1 * 1024 * 1024},
	}
	f.history = []nzbHistory{
		{NZBID: 3, Name: "Ours Complete", Category: nzbCat, Status: "SUCCESS/ALL", FileSizeLo: 500, DestDir: "/downloads/complete/ours"},
		{NZBID: 4, Name: "Their Complete", Category: "movies", Status: "SUCCESS/ALL", FileSizeLo: 900, DestDir: "/x"},
	}
	c := nzbClient(t, f.server(t).URL)
	got, err := c.Transfers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d transfers, want only our 2 (the operator's are invisible)", len(got))
	}
	byID := map[string]providers.Transfer{}
	for _, tr := range got {
		byID[tr.ID] = tr
	}
	done := byID["3"]
	if !done.Done || done.Path != "/downloads/complete/ours" || done.BytesDone != 500 {
		t.Errorf("completed transfer wrong: %+v", done)
	}
	inflight := byID["1"]
	if inflight.Done || inflight.Path != "" {
		t.Errorf("an in-flight transfer must not be done and must report no path yet: %+v", inflight)
	}
	// 100 MB total, 40 MB left → 60 MB done.
	if inflight.BytesTotal != 100*1024*1024 || inflight.BytesDone != 60*1024*1024 {
		t.Errorf("in-flight progress wrong: total=%d done=%d", inflight.BytesTotal, inflight.BytesDone)
	}
}

func TestNZBGetTransfersReportsFailure(t *testing.T) {
	f := newFakeNZBGet()
	f.history = []nzbHistory{
		{NZBID: 5, Name: "Broke", Category: nzbCat, Status: "FAILURE/PAR", DestDir: "/x"},
	}
	c := nzbClient(t, f.server(t).URL)
	got, err := c.Transfers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 transfer, got %d", len(got))
	}
	// A failed transfer carries the error and no path: ingest must not open the
	// bytes of a download that did not complete.
	if got[0].Error == "" || got[0].Path != "" {
		t.Errorf("a failed transfer must carry an error and no path: %+v", got[0])
	}
}

func TestNZBGetTransfersHealthWarningIsNotAFailure(t *testing.T) {
	// A WARNING/HEALTH history means the bytes are on disk with a health note —
	// usable, not a failure. It must present as a completed transfer with a path.
	f := newFakeNZBGet()
	f.history = []nzbHistory{
		{NZBID: 6, Name: "Slightly unhealthy but here", Category: nzbCat, Status: "WARNING/HEALTH", FileSizeLo: 700, DestDir: "/downloads/complete/warn"},
	}
	c := nzbClient(t, f.server(t).URL)
	got, err := c.Transfers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Error != "" || got[0].Path != "/downloads/complete/warn" {
		t.Errorf("a WARNING download should be a usable completed transfer: %+v", got)
	}
}

func TestNZBGetRemoveRefusesForeign(t *testing.T) {
	f := newFakeNZBGet()
	f.groups = []nzbGroup{{NZBID: 7, NZBName: "not ours", Category: "tv"}}
	c := nzbClient(t, f.server(t).URL)
	err := c.Remove(context.Background(), "7", true)
	if !errors.Is(err, ErrNotOurs) {
		t.Fatalf("removing a foreign transfer = %v, want ErrNotOurs", err)
	}
	if len(f.groups) != 1 {
		t.Error("a refused remove must not delete anything")
	}
}

func TestNZBGetRemoveOursFromQueue(t *testing.T) {
	f := newFakeNZBGet()
	f.groups = []nzbGroup{{NZBID: 8, NZBName: "ours", Category: nzbCat, Status: "DOWNLOADING"}}
	c := nzbClient(t, f.server(t).URL)
	if err := c.Remove(context.Background(), "8", true); err != nil {
		t.Fatal(err)
	}
	if len(f.groups) != 0 {
		t.Error("removing our queued transfer should delete it from the queue")
	}
}

func TestNZBGetRemoveOursFromHistory(t *testing.T) {
	f := newFakeNZBGet()
	f.history = []nzbHistory{{NZBID: 9, Name: "ours", Category: nzbCat, Status: "SUCCESS/ALL"}}
	c := nzbClient(t, f.server(t).URL)
	if err := c.Remove(context.Background(), "9", true); err != nil {
		t.Fatal(err)
	}
	if len(f.history) != 0 {
		t.Error("removing our finished transfer should delete it from the history")
	}
}

func TestNZBGetAddFails(t *testing.T) {
	f := newFakeNZBGet()
	f.addFails = true
	c := nzbClient(t, f.server(t).URL)
	_, err := c.Add(context.Background(), secret.Value(nzbURL))
	if !errors.Is(err, ErrRPCFailure) {
		t.Fatalf("a rejected source = %v, want ErrRPCFailure", err)
	}
}

// The username and password reach the WIRE as an HTTP basic credential, through
// the whole Validate → Constructor → real client path.
//
// This is the NZBGet analogue of TestSABAPIKeyReachesTheWire, and the point is
// the fork from SABnzbd: NZBGet is a basic-scheme (AuthBasic) client, so its
// credential leaves the wrapper through credentialFor's Basic() — reaching for
// Token() the way SABnzbd does would yield an empty pair and a client that 401s
// an hour later, the quiet failure ADR-0031 exists to prevent. It also proves
// the construct wiring is reached at all: a mechanism with no caller here would
// send no credential because nothing built the client.
func TestNZBGetCredentialReachesTheWire(t *testing.T) {
	var gotUser, gotPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); ok {
			gotUser, gotPass = u, p
		}
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "version" {
			writeResult(w, "21.1")
			return
		}
		writeResult(w, []nzbGroup{})
	}))
	defer srv.Close()

	client := constructFor(t, providers.Entry{
		Name:     "a-usenet-client",
		Type:     string(providers.KindNZBGet),
		Endpoint: srv.URL,
		Credential: &providers.CredentialEntry{
			Username: "the-configured-user",
			Password: providers.Secret("the-configured-pass-9f2a"),
		},
	})

	health := client.Check(context.Background())
	if !health.Healthy {
		t.Fatalf("the probe should have succeeded: %+v", health)
	}
	if gotUser != "the-configured-user" || gotPass != "the-configured-pass-9f2a" {
		t.Errorf("basic credential on the wire = %q:%q, want the configured pair", gotUser, gotPass)
	}
}

func TestNZBGetComposesRefusingTorrentSourcesQBittorrentTakes(t *testing.T) {
	// The composition boundary from NZBGet's side: it must refuse exactly the
	// sources the torrent client takes, so the two never both claim a release.
	for _, s := range []string{
		"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
		"https://tracker.test/x.torrent",
	} {
		if !isTorrentSource(s) {
			t.Fatalf("precondition: qBittorrent should take %q", s)
		}
		if _, ok := isUsenetSource(s); ok {
			t.Errorf("NZBGet must refuse %q — the torrent client takes it", s)
		}
	}
}

func TestNZBGetRemoveCommandVariesByListAndDeleteData(t *testing.T) {
	// The four editqueue commands, one per (queued|done)×(keep|delete) corner, so
	// a wrong mapping — deleting files when asked to keep them, or the reverse —
	// is caught. The failure that matters most is deleting bytes Heyarr has not
	// ingested, so keep-files must never send a "Final" command.
	cases := []struct {
		name       string
		done       bool
		deleteData bool
		want       string
	}{
		{"queued keep", false, false, "GroupDelete"},
		{"queued delete", false, true, "GroupFinalDelete"},
		{"done keep", true, false, "HistoryDelete"},
		{"done delete", true, true, "HistoryFinalDelete"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotCommand string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Method string            `json:"method"`
					Params []json.RawMessage `json:"params"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				w.Header().Set("Content-Type", "application/json")
				switch req.Method {
				case "listgroups":
					if tc.done {
						writeResult(w, []nzbGroup{})
					} else {
						writeResult(w, []nzbGroup{{NZBID: 10, NZBName: "ours", Category: nzbCat}})
					}
				case "history":
					if tc.done {
						writeResult(w, []nzbHistory{{NZBID: 10, Name: "ours", Category: nzbCat, Status: "SUCCESS/ALL"}})
					} else {
						writeResult(w, []nzbHistory{})
					}
				case "editqueue":
					_ = json.Unmarshal(req.Params[0], &gotCommand)
					writeResult(w, true)
				default:
					writeResult(w, "21.1")
				}
			}))
			defer srv.Close()

			c, _ := NewNZBGet(NZBGetOptions{Name: "nzb", Endpoint: srv.URL, Label: nzbCat})
			if err := c.Remove(context.Background(), "10", tc.deleteData); err != nil {
				t.Fatal(err)
			}
			if gotCommand != tc.want {
				t.Errorf("Remove(done=%v, deleteData=%v) sent %q, want %q",
					tc.done, tc.deleteData, gotCommand, tc.want)
			}
		})
	}
}

// combineSize joins NZBGet's split halves; a high half must actually count.
func TestNZBGetCombineSize(t *testing.T) {
	// 5 GiB is past a uint32, so it exercises the high half — a size the Lo field
	// alone cannot hold, which is the whole reason NZBGet splits it.
	const fiveGiB = int64(5) << 30
	hi := uint32(fiveGiB >> 32)
	lo := uint32(fiveGiB & 0xffffffff)
	if got := combineSize(hi, lo); got != fiveGiB {
		t.Errorf("combineSize(%d,%d) = %d, want %d", hi, lo, got, fiveGiB)
	}
	if got := strconv.FormatInt(combineSize(0, 100), 10); got != "100" {
		t.Errorf("combineSize(0,100) = %s, want 100", got)
	}
}
