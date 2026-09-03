//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Followed sources over the real HTTP surface (§55, M12). The follow op and its
// REST shell are exercised together, because the seam a downstream client
// consumes is the route, not the Go method.

func follow(h *harness, body string) *http.Response {
	return h.doStable(http.MethodPost, "/api/v1/followed-sources", strings.NewReader(body))
}

type followedView struct {
	ID               string `json:"id"`
	WorkID           string `json:"work_id"`
	Title            string `json:"title"`
	Type             string `json:"type"`
	FeedRef          string `json:"feed_ref"`
	QualityProfileID string `json:"quality_profile_id"`
	Monitor          bool   `json:"monitor"`
	Backfill         string `json:"backfill"`
	ItemsKnown       int    `json:"items_known"`
	ItemsArchived    int    `json:"items_archived"`
	Health           string `json:"health"`
}

// A follow by title creates the series work and stores an inferred tv_series
// source with the TVDB id as its feed_ref.
func TestFollowASeriesByTitleInfersTVSeries(t *testing.T) {
	h := newHarness(t).seed()

	resp := follow(h, `{"tvdb_id":"12345","title":"The Series","quality_profile":"living-room","backfill":"full"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("follow = %d: %s", resp.StatusCode, h.body(resp))
	}
	var v followedView
	if err := json.Unmarshal(h.body(resp), &v); err != nil {
		t.Fatal(err)
	}
	if v.Type != "tv_series" {
		t.Errorf("type = %q, want tv_series (inferred, not asked)", v.Type)
	}
	if v.FeedRef != "12345" {
		t.Errorf("feed_ref = %q, want the TVDB id 12345", v.FeedRef)
	}
	if v.WorkID == "" || v.Backfill != "full" || !v.Monitor {
		t.Errorf("unexpected view: %+v", v)
	}
	if resp.Header.Get("Location") == "" {
		t.Error("a created source has no Location header")
	}
	// No metadata provider is configured in this harness, so health is unknown.
	if v.Health != "unknown" {
		t.Errorf("health = %q, want unknown with no feed adapter configured", v.Health)
	}
}

// The type is inferred from the identity: a TVDB URL with a numeric series id is
// a tv_series (feed_ref = the id); any other http(s) feed URL is a podcast
// (feed_ref = the URL itself); an identity that is neither a tvdb id nor an
// http(s) URL is refused rather than stored unpolled.
func TestFollowInfersFromAURL(t *testing.T) {
	h := newHarness(t).seed()

	ok := follow(h, `{"url":"https://thetvdb.com/series/98765","title":"Another","quality_profile":"living-room"}`)
	if ok.StatusCode != http.StatusCreated {
		t.Fatalf("a TVDB URL should be followed: %d %s", ok.StatusCode, h.body(ok))
	}
	var v followedView
	_ = json.Unmarshal(h.body(ok), &v)
	if v.FeedRef != "98765" {
		t.Errorf("feed_ref = %q, want 98765 parsed from the URL", v.FeedRef)
	}
	if v.Type != "tv_series" {
		t.Errorf("type = %q, want tv_series", v.Type)
	}

	pod := follow(h, `{"url":"https://feeds.example.com/show.xml","title":"A Show","quality_profile":"living-room"}`)
	if pod.StatusCode != http.StatusCreated {
		t.Fatalf("a podcast feed URL should be followed: %d %s", pod.StatusCode, h.body(pod))
	}
	var pv followedView
	_ = json.Unmarshal(h.body(pod), &pv)
	if pv.Type != "podcast" {
		t.Errorf("type = %q, want podcast", pv.Type)
	}
	if pv.FeedRef != "https://feeds.example.com/show.xml" {
		t.Errorf("feed_ref = %q, want the feed URL itself", pv.FeedRef)
	}

	bad := follow(h, `{"url":"not-a-url","title":"X","quality_profile":"living-room"}`)
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("an identity that is neither a tvdb id nor an http(s) url should be refused: %d", bad.StatusCode)
	}
	if !strings.Contains(string(h.body(bad)), "http(s) feed url") {
		t.Errorf("the refusal should name the expected shape, said: %s", h.body(bad))
	}
}

// A YouTube channel is recognised from the URL alone (#415): both the channel-
// feed URL and a /channel/<id> URL carry the channel id, and the stored feed_ref
// is normalised to the canonical feed URL either way.
func TestFollowInfersYouTube(t *testing.T) {
	h := newHarness(t).seed()

	// Both URL forms carry the id and normalise to the canonical feed URL. Two
	// distinct channels, so the normalisation assertion is not confounded by the
	// dedupe that makes two URLs for the SAME channel one subscription.
	for _, tc := range []struct{ url, wantFeed string }{
		{
			"https://www.youtube.com/feeds/videos.xml?channel_id=UCaaaaaaaaaaaaaaaaaaaaaa",
			"https://www.youtube.com/feeds/videos.xml?channel_id=UCaaaaaaaaaaaaaaaaaaaaaa",
		},
		{
			"https://www.youtube.com/channel/UCbbbbbbbbbbbbbbbbbbbbbb",
			"https://www.youtube.com/feeds/videos.xml?channel_id=UCbbbbbbbbbbbbbbbbbbbbbb",
		},
	} {
		body := `{"url":"` + tc.url + `","title":"Channel ` + tc.url + `","quality_profile":"living-room"}`
		ok := follow(h, body)
		if ok.StatusCode != http.StatusCreated {
			t.Fatalf("a youtube URL (%s) should be followed: %d %s", tc.url, ok.StatusCode, h.body(ok))
		}
		var v followedView
		_ = json.Unmarshal(h.body(ok), &v)
		if v.Type != "youtube_channel" {
			t.Errorf("url %s: type = %q, want youtube_channel", tc.url, v.Type)
		}
		if v.FeedRef != tc.wantFeed {
			t.Errorf("url %s: feed_ref = %q, want the canonical feed URL %q", tc.url, v.FeedRef, tc.wantFeed)
		}
	}

	// A handle URL has no id without a lookup — refused, not guessed.
	handle := follow(h, `{"url":"https://www.youtube.com/@sometv","title":"H","quality_profile":"living-room"}`)
	if handle.StatusCode != http.StatusBadRequest {
		t.Errorf("a youtube @handle URL has no channel id and must be refused: %d", handle.StatusCode)
	}
}

// An explicit type disambiguates a plain feed URL (#415): a podcast RSS feed and
// an article RSS feed are identical at the URL, so type=rss_feed is how a caller
// follows a feed's articles rather than treat it as a podcast. And a type that
// contradicts the identity is refused by name.
func TestFollowTypeHint(t *testing.T) {
	h := newHarness(t).seed()

	rss := follow(h, `{"url":"https://blog.example.com/rss.xml","type":"rss_feed","title":"A Blog","quality_profile":"living-room"}`)
	if rss.StatusCode != http.StatusCreated {
		t.Fatalf("type=rss_feed on a feed URL should be followed: %d %s", rss.StatusCode, h.body(rss))
	}
	var rv followedView
	_ = json.Unmarshal(h.body(rss), &rv)
	if rv.Type != "rss_feed" {
		t.Errorf("type = %q, want rss_feed (the caller named it)", rv.Type)
	}

	// The same URL with no hint stays a podcast (backward compatible).
	pod := follow(h, `{"url":"https://blog.example.com/other.xml","title":"Another","quality_profile":"living-room"}`)
	var pv followedView
	_ = json.Unmarshal(h.body(pod), &pv)
	if pv.Type != "podcast" {
		t.Errorf("no hint: type = %q, want podcast (the backward-compatible default)", pv.Type)
	}

	// A hint that contradicts the identity is a caller error.
	conflict := follow(h, `{"tvdb_id":"12345","type":"podcast","title":"X","quality_profile":"living-room"}`)
	if conflict.StatusCode != http.StatusBadRequest {
		t.Errorf("type=podcast with a tvdb_id contradicts the identity and must be refused: %d", conflict.StatusCode)
	}

	// A not-yet-implemented / unknown type is refused by name.
	bad := follow(h, `{"url":"https://x.example.com/f.xml","type":"nonsense","title":"X","quality_profile":"living-room"}`)
	if bad.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown type must be refused: %d", bad.StatusCode)
	}
}

func TestFollowRefusesMissingAndConflictingInputs(t *testing.T) {
	h := newHarness(t).seed()

	cases := []struct {
		name, body string
		wantStatus int
		wantMsg    string
	}{
		{
			"no feed identity", `{"title":"X","quality_profile":"living-room"}`,
			http.StatusBadRequest, "feed identity",
		},
		{
			"no series identity", `{"tvdb_id":"1","quality_profile":"living-room"}`,
			http.StatusBadRequest, "name the series",
		},
		{
			"no profile", `{"tvdb_id":"1","title":"X"}`,
			http.StatusBadRequest, "quality profile",
		},
		{
			"retention reserved", `{"tvdb_id":"1","title":"X","quality_profile":"living-room","retention":"30d"}`,
			http.StatusBadRequest, "retention",
		},
		{
			"non-numeric tvdb_id", `{"tvdb_id":"abc","title":"X","quality_profile":"living-room"}`,
			http.StatusBadRequest, "numeric",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := follow(h, tc.body)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.wantStatus, h.body(resp))
			}
			if !strings.Contains(string(h.body(resp)), tc.wantMsg) {
				t.Errorf("message should mention %q", tc.wantMsg)
			}
		})
	}
}

// Following the same series through the same feed twice is one subscription
// written twice — a 409.
func TestFollowingTwiceIsAConflict(t *testing.T) {
	h := newHarness(t).seed()
	body := `{"tvdb_id":"555","title":"Repeat","quality_profile":"living-room"}`
	if r := follow(h, body); r.StatusCode != http.StatusCreated {
		t.Fatalf("first follow = %d", r.StatusCode)
	}
	// Same work (resolved from the same title) + same feed_ref.
	if r := follow(h, body); r.StatusCode != http.StatusConflict {
		t.Fatalf("second follow = %d, want 409", r.StatusCode)
	}
}

func TestListAndUnfollow(t *testing.T) {
	h := newHarness(t).seed()
	create := follow(h, `{"tvdb_id":"777","title":"Listed","quality_profile":"living-room"}`)
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("follow = %d", create.StatusCode)
	}
	var v followedView
	_ = json.Unmarshal(h.body(create), &v)

	listed := h.get("/api/v1/followed-sources")
	if listed.StatusCode != http.StatusOK {
		t.Fatalf("list = %d", listed.StatusCode)
	}
	var page struct {
		FollowedSources []followedView `json:"followed_sources"`
	}
	if err := json.Unmarshal(h.body(listed), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.FollowedSources) != 1 || page.FollowedSources[0].ID != v.ID {
		t.Fatalf("list did not return the created source: %+v", page.FollowedSources)
	}

	// keep_archive=false is refused in Phase 1.
	refused := h.doStable(http.MethodDelete, "/api/v1/followed-sources/"+v.ID+"?keep_archive=false", nil)
	if refused.StatusCode != http.StatusBadRequest {
		t.Fatalf("keep_archive=false should be refused: %d", refused.StatusCode)
	}

	// The default (keep_archive true) unfollows.
	del := h.doStable(http.MethodDelete, "/api/v1/followed-sources/"+v.ID, nil)
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("unfollow = %d, want 204: %s", del.StatusCode, h.body(del))
	}
	// A second delete is a 404 — nothing there.
	if again := h.doStable(http.MethodDelete, "/api/v1/followed-sources/"+v.ID, nil); again.StatusCode != http.StatusNotFound {
		t.Fatalf("deleting a gone source = %d, want 404", again.StatusCode)
	}
}

// Content-intent search finds a work by its normalised title, with no source in
// the question.
func TestContentIntentSearch(t *testing.T) {
	h := newHarness(t).seed()
	resp := h.doStable(http.MethodPost, "/api/v1/search",
		strings.NewReader(`{"query":"arrival"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search = %d: %s", resp.StatusCode, h.body(resp))
	}
	var out struct {
		Works []struct {
			WorkID string `json:"work_id"`
			Title  string `json:"title"`
		} `json:"works"`
	}
	if err := json.Unmarshal(h.body(resp), &out); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range out.Works {
		if w.Title == "Arrival" {
			found = true
		}
	}
	if !found {
		t.Errorf("search for \"arrival\" did not find Arrival: %+v", out.Works)
	}

	// A search with nothing to search on is refused.
	empty := h.doStable(http.MethodPost, "/api/v1/search", strings.NewReader(`{}`))
	if empty.StatusCode != http.StatusBadRequest {
		t.Errorf("an empty search = %d, want 400", empty.StatusCode)
	}
}
