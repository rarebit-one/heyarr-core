package mcp_test

import (
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// The follow tools over the real JSON-RPC surface (§55, M12). They share
// resources.FollowSource/ListFollowed/Unfollow with the REST routes, so this
// asserts the MCP door reaches the same op — the "one intent, two doors"
// discipline want_content is built on.

func TestFollowSourceListAndUnfollow(t *testing.T) {
	t.Parallel()
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

// follow_source is source-agnostic: an http(s) feed URL is inferred as a podcast
// and followed, while an identity that is neither a tvdb id nor an http(s) URL is
// refused rather than stored unpolled.
func TestFollowSourceInfersPodcastAndRefusesJunk(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false)

	var followed struct {
		Type    string `json:"type"`
		FeedRef string `json:"feed_ref"`
	}
	h.call("", "follow_source",
		`{"url":"https://example.com/feed.xml","title":"Pod","quality_profile":"living-room"}`).
		structured(t, &followed)
	if followed.Type != "podcast" {
		t.Errorf("a feed URL should be followed as a podcast, got type %q", followed.Type)
	}
	if followed.FeedRef != "https://example.com/feed.xml" {
		t.Errorf("feed_ref = %q, want the feed URL itself", followed.FeedRef)
	}

	resp := h.call("", "follow_source",
		`{"url":"not-a-url","title":"X","quality_profile":"living-room"}`)
	if resp.Body.Error == nil {
		t.Fatal("an identity that is neither a tvdb id nor an http(s) url should be an error")
	}
}

// poll_source forces a followed source to poll now, through the same
// resources.PollSource the REST route uses — so the MCP door reaches the same
// enqueue. It queues a job and says so; an unknown id is a quotable not-found.
func TestPollSourceTool(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false)

	var created struct {
		ID string `json:"id"`
	}
	h.call("", "follow_source",
		`{"tvdb_id":"654","title":"Pollable","quality_profile":"living-room"}`).
		structured(t, &created)
	if created.ID == "" {
		t.Fatal("follow_source did not create a source")
	}

	var polled struct {
		SourceID string `json:"source_id"`
		JobID    string `json:"job_id"`
		Status   string `json:"status"`
	}
	h.call("", "poll_source", `{"source_id":"`+created.ID+`"}`).structured(t, &polled)
	if polled.SourceID != created.ID || polled.Status != "queued" || polled.JobID == "" {
		t.Fatalf("poll_source = %+v, want the source id, queued, and a job id", polled)
	}

	// An unknown id is invalid-params with a quotable message, not a 500.
	miss := h.call("", "poll_source", `{"source_id":"nope"}`)
	if miss.Body.Error == nil {
		t.Fatal("poll_source on an unknown id should be an error")
	}
	if miss.Body.Error.Code != -32602 {
		t.Errorf("code = %d, want -32602 (invalid params)", miss.Body.Error.Code)
	}
	if !strings.Contains(miss.Body.Error.Message, "no followed source") {
		t.Errorf("the refusal should say there is no such source; got %q", miss.Body.Error.Message)
	}

	// Missing source_id is refused before anything is enqueued.
	if bad := h.call("", "poll_source", `{}`); bad.Body.Error == nil {
		t.Error("poll_source with no source_id should be refused")
	}
}

// poll_source is a mutating verb: it declares write scope and is not read-only,
// so an MCP client's confirmation prompt treats it as a change.
func TestPollSourceIsAWriteTool(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false)
	var found bool
	for _, tool := range h.server.Tools() {
		if tool.Name != "poll_source" {
			continue
		}
		found = true
		if tool.Scope != auth.ScopeWrite {
			t.Errorf("poll_source scope = %q, want write", tool.Scope)
		}
		if tool.ReadOnly {
			t.Error("poll_source is marked read-only but it enqueues a poll")
		}
	}
	if !found {
		t.Fatal("poll_source is not registered")
	}
}

// A read token cannot force a poll — poll_source changes what will be fetched,
// so the scope middleware refuses it (forbidden, not invalid-params).
func TestPollSourceNeedsWriteScope(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true)
	read := h.mint("reader", auth.ScopeRead)
	resp := h.call(read, "poll_source", `{"source_id":"whatever"}`)
	if resp.Body.Error == nil {
		t.Fatal("a read token called poll_source and was allowed")
	}
	if resp.Body.Error.Code != -32001 {
		t.Errorf("code = %d, want -32001 (forbidden)", resp.Body.Error.Code)
	}
	if !strings.Contains(resp.Body.Error.Message, "write") {
		t.Errorf("the refusal should name the scope; got %q", resp.Body.Error.Message)
	}
}

// list_followed decodes its arguments like every other tool: limit cuts the
// list and says so, a misspelled field is refused rather than ignored, and no
// arguments at all still lists everything.
func TestListFollowedHonoursItsArguments(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false)

	for _, id := range []string{"321", "322", "323"} {
		h.call("", "follow_source",
			`{"tvdb_id":"`+id+`","title":"Show `+id+`","quality_profile":"living-room"}`).
			structured(t, &struct{}{})
	}

	type listing struct {
		Count           int  `json:"count"`
		Truncated       bool `json:"truncated"`
		FollowedSources []struct {
			ID string `json:"id"`
		} `json:"followed_sources"`
	}

	var cut listing
	h.call("", "list_followed", `{"limit":2}`).structured(t, &cut)
	if len(cut.FollowedSources) != 2 || cut.Count != 2 || !cut.Truncated {
		t.Errorf("limit 2 of 3 = %+v, want two sources, count 2, truncated", cut)
	}

	var all listing
	h.call("", "list_followed", `{"limit":3}`).structured(t, &all)
	if len(all.FollowedSources) != 3 || all.Count != 3 || all.Truncated {
		t.Errorf("limit 3 of 3 = %+v, want three sources, not truncated", all)
	}

	var bare listing
	h.rpc("", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_followed"}}`).
		structured(t, &bare)
	if len(bare.FollowedSources) != 3 || bare.Truncated {
		t.Errorf("no arguments = %+v, want every source, not truncated", bare)
	}

	typo := h.call("", "list_followed", `{"limt":1}`)
	if typo.Body.Error == nil {
		t.Fatalf("a misspelled field was accepted: %s", typo.Raw)
	}
	if typo.Body.Error.Code != -32602 || !strings.Contains(typo.Body.Error.Message, "limt") {
		t.Errorf("error = %d %q, want -32602 naming the unknown field",
			typo.Body.Error.Code, typo.Body.Error.Message)
	}
}
