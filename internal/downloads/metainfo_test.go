package downloads

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
)

// #492: Heyarr, not the download client, fetches the indexer's .torrent.
//
// The confirmed live failure was a loopback-bound Prowlarr the host-native
// Heyarr can reach but Transmission — in its own container network namespace —
// cannot. Handing Transmission the download URL as `filename` made it try the
// fetch, get "No Response", and park every grab at SELECTED forever. The fix is
// that Heyarr resolves the .torrent itself and hands over the bytes as
// `metainfo`, so the client is never asked to reach the indexer at all.
//
// These stand up two servers: an INDEXER that serves a real .torrent behind an
// apikey query (the credential riding in the URL, as Torznab's download links
// do), and a Transmission STUB that captures the arguments of torrent-add. The
// stub deliberately has no path to the indexer and needs none — that absence is
// the property under test.

// transmissionAddStub speaks just enough of the RPC for Add: the 409 handshake,
// session-get, and a torrent-add that records what it was handed. It answers
// torrent-add with a synthesised torrent-added so Add returns cleanly.
//
// It FAILS the test if torrent-add ever arrives carrying a `filename` that is an
// http(s) URL — that is exactly the pre-#492 behaviour, the thing this whole
// change removes, and a stub that quietly tolerated it would let the bug back
// in without a red test.
func transmissionAddStub(t *testing.T, gotArgs *map[string]any) *httptest.Server {
	t.Helper()
	const sid = "stub-session-id"
	hash := strings.Repeat("a", 40)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			t.Errorf("stub could not decode the RPC envelope: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch env.Method {
		case "session-get":
			// RPC 19, above the 16 that introduced labels, so the primary
			// (labelled) add path is exercised rather than the subdirectory
			// fallback.
			_, _ = w.Write([]byte(`{"result":"success","arguments":` +
				`{"version":"4.1.3","rpc-version":19,"download-dir":"/downloads"}}`))
		case "torrent-add":
			if fn, ok := env.Arguments["filename"].(string); ok && isHTTPURL(fn) {
				t.Error("torrent-add was handed an http(s) URL as filename — " +
					"that is the #492 bug: the client would have to reach the indexer itself")
			}
			*gotArgs = env.Arguments
			_, _ = w.Write([]byte(`{"result":"success","arguments":{"torrent-added":` +
				`{"id":1,"name":"fixture.bin","hashString":"` + hash + `"}}}`))
		default:
			_, _ = w.Write([]byte(`{"result":"success","arguments":{}}`))
		}
	}))
}

// stubbedClient builds a Transmission client pointed at the add stub and warms
// its session with a Check, so SupportsLabels() reflects the stub's RPC 19.
func stubbedClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	c, err := New(Options{
		Name:     "test",
		Endpoint: endpoint,
		Label:    "heyarr",
		Now:      func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	if h := c.Check(context.Background()); !h.Healthy {
		t.Fatalf("the stub did not come up healthy: %s", h.Detail)
	}
	return c
}

// A real .torrent for the indexer to serve. makeWebseedTorrent is the harness
// generator (torrentfile_test.go); reused here so the bytes handed over as
// metainfo are a genuine metainfo file, not an arbitrary blob.
func sampleTorrent(t *testing.T) []byte {
	t.Helper()
	payload := bytes.Repeat([]byte("heyarr-492-"), 400)
	torrent, _, err := makeWebseedTorrent("fixture.bin", payload, 4096, "http://webseed/fixture.bin")
	if err != nil {
		t.Fatalf("makeWebseedTorrent: %v", err)
	}
	return torrent
}

// 🔴 An http(s) .torrent download URL is fetched by Heyarr and handed to
// Transmission as metainfo — never as a filename it would have to fetch itself.
//
// The indexer URL here has NO .torrent suffix — it is Prowlarr's
// `/<n>/download?apikey=…`, the exact shape #492 was filed against — so this
// also pins that the decision is on the scheme, not the extension.
func TestAnHTTPTorrentURLIsFetchedAndPassedAsMetainfo(t *testing.T) {
	torrent := sampleTorrent(t)

	var fetched bool
	indexer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The credential rides in the URL, the way a Torznab download link
		// carries its apikey. A fetch that dropped it would 401 here.
		if r.URL.Query().Get("apikey") != "s3cr3t" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fetched = true
		w.Header().Set("Content-Type", "application/x-bittorrent")
		_, _ = w.Write(torrent)
	}))
	defer indexer.Close()

	var gotArgs map[string]any
	trans := transmissionAddStub(t, &gotArgs)
	defer trans.Close()

	client := stubbedClient(t, trans.URL)
	source := secret.Value(indexer.URL + "/42/download?apikey=s3cr3t")
	if _, err := client.Add(context.Background(), source); err != nil {
		t.Fatalf("Add returned an error: %v", err)
	}

	if !fetched {
		t.Fatal("Heyarr never fetched the .torrent from the indexer — the whole point of the fix")
	}
	if _, ok := gotArgs["filename"]; ok {
		t.Error("torrent-add carried a filename; the client must be handed metainfo, not a URL to fetch")
	}
	enc, ok := gotArgs["metainfo"].(string)
	if !ok {
		t.Fatalf("torrent-add carried no metainfo string; args were %#v", gotArgs)
	}
	decoded, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("metainfo is not valid base64: %v", err)
	}
	if !bytes.Equal(decoded, torrent) {
		t.Errorf("metainfo bytes are not the .torrent the indexer served (got %d bytes, want %d)",
			len(decoded), len(torrent))
	}
}

// A magnet has nothing to fetch, so it is passed straight through as filename
// and the swarm moves the bytes. Rewriting it would risk dropping a tracker.
func TestAMagnetIsPassedThroughUnchanged(t *testing.T) {
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=x"

	var gotArgs map[string]any
	trans := transmissionAddStub(t, &gotArgs)
	defer trans.Close()

	client := stubbedClient(t, trans.URL)
	if _, err := client.Add(context.Background(), secret.Value(magnet)); err != nil {
		t.Fatalf("Add returned an error: %v", err)
	}

	if _, ok := gotArgs["metainfo"]; ok {
		t.Error("a magnet was fetched into metainfo; there is nothing to fetch and doing so could drop a tracker")
	}
	fn, ok := gotArgs["filename"].(string)
	if !ok {
		t.Fatalf("torrent-add carried no filename; args were %#v", gotArgs)
	}
	if fn != magnet {
		t.Errorf("magnet was altered: got %q, want %q", fn, magnet)
	}
}

// A failed fetch is a clean error that never discloses the URL — it carries a
// passkey, and registry.Grab renders this error into an operator's log.
func TestAFailedFetchDoesNotLeakTheURL(t *testing.T) {
	indexer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer indexer.Close()

	var gotArgs map[string]any
	trans := transmissionAddStub(t, &gotArgs)
	defer trans.Close()

	client := stubbedClient(t, trans.URL)
	source := secret.Value(indexer.URL + "/42/download?apikey=s3cr3t-passkey")
	_, err := client.Add(context.Background(), source)
	if err == nil {
		t.Fatal("Add succeeded despite the indexer returning 500")
	}
	if strings.Contains(err.Error(), "s3cr3t-passkey") || strings.Contains(err.Error(), indexer.URL) {
		t.Errorf("the error disclosed the download URL or its passkey: %v", err)
	}
	if gotArgs != nil {
		t.Error("torrent-add was still called after the fetch failed; nothing should have been queued")
	}
}
