package downloads

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A magnet-only tracker behind Prowlarr answers its /download link with a 301
// to a magnet: URI. addSource must hand that magnet to Transmission as
// `filename`, verbatim (passkey and every tracker preserved), not try to fetch
// a .torrent — which is the grab failure this fixes.
func TestAddSourceFollowsRedirectToMagnet(t *testing.T) {
	const magnet = "magnet:?xt=urn:btih:ABD2AD9EEE6011CD06F5B0D320DABA5C10B070F0" +
		"&dn=Slow.Horses.S01E01-AFG&tr=udp%3A%2F%2Ftracker.opentrackr.org%3A1337%2Fannounce" +
		"&tr=udp%3A%2F%2Fopen.stealth.si%3A80%2Fannounce"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", magnet)
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer srv.Close()

	c := &Client{httpc: &http.Client{}}
	args, err := c.addSource(context.Background(), srv.URL+"/4/download?apikey=k&link=z")
	if err != nil {
		t.Fatalf("addSource: %v", err)
	}
	if got := args["filename"]; got != magnet {
		t.Errorf("filename = %q\n want %q (the magnet, verbatim)", got, magnet)
	}
	if _, ok := args["metainfo"]; ok {
		t.Error("a magnet has nothing to fetch — metainfo must not be set")
	}
}

// An ordinary http(s) redirect to the actual .torrent is still followed, and
// its bytes handed over as metainfo.
func TestAddSourceFollowsHTTPRedirectToTorrent(t *testing.T) {
	torrent := []byte("d8:announce9:test:6969e") // stand-in bencode bytes
	mux := http.NewServeMux()
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/file.torrent", http.StatusFound)
	})
	mux.HandleFunc("/file.torrent", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(torrent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{httpc: &http.Client{}}
	args, err := c.addSource(context.Background(), srv.URL+"/download")
	if err != nil {
		t.Fatalf("addSource: %v", err)
	}
	want := base64.StdEncoding.EncodeToString(torrent)
	if got := args["metainfo"]; got != want {
		t.Errorf("metainfo = %v, want the fetched .torrent bytes", got)
	}
	if _, ok := args["filename"]; ok {
		t.Error("a fetched .torrent goes as metainfo, not filename")
	}
}

// A direct .torrent (200, no redirect) is unchanged: fetched and handed as
// metainfo.
func TestAddSourceDirectTorrent(t *testing.T) {
	torrent := []byte("d8:announce4:teste")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(torrent)
	}))
	defer srv.Close()

	c := &Client{httpc: &http.Client{}}
	args, err := c.addSource(context.Background(), srv.URL+"/download")
	if err != nil {
		t.Fatalf("addSource: %v", err)
	}
	if args["metainfo"] != base64.StdEncoding.EncodeToString(torrent) {
		t.Error("direct .torrent should be handed over as metainfo")
	}
}

// A bare magnet source is unchanged: passed straight through as filename,
// never fetched.
func TestAddSourceBareMagnetUnchanged(t *testing.T) {
	const magnet = "magnet:?xt=urn:btih:DEADBEEF&dn=x"
	c := &Client{httpc: &http.Client{}}
	args, err := c.addSource(context.Background(), magnet)
	if err != nil {
		t.Fatalf("addSource: %v", err)
	}
	if args["filename"] != magnet {
		t.Errorf("filename = %v, want the magnet unchanged", args["filename"])
	}
}
