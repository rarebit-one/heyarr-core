package mcp

import "github.com/rarebit-one/heyarr-core/internal/auth"

// registerLibraryTools registers the catalog reads: resolving what someone
// means (search_content, discover_content, get_external_ids) and the browse lane
// (ADR-0075) that walks the catalog the way a consumer client presents it.
func (s *Server) registerLibraryTools() {
	s.tools.register(Tool{
		Name:     "search_content",
		Title:    "Search the library",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "Find works already in the library by title. Use this to resolve " +
			"what someone means before wanting it — a work found here can be wanted by " +
			"id, which is exact, rather than by description, which may create a second " +
			"work if it does not match what a scan would have produced. A tv_series hit " +
			"that carries a stored tvdb_id can be followed in one step by passing that id " +
			"to follow_source; a hit without one omits it. Each work carries its " +
			"attributes and its artwork (null when it has none); `episodes` lists the " +
			"parts of a work that matched by their own title — a scanned episode with " +
			"its file, or an item a followed source projected.",
		InputSchema: schemaSearchContent,
		Handler:     s.searchContent,
	})

	s.tools.register(Tool{
		Name:     "discover_content",
		Title:    "Discover new content",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "Find content the library does NOT already hold. Where search_content " +
			"looks only in the library, this asks every configured metadata provider (TVDB/TMDB " +
			"for series, TMDB for movies, Open Library for books, MusicBrainz for music) for " +
			"candidate works matching a free-text title, whether or not they are catalogued — " +
			"the \"search then acquire\" door. A tv_series result carries a tvdb_id you pass " +
			"straight to follow_source; every result (including movie/book/music) carries " +
			"source+external_id for reference and a type telling you which: a tv_series/podcast/" +
			"youtube_channel/rss_feed result is followed (follow_source), a movie/book/music " +
			"result has no calendar and is wanted instead (want_content by title+year+" +
			"content_type). Use this when search_content came back empty and someone wants " +
			"something new. Needs at least one metadata or enrich provider configured; a node " +
			"without one says so rather than returning nothing.",
		InputSchema: schemaDiscoverContent,
		Handler:     s.discoverContent,
	})

	s.tools.register(Tool{
		Name:     "get_external_ids",
		Title:    "Get external identifiers",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "Resolve external catalogue identifiers (tmdb, imdb) for a work or " +
			"edition, or reverse a source+value back to the work or edition that carries " +
			"it. Use this to reconcile an outside id to a heyarr work_id and back by id " +
			"rather than by a fuzzy title. Read-only; an unknown id returns an empty list.",
		InputSchema: schemaGetExternalIDs,
		Handler:     s.getExternalIDs,
	})

	// The browse lane (ADR-0075): shelf reads that map to the newer HTTP
	// browse handlers, each a shell over the same resources intent so the two
	// doors cannot drift. They complement search_content — resolving a title —
	// with walking the catalog the way a consumer client presents it.
	s.tools.register(Tool{
		Name:     "browse_library",
		Title:    "Browse the library",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "Walk the catalog the way a shelf presents it, rather than resolving one " +
			"title. Filter by content_type, year (exact or a from/to range), library, artist or " +
			"author, and sort A–Z or newest-added first; each work carries its poster and the one " +
			"file a tap would play (null when it has none). Reach for this to answer \"what do we " +
			"have\" and \"what is new\"; use search_content when you already know the title.",
		InputSchema: schemaBrowseLibrary,
		Handler:     s.browseLibrary,
	})

	s.tools.register(Tool{
		Name:     "list_artists",
		Title:    "List artists",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "List the artists the music library groups by, with how many works each has " +
			"and a representative cover. An artist is a grouping over what a scan wrote, not an " +
			"entity — pass a name back to browse_library as `artist` to get that artist's albums.",
		InputSchema: schemaGrouping,
		Handler:     s.listArtists,
	})

	s.tools.register(Tool{
		Name:     "list_authors",
		Title:    "List authors",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "List the authors the book library groups by, with how many works each has " +
			"and a representative cover. An author is a grouping over what a scan wrote, not an " +
			"entity — pass a name back to browse_library as `author` to get that author's books.",
		InputSchema: schemaGrouping,
		Handler:     s.listAuthors,
	})

	s.tools.register(Tool{
		Name:     "continue_rail",
		Title:    "Pick up where it stopped",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "List the newest not-finished session per work that has a position — the " +
			"\"continue\" rail a client draws to resume where playback stopped. Each row carries the " +
			"work, the part (season and episode for a series), the file with its duration, and the " +
			"stored position. These are resume points the node keeps server-side; the encrypted " +
			"user artifacts this server cannot decrypt stay in the separate device-side surface.",
		InputSchema: schemaContinueRail,
		Handler:     s.continueRail,
	})
}
