package mcp

import "github.com/rarebit-one/heyarr-core/internal/auth"

// registerFollowedTools registers the followed-source lane (§55): standing
// subscriptions, as distinct from a one-off want.
func (s *Server) registerFollowedTools() {
	s.tools.register(Tool{
		Name:     "follow_source",
		Title:    "Follow a source",
		Scope:    auth.ScopeWrite,
		ReadOnly: false,
		Description: "Subscribe to a source and archive everything it emits, forever — a " +
			"STANDING subscription, distinct from want_content, which gets one thing once. " +
			"Give a content intent (which series or podcast) and an identity (a url, or a " +
			"tvdb_id); the type is inferred, you never name a source or a provider. A TVDB id " +
			"or URL is a TV series; any other http(s) feed URL is a podcast.",
		InputSchema: schemaFollowSource,
		Handler:     s.followSource,
	})

	s.tools.register(Tool{
		Name:     "list_followed",
		Title:    "What is followed",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "List every followed source with how many items its feed has yielded, " +
			"how many are archived, when it was last polled and when it is due next, and " +
			"whether its feed adapter is healthy. Read this to answer \"is this working\".",
		InputSchema: schemaLimit,
		Handler:     s.listFollowed,
	})

	s.tools.register(Tool{
		Name:     "unfollow",
		Title:    "Stop following a source",
		Scope:    auth.ScopeWrite,
		ReadOnly: false,
		Description: "Stop a subscription. By default it stops future polls and KEEPS every " +
			"episode already archived (keep_archive true). Phase 1 always keeps the archive; " +
			"asking to remove it is refused.",
		InputSchema: schemaUnfollow,
		Handler:     s.unfollow,
	})

	s.tools.register(Tool{
		Name:     "poll_source",
		Title:    "Poll a source now",
		Scope:    auth.ScopeWrite,
		ReadOnly: false,
		Description: "Poll one followed source now instead of waiting for its next scheduled " +
			"round, which can be up to six hours out. Reach for it after following something and " +
			"not wanting to wait, or when a feed just posted. It QUEUES the poll and returns a job " +
			"— a feed host can be slow, so nothing holds while it runs — and leaves the source's " +
			"schedule alone: this is an EXTRA poll, not a reschedule. Idempotent: asking again while " +
			"a poll is still queued collapses to the one job. Read list_followed afterwards to see " +
			"when it last polled and what it found.",
		InputSchema: schemaPollSource,
		Handler:     s.pollSource,
	})

	s.tools.register(Tool{
		Name:     "set_source_profile",
		Title:    "Repoint a source at a quality profile",
		Scope:    auth.ScopeWrite,
		ReadOnly: false,
		Description: "Change a followed source's strategy IN PLACE, without unfollowing it: which " +
			"quality profile it — and every item it has already archived — is judged against, " +
			"and/or its backfill. Reach for the profile when a feed is on the wrong one: an article " +
			"or podcast feed on a video profile never counts as archived, because a captured page " +
			"has no resolution for the video profile's gate to pass; move it to \"published\" and its " +
			"held items are re-judged and become archived at once. Reach for backfill=full when a " +
			"from_now follow should now archive its whole back-catalogue: the poll it queues " +
			"projects every item the feed has ever listed — a real capacity commitment. Give at " +
			"least one of the two. Name the profile as a person would, the same as follow_source.",
		InputSchema: schemaSetSourceProfile,
		Handler:     s.setSourceProfile,
	})

	s.tools.register(Tool{
		Name:     "followed_source_items",
		Title:    "What a followed source has",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "Detail ONE followed source: the subscription itself plus every item its feed " +
			"has yielded — what is archived and what it merely knows about, each with the want it " +
			"projected and that want's state. Where list_followed answers \"is this working\" across " +
			"all subscriptions, this answers \"what has this one got\" for a single source.",
		InputSchema: schemaFollowedSourceItems,
		Handler:     s.followedSourceItems,
	})
}
