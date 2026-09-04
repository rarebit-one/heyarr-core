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

// The SABnzbd client, driven against a fake of its HTTP API.
//
// ADR-0026: a download client's merge-path test is a fake of its protocol, and
// its live exercise is opt-in against a real instance (TestLiveSABnzbd). The
// fake here answers with the shapes SABnzbd actually returns — a version object,
// a queue object with string sizes, a history object with a completed path, an
// addurl object carrying an nzo_id, and the `{"status":false,"error":"API Key
// Incorrect"}` envelope for a bad key — so the client's real transport and
// parsing are exercised, not stubbed.

const (
	sabKey = "test-api-key"
	sabCat = "heyarr"
	sabNZB = "https://indexer.test/getnzb/abc.nzb?apikey=secret"
)

// fakeSAB is a configurable stand-in for a SABnzbd HTTP API.
type fakeSAB struct {
	requireAuth bool
	version     string
	addFails    bool
	queue       []sabQueueSlot
	history     []sabHistorySlot
	addedURLs   []string // the `name` (nzb URL) handed to addurl
	nextNZOID   string   // the id addurl assigns
}

func newFakeSAB() *fakeSAB {
	return &fakeSAB{requireAuth: true, version: "4.2.0", nextNZOID: "SABnzbd_nzo_new1"}
}

func (f *fakeSAB) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		mode := q.Get("mode")

		// version is a public read; everything else needs the key when required.
		if f.requireAuth && mode != "version" && q.Get("apikey") != sabKey {
			_, _ = w.Write([]byte(`{"status":false,"error":"API Key Incorrect"}`))
			return
		}

		switch mode {
		case "version":
			_, _ = w.Write([]byte(`{"version":"` + f.version + `"}`))
		case "queue":
			if q.Get("name") == "delete" {
				f.deleteFromQueue(q.Get("value"))
				_, _ = w.Write([]byte(`{"status":true}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"queue": map[string]any{"slots": f.queue},
			})
		case "history":
			if q.Get("name") == "delete" {
				f.deleteFromHistory(q.Get("value"))
				_, _ = w.Write([]byte(`{"status":true}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"history": map[string]any{"slots": f.history},
			})
		case "addurl":
			if f.addFails {
				_, _ = w.Write([]byte(`{"status":false,"nzo_ids":[]}`))
				return
			}
			f.addedURLs = append(f.addedURLs, q.Get("name"))
			f.queue = append(f.queue, sabQueueSlot{
				NZOID: f.nextNZOID, Filename: q.Get("nzbname"), Cat: q.Get("cat"),
				Status: "Downloading", MB: "100.0", MBLeft: "100.0",
			})
			_, _ = w.Write([]byte(`{"status":true,"nzo_ids":["` + f.nextNZOID + `"]}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeSAB) deleteFromQueue(id string) {
	kept := f.queue[:0]
	for _, s := range f.queue {
		if !strings.EqualFold(s.NZOID, id) {
			kept = append(kept, s)
		}
	}
	f.queue = kept
}

func (f *fakeSAB) deleteFromHistory(id string) {
	kept := f.history[:0]
	for _, s := range f.history {
		if !strings.EqualFold(s.NZOID, id) {
			kept = append(kept, s)
		}
	}
	f.history = kept
}

func sabClient(t *testing.T, endpoint string) *SABClient {
	t.Helper()
	c, err := NewSABnzbd(SABOptions{
		Name: "sab", Endpoint: endpoint, APIKey: sabKey, Label: sabCat,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSABCheckHealthy(t *testing.T) {
	f := newFakeSAB()
	c := sabClient(t, f.server(t).URL)
	h := c.Check(context.Background())
	if !h.Healthy {
		t.Fatalf("expected healthy, got %+v", h)
	}
	if h.Version != "4.2.0" {
		t.Errorf("version = %q, want the app version 4.2.0", h.Version)
	}
}

func TestSABCheckWrongCredential(t *testing.T) {
	f := newFakeSAB()
	srv := f.server(t)
	c, _ := NewSABnzbd(SABOptions{Name: "sab", Endpoint: srv.URL, APIKey: "wrong", Label: sabCat})
	h := c.Check(context.Background())
	if h.Healthy {
		t.Fatal("a wrong credential must not be healthy")
	}
	if !strings.Contains(h.Detail, "credential") {
		t.Errorf("detail should name the credential, got %q", h.Detail)
	}
}

func TestSABCheckUnreachable(t *testing.T) {
	// A port that refuses, the ADR-0025 case: unhealthy, not a startup failure.
	c := sabClient(t, "http://127.0.0.1:9")
	h := c.Check(context.Background())
	if h.Healthy {
		t.Fatal("an unreachable instance must not be healthy")
	}
	if strings.Contains(h.Detail, "credential") {
		t.Errorf("an unreachable instance is not a credential problem, got %q", h.Detail)
	}
}

func TestSABCheckAuthRelaxed(t *testing.T) {
	// An instance with the api-key requirement relaxed (a trusted network): no
	// credential configured, no key required. A supported deployment.
	f := newFakeSAB()
	f.requireAuth = false
	c, _ := NewSABnzbd(SABOptions{Name: "sab", Endpoint: f.server(t).URL, Label: sabCat})
	if h := c.Check(context.Background()); !h.Healthy {
		t.Fatalf("a key-relaxed instance should be healthy, got %+v", h)
	}
}

func TestSABAddNZBResolvesByNZOID(t *testing.T) {
	f := newFakeSAB()
	c := sabClient(t, f.server(t).URL)
	tr, err := c.Add(context.Background(), secret.Value(sabNZB))
	if err != nil {
		t.Fatal(err)
	}
	if tr.ID != "SABnzbd_nzo_new1" {
		t.Fatalf("transfer id = %q, want the nzo_id SABnzbd_nzo_new1", tr.ID)
	}
	if len(f.addedURLs) != 1 || f.addedURLs[0] != sabNZB {
		t.Errorf("the nzb URL was not handed to addurl: %v", f.addedURLs)
	}
}

func TestSABAddRefusesNonUsenet(t *testing.T) {
	f := newFakeSAB()
	c := sabClient(t, f.server(t).URL)
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
		t.Errorf("a refused source must never reach addurl, got %v", f.addedURLs)
	}
}

func TestSABAddIsIdempotent(t *testing.T) {
	f := newFakeSAB()
	c := sabClient(t, f.server(t).URL)
	first, err := c.Add(context.Background(), secret.Value(sabNZB))
	if err != nil {
		t.Fatal(err)
	}
	// A re-run of the same job (invariant 9) must not enqueue a second download.
	second, err := c.Add(context.Background(), secret.Value(sabNZB))
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Errorf("a re-add returned a different transfer: %q then %q", first.ID, second.ID)
	}
	if len(f.addedURLs) != 1 {
		t.Errorf("a re-add must not hit addurl again, got %d adds", len(f.addedURLs))
	}
}

func TestSABTransfersMergeFilterAndMap(t *testing.T) {
	f := newFakeSAB()
	f.queue = []sabQueueSlot{
		{NZOID: "q_ours", Filename: "Ours Downloading", Cat: sabCat, Status: "Downloading", MB: "100.0", MBLeft: "40.0"},
		{NZOID: "q_theirs", Filename: "Operator's own", Cat: "tv", Status: "Downloading", MB: "10.0", MBLeft: "1.0"},
	}
	f.history = []sabHistorySlot{
		{NZOID: "h_ours", Name: "Ours Complete", Category: sabCat, Status: "Completed", Bytes: 500, Storage: "/downloads/complete/ours"},
		{NZOID: "h_theirs", Name: "Their Complete", Category: "movies", Status: "Completed", Bytes: 900, Storage: "/x"},
	}
	c := sabClient(t, f.server(t).URL)
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
	done := byID["h_ours"]
	if !done.Done || done.Path != "/downloads/complete/ours" || done.BytesDone != 500 {
		t.Errorf("completed transfer wrong: %+v", done)
	}
	inflight := byID["q_ours"]
	if inflight.Done || inflight.Path != "" {
		t.Errorf("an in-flight transfer must not be done and must report no path yet: %+v", inflight)
	}
	// 100 MB total, 40 MB left → 60 MB done.
	if inflight.BytesTotal != 100*1024*1024 || inflight.BytesDone != 60*1024*1024 {
		t.Errorf("in-flight progress wrong: total=%d done=%d", inflight.BytesTotal, inflight.BytesDone)
	}
}

func TestSABTransfersReportsFailure(t *testing.T) {
	f := newFakeSAB()
	f.history = []sabHistorySlot{
		{NZOID: "h_fail", Name: "Broke", Category: sabCat, Status: "Failed", FailMessage: "unpack failed", Storage: "/x"},
	}
	c := sabClient(t, f.server(t).URL)
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

func TestSABRemoveRefusesForeign(t *testing.T) {
	f := newFakeSAB()
	f.queue = []sabQueueSlot{{NZOID: "theirs", Filename: "not ours", Cat: "tv"}}
	c := sabClient(t, f.server(t).URL)
	err := c.Remove(context.Background(), "theirs", true)
	if !errors.Is(err, ErrNotOurs) {
		t.Fatalf("removing a foreign transfer = %v, want ErrNotOurs", err)
	}
	if len(f.queue) != 1 {
		t.Error("a refused remove must not delete anything")
	}
}

func TestSABRemoveOursFromQueue(t *testing.T) {
	f := newFakeSAB()
	f.queue = []sabQueueSlot{{NZOID: "q_ours", Filename: "ours", Cat: sabCat}}
	c := sabClient(t, f.server(t).URL)
	if err := c.Remove(context.Background(), "q_ours", true); err != nil {
		t.Fatal(err)
	}
	if len(f.queue) != 0 {
		t.Error("removing our queued transfer should delete it from the queue")
	}
}

func TestSABRemoveOursFromHistory(t *testing.T) {
	f := newFakeSAB()
	f.history = []sabHistorySlot{{NZOID: "h_ours", Name: "ours", Category: sabCat, Status: "Completed"}}
	c := sabClient(t, f.server(t).URL)
	if err := c.Remove(context.Background(), "h_ours", true); err != nil {
		t.Fatal(err)
	}
	if len(f.history) != 0 {
		t.Error("removing our finished transfer should delete it from the history")
	}
}

func TestSABAddFails(t *testing.T) {
	f := newFakeSAB()
	f.addFails = true
	c := sabClient(t, f.server(t).URL)
	_, err := c.Add(context.Background(), secret.Value(sabNZB))
	if !errors.Is(err, ErrRPCFailure) {
		t.Fatalf("a rejected source = %v, want ErrRPCFailure", err)
	}
}

func TestIsUsenetSource(t *testing.T) {
	yes := map[string]string{
		"https://x.test/a.nzb":           "https://x.test/a.nzb",
		"https://x.test/a.nzb?apikey=z":  "https://x.test/a.nzb?apikey=z",
		"HTTPS://X.TEST/A.NZB":           "HTTPS://X.TEST/A.NZB",
		"usenet:https://x.test/get?id=1": "https://x.test/get?id=1",
		"nzb:https://x.test/get?id=1":    "https://x.test/get?id=1",
	}
	for in, want := range yes {
		got, ok := isUsenetSource(in)
		if !ok {
			t.Errorf("isUsenetSource(%q) = false, want true", in)
		}
		if got != want {
			t.Errorf("isUsenetSource(%q) stripped to %q, want %q", in, got, want)
		}
	}
	no := []string{
		"magnet:?xt=urn:btih:abc",
		"https://x.test/a.torrent",
		"https://x.test/a.mkv",
		"http://x.test/get?id=1",
		"",
	}
	for _, s := range no {
		if _, ok := isUsenetSource(s); ok {
			t.Errorf("isUsenetSource(%q) = true, want false (so it composes)", s)
		}
	}
}

// The api_key reaches the WIRE as SABnzbd's `apikey` query parameter, through
// the whole Validate → Constructor → real client path.
//
// This is the SABnzbd analogue of TestAPasswordWithAColonReachesTheWireIntact:
// SABnzbd is a token-scheme (AuthToken) client, so its credential leaves the
// wrapper through Token() rather than Basic(), and reaching for the wrong
// accessor in construct.go would yield an empty key and a client that 401s an
// hour later — the quiet failure ADR-0031 exists to prevent. It also proves the
// construct wiring is reached at all: a mechanism with no caller here would send
// no key because nothing built the client.
func TestSABAPIKeyReachesTheWire(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if k := r.URL.Query().Get("apikey"); k != "" {
			gotKey = k
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("mode") {
		case "version":
			_, _ = w.Write([]byte(`{"version":"4.2.0"}`))
		default:
			_, _ = w.Write([]byte(`{"queue":{"slots":[]}}`))
		}
	}))
	defer srv.Close()

	client := constructFor(t, providers.Entry{
		Name:     "a-usenet-client",
		Type:     string(providers.KindSABnzbd),
		Endpoint: srv.URL,
		APIKey:   providers.Secret("the-configured-key-9f2a"),
	})

	health := client.Check(context.Background())
	if !health.Healthy {
		t.Fatalf("the probe should have succeeded: %+v", health)
	}
	if gotKey != "the-configured-key-9f2a" {
		t.Errorf("apikey on the wire = %q, want the configured key", gotKey)
	}
}

func TestSABComposesRefusingTorrentSourcesQBittorrentTakes(t *testing.T) {
	// The composition boundary from SABnzbd's side: it must refuse exactly the
	// sources the torrent client takes, so the two never both claim a release.
	torrentSources := []string{
		"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
		"https://tracker.test/x.torrent",
	}
	for _, s := range torrentSources {
		if !isTorrentSource(s) {
			t.Fatalf("precondition: qBittorrent should take %q", s)
		}
		if _, ok := isUsenetSource(s); ok {
			t.Errorf("SABnzbd must refuse %q — the torrent client takes it", s)
		}
	}
}
