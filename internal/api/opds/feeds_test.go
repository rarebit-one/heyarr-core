// Response bodies are closed by the t.Cleanup the harness registers in get().
//
//nolint:bodyclose // closed by the harness's t.Cleanup
package opds_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestRootFeedIsNavigation(t *testing.T) {
	h := newHarness(t)
	resp := h.get("/opds", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "kind=navigation") {
		t.Errorf("content type = %q, want a navigation feed", ct)
	}
	f := parseFeed(t, resp)
	// The one entry descends into the acquisition feed.
	if len(f.Entries) != 1 {
		t.Fatalf("root feed has %d entries, want 1", len(f.Entries))
	}
	sub := f.Entries[0].Links
	if len(sub) != 1 || sub[0].Href != "/opds/publications" {
		t.Fatalf("root entry link = %+v, want the publications subsection", sub)
	}
	if !strings.Contains(sub[0].Type, "kind=acquisition") {
		t.Errorf("subsection type = %q, want acquisition", sub[0].Type)
	}
}

func TestAuthChallenge(t *testing.T) {
	h := newHarness(t)
	resp := h.get("/opds", false) // no credential
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if wa := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(wa, "Basic") {
		t.Errorf("WWW-Authenticate = %q, want a Basic challenge", wa)
	}
}

func TestAuthWrongPassword(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest(http.MethodGet, h.http.URL+"/opds", nil)
	req.SetBasicAuth("reader", "not-the-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestPublicationsFeed(t *testing.T) {
	h := newHarness(t)
	resp := h.get("/opds/publications", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "kind=acquisition") {
		t.Errorf("content type = %q, want an acquisition feed", ct)
	}
	f := parseFeed(t, resp)

	// Exactly one entry: The Long Survey. Marginalia's only format is linked
	// (no blob) so it is not acquirable and has no entry; the movie is not a
	// publication.
	if len(f.Entries) != 1 {
		t.Fatalf("acquisition feed has %d entries, want 1 (%+v)", len(f.Entries), entryTitles(f))
	}
	e := f.Entries[0]
	if e.Title != "The Long Survey" {
		t.Errorf("entry title = %q, want The Long Survey", e.Title)
	}
	if len(e.Authors) != 1 || e.Authors[0].Name != "Ada Prentice" {
		t.Errorf("entry author = %+v, want Ada Prentice", e.Authors)
	}

	// Two acquisition links, one per streamable format, each with its media type.
	got := map[string]string{}
	for _, l := range e.Links {
		if l.Rel != "http://opds-spec.org/acquisition" {
			t.Errorf("unexpected link rel %q", l.Rel)
			continue
		}
		got[l.Href] = l.Type
	}
	if len(got) != 2 {
		t.Fatalf("acquisition links = %+v, want 2", got)
	}
	if got["/opds/download/ea1"] != "application/epub+zip" {
		t.Errorf("epub link type = %q", got["/opds/download/ea1"])
	}
	if got["/opds/download/ea2"] != "application/x-cbz" {
		t.Errorf("cbz link type = %q", got["/opds/download/ea2"])
	}
}

func entryTitles(f feedT) []string {
	out := make([]string, len(f.Entries))
	for i, e := range f.Entries {
		out[i] = e.Title
	}
	return out
}
