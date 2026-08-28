//nolint:bodyclose // response bodies are closed by the t.Cleanup the harness registers
package dlna_test

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDescriptionAndSCPD(t *testing.T) {
	h := newHarness(t)
	desc := readAll(t, h.get("/dlna/description.xml"))
	if !strings.Contains(desc, "urn:schemas-upnp-org:device:MediaServer:1") {
		t.Errorf("description is not a MediaServer:\n%s", desc)
	}
	if !strings.Contains(desc, "/dlna/control/ContentDirectory") {
		t.Error("description does not point at the control URL")
	}
	scpd := readAll(t, h.get("/dlna/ContentDirectory.xml"))
	if !strings.Contains(scpd, "<name>Browse</name>") {
		t.Errorf("SCPD does not advertise the Browse action:\n%s", scpd)
	}
}

func TestBrowseRootFolders(t *testing.T) {
	h := newHarness(t)
	r := h.browse("0", "BrowseDirectChildren")
	if r.fault {
		t.Fatalf("root browse faulted: %d", r.faultCode)
	}
	// Movies and Music are servable; Books (epub) is not, so it is absent.
	if len(r.didl.Containers) != 2 {
		t.Fatalf("root should list 2 folders (movie, music), got %d: %+v", len(r.didl.Containers), r.didl.Containers)
	}
	movies := r.containerByID(t, "ct:movie")
	if movies.Title != "Movies" || movies.ChildCount != 1 || movies.Class != "object.container.storageFolder" {
		t.Errorf("movies folder wrong: %+v", movies)
	}
	if r.totalMatches != 2 {
		t.Errorf("totalMatches = %d, want 2", r.totalMatches)
	}
	// No Books container — an epub is not render-servable.
	for _, c := range r.didl.Containers {
		if c.ID == "ct:book" {
			t.Error("a non-servable content type (book) was advertised as a folder")
		}
	}
}

func TestBrowseMovieItem(t *testing.T) {
	h := newHarness(t)
	r := h.browse("ct:movie", "BrowseDirectChildren")
	if r.fault {
		t.Fatalf("movie browse faulted: %d", r.faultCode)
	}
	it := r.itemByTitle(t, "Arrival")
	if it.Class != "object.item.videoItem" {
		t.Errorf("movie class = %q, want videoItem", it.Class)
	}
	if it.ParentID != "ct:movie" {
		t.Errorf("item parentID = %q", it.ParentID)
	}
	if it.Res.ProtocolInfo != "http-get:*:video/x-matroska:*" {
		t.Errorf("protocolInfo = %q", it.Res.ProtocolInfo)
	}
	if !strings.Contains(it.Res.URL, "/render/") {
		t.Errorf("res URL is not a render capability: %q", it.Res.URL)
	}
	if it.Res.Size != int64(len(h.bytesOf["Arrival"])) {
		t.Errorf("res size = %d, want %d", it.Res.Size, len(h.bytesOf["Arrival"]))
	}
}

// TestResURLServesExactBytes is the mechanism-with-no-caller proof at unit
// level: a Browse hands out a res URL, and fetching THAT URL returns the blob's
// bytes. The adapter emits a real, fetchable capability, not a decorative
// string.
func TestResURLServesExactBytes(t *testing.T) {
	h := newHarness(t)
	it := h.browse("ct:movie", "BrowseDirectChildren").itemByTitle(t, "Arrival")

	// The res URL is absolute-ish; fetch it by path against the same server.
	path := it.Res.URL
	if i := strings.Index(path, "/render/"); i >= 0 {
		path = path[i:]
	}
	resp := h.get(path)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fetching the res URL = %d, want 200", resp.StatusCode)
	}
	if body := readAllBytes(t, resp); !bytes.Equal(body, h.bytesOf["Arrival"]) {
		t.Fatalf("res bytes = %q, want the blob bytes %q", body, h.bytesOf["Arrival"])
	}

	// And it honours Range, because the render route it points at delegates to
	// the ordinary blob handler.
	req, _ := http.NewRequest(http.MethodGet, h.http.URL+path, nil)
	req.Header.Set("Range", "bytes=0-3")
	rr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rr.Body.Close() })
	if rr.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range fetch = %d, want 206", rr.StatusCode)
	}
}

func TestBrowseMusicItem(t *testing.T) {
	h := newHarness(t)
	it := h.browse("ct:music", "BrowseDirectChildren").itemByTitle(t, "Datum")
	if it.Class != "object.item.audioItem.musicTrack" {
		t.Errorf("music class = %q", it.Class)
	}
	if it.Res.ProtocolInfo != "http-get:*:audio/flac:*" {
		t.Errorf("protocolInfo = %q", it.Res.ProtocolInfo)
	}
}

func TestBrowseMetadataRoot(t *testing.T) {
	h := newHarness(t)
	r := h.browse("0", "BrowseMetadata")
	if r.fault || len(r.didl.Containers) != 1 {
		t.Fatalf("root metadata = %+v (fault=%v)", r.didl.Containers, r.fault)
	}
	if r.didl.Containers[0].ID != "0" {
		t.Errorf("root metadata id = %q, want 0", r.didl.Containers[0].ID)
	}
}

func TestNonServableBookNeverAppears(t *testing.T) {
	h := newHarness(t)
	// There is no book folder, and browsing the (nonexistent) book container
	// yields nothing rather than the epub.
	r := h.browse("ct:book", "BrowseDirectChildren")
	if r.fault {
		t.Fatalf("browsing an empty content type should not fault, got %d", r.faultCode)
	}
	if len(r.didl.Items) != 0 {
		t.Errorf("a non-servable epub was advertised as an item: %+v", r.didl.Items)
	}
}

func TestUnknownObjectFaults(t *testing.T) {
	h := newHarness(t)
	r := h.browse("asset:nope", "BrowseMetadata")
	if !r.fault || r.faultCode != 701 {
		t.Fatalf("unknown object should fault 701, got fault=%v code=%d", r.fault, r.faultCode)
	}
	r2 := h.browse("garbage", "BrowseDirectChildren")
	if !r2.fault || r2.faultCode != 701 {
		t.Fatalf("unknown container should fault 701, got fault=%v code=%d", r2.fault, r2.faultCode)
	}
}

func TestNonBrowseActionFaults(t *testing.T) {
	h := newHarness(t)
	body := `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<u:Search xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1"><ContainerID>0</ContainerID></u:Search>` +
		`</s:Body></s:Envelope>`
	r := h.decodeBrowse(h.post("/dlna/control/ContentDirectory", body))
	if !r.fault || r.faultCode != 401 {
		t.Fatalf("an unimplemented action should fault 401, got fault=%v code=%d", r.fault, r.faultCode)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	return string(readAllBytes(t, resp))
}

func readAllBytes(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
