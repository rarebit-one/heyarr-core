package downloads

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A transfer whose every file is a program is reported Executable, once its
// file list is known; a real release, a mixed one and one still fetching its
// metadata are not (#819).
func TestTransmissionReportsAProgramOnlyTransfer(t *testing.T) {
	fake := strings.Repeat("a", 40)
	real := strings.Repeat("b", 40)
	mixed := strings.Repeat("c", 40)
	pending := strings.Repeat("d", 40)
	files := map[string][]string{
		fake:    {"Show S06E06 1080p WEB H264-GRP.exe"},
		real:    {"Show.S06E05/Show.S06E05.mkv", "Show.S06E05/Show.S06E05.nfo"},
		mixed:   {"Game/setup.exe", "Game/data.bin"},
		pending: {"Show.S06E07.exe"},
	}
	var fileLookups int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method    string `json:"method"`
			Arguments struct {
				IDs []string `json:"ids"`
			} `json:"arguments"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		type tor map[string]any
		var out []tor
		if len(req.Arguments.IDs) > 0 {
			fileLookups++
			for _, id := range req.Arguments.IDs {
				fs := []tor{}
				for _, n := range files[id] {
					fs = append(fs, tor{"name": n})
				}
				out = append(out, tor{"hashString": id, "files": fs})
			}
		} else {
			for _, h := range []string{fake, real, mixed, pending} {
				meta := 1.0
				if h == pending {
					meta = 0.2 // a magnet still fetching its metadata
				}
				out = append(out, tor{
					"hashString": h, "status": statusDownloading, "labels": []string{"heyarr"},
					"metadataPercentComplete": meta,
				})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": "success", "arguments": map[string]any{"torrents": out}})
	}))
	defer srv.Close()
	c, err := New(Options{Name: "t", Endpoint: srv.URL, Label: "heyarr"})
	if err != nil {
		t.Fatal(err)
	}

	for pass := range 2 {
		got, err := c.Transfers(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		exe := map[string]bool{}
		for _, tr := range got {
			exe[tr.ID] = tr.Executable
		}
		if !exe[fake] || exe[real] || exe[mixed] || exe[pending] {
			t.Errorf("pass %d: executable = %v, want only the program-only transfer", pass, exe)
		}
	}
	if fileLookups != 1 {
		t.Errorf("%d file-list lookups over two passes, want 1: a verdict is read once", fileLookups)
	}
}

func TestQBittorrentReportsAProgramOnlyTransfer(t *testing.T) {
	f := newFakeQB()
	fake, real, meta := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	f.torrents = []qbTorrent{
		{Hash: fake, State: "downloading", Category: qbCat, Size: 10},
		{Hash: real, State: "downloading", Category: qbCat, Size: 10},
		{Hash: meta, State: "metaDL", Category: qbCat},
	}
	f.files = map[string][]string{
		fake: {"Show S06E06 1080p WEB H264-GRP.exe"},
		real: {"Show.S06E05.mkv"},
		meta: {"Show.S06E07.exe"},
	}
	c := qbClient(t, f.server(t).URL)
	for pass := range 2 {
		got, err := c.Transfers(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range got {
			if want := tr.ID == fake; tr.Executable != want {
				t.Errorf("pass %d: %s executable = %v, want %v", pass, tr.ID[:4], tr.Executable, want)
			}
		}
	}
	if f.filesAsked != 2 {
		t.Errorf("torrents/files asked %d times over two passes, want 2 (one per judged transfer)", f.filesAsked)
	}
}
