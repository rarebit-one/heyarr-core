package musicbrainz

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const recordingFixture = `{"id":"12345678-1234-4234-8234-123456789abc","title":"Time After Time","disambiguation":"live","length":240001,"score":97,"artist-credit":[{"name":"Cyndi Lauper","joinphrase":" feat. ","artist":{"id":"11111111-1111-4111-8111-111111111111","name":"Cyndi Lauper"}},{"artist":{"id":"22222222-2222-4222-8222-222222222222","name":"Guest"}}],"releases":[{"id":"33333333-3333-4333-8333-333333333333","title":"Live","date":"1985"}]}`

func TestSearchRecordings(t *testing.T) {
	var gotQuery, gotUA string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/recording/" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gotQuery, gotUA = r.URL.Query().Get("query"), r.UserAgent()
		fmt.Fprintf(w, `{"recordings":[%s,{"id":"bad","title":"invalid"}]}`, recordingFixture)
	}))
	defer s.Close()
	c, err := New(Options{Name: "test", Endpoint: s.URL, UserAgent: "test/1 (contact)", MinInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.SearchRecordings(context.Background(), `Artist "One"`, `Song \ Title`)
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != `recording:"Song \\ Title" AND artist:"Artist \"One\""` {
		t.Errorf("query = %q", gotQuery)
	}
	if gotUA != "test/1 (contact)" {
		t.Errorf("user agent = %q", gotUA)
	}
	if len(out) != 1 {
		t.Fatalf("results = %+v", out)
	}
	x := out[0]
	if x.ID != "12345678-1234-4234-8234-123456789abc" || x.Title != "Time After Time" || x.DurationMS != 240001 || x.Score != 97 || x.Disambiguation != "live" {
		t.Errorf("recording = %+v", x)
	}
	if len(x.Artists) != 2 || x.Artists[0].JoinPhrase != " feat. " || x.Artists[1].Name != "Guest" || len(x.Releases) != 1 || x.Releases[0].Date != "1985" {
		t.Errorf("credits and releases = %+v", x)
	}
	if _, err = c.SearchRecordings(context.Background(), "", "title"); err == nil {
		t.Error("empty artist accepted")
	}
}

func TestLookupRecording(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/recording/") || !strings.Contains(r.URL.Query().Get("inc"), "artist-credits") {
			t.Errorf("request = %s", r.URL.String())
		}
		fmt.Fprint(w, recordingFixture)
	}))
	defer s.Close()
	c, err := New(Options{Name: "test", Endpoint: s.URL, MinInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.LookupRecording(context.Background(), "12345678-1234-4234-8234-123456789abc")
	if err != nil || got.Title != "Time After Time" {
		t.Fatalf("got = %+v, err = %v", got, err)
	}
	if _, err := c.LookupRecording(context.Background(), "not-a-uuid"); err == nil {
		t.Error("invalid ID accepted")
	}
	if _, err := c.LookupRecording(context.Background(), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"); err == nil {
		t.Error("mismatched returned ID accepted")
	}
}
