package mcp_test

import (
	"testing"
)

// The follow tools over the real JSON-RPC surface (§55, M12). They share
// resources.FollowSource/ListFollowed/Unfollow with the REST routes, so this
// asserts the MCP door reaches the same op — the "one intent, two doors"
// discipline want_content is built on.

func TestFollowSourceListAndUnfollow(t *testing.T) {
	h := newHarness(t, false)

	var created struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		FeedRef string `json:"feed_ref"`
	}
	h.call("", "follow_source",
		`{"tvdb_id":"321","title":"Some Show","quality_profile":"living-room","backfill":"full"}`).
		structured(t, &created)
	if created.Type != "tv_series" || created.FeedRef != "321" || created.ID == "" {
		t.Fatalf("follow_source returned %+v", created)
	}

	var listed struct {
		FollowedSources []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"followed_sources"`
	}
	h.call("", "list_followed", `{}`).structured(t, &listed)
	if len(listed.FollowedSources) != 1 || listed.FollowedSources[0].ID != created.ID {
		t.Fatalf("list_followed = %+v", listed.FollowedSources)
	}

	var done struct {
		SourceID string `json:"source_id"`
		Status   string `json:"status"`
	}
	h.call("", "unfollow", `{"source_id":"`+created.ID+`"}`).structured(t, &done)
	if done.SourceID != created.ID {
		t.Fatalf("unfollow = %+v", done)
	}

	h.call("", "list_followed", `{}`).structured(t, &listed)
	if len(listed.FollowedSources) != 0 {
		t.Errorf("after unfollow the list is not empty: %+v", listed.FollowedSources)
	}
}

// follow_source is source-agnostic: a non-TVDB source is refused with the
// Phase-1 message rather than stored.
func TestFollowSourceRefusesNonTVSeries(t *testing.T) {
	h := newHarness(t, false)

	resp := h.call("", "follow_source",
		`{"url":"https://example.com/feed.xml","title":"Pod","quality_profile":"living-room"}`)
	if resp.Body.Error == nil {
		t.Fatal("following a non-TVDB source should be an error in Phase 1")
	}
}
