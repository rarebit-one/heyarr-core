package mcp

// The tool input schemas, hand-written.
//
// # Why these are authored rather than reflected
//
// A tool schema is an interface contract with the same permanence as an
// endpoint — more, because an agent was built against the field names and
// there is no deprecation header an agent reads. ADR-0015 gives the same
// reasoning for a hand-written OpenAPI document: a schema generated from a
// struct changes silently when the struct does, and the change reaches every
// consumer before anyone has decided it should.
//
// They are also DOCUMENTATION for a reader who is not a person. The
// descriptions here are the only thing standing between an agent and guessing,
// so they say what a field means and when to use it rather than restating its
// name.

// obj is a small helper so the schemas read as shapes rather than as maps.
func obj(props map[string]any, required ...string) map[string]any {
	out := map[string]any{
		"type":       "object",
		"properties": props,
		// Unknown fields are refused at decode time, so declaring it here
		// keeps the schema honest about what will actually happen rather than
		// letting an agent believe an extra field is merely ignored.
		"additionalProperties": false,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// schemaNoArgs is for tools that take nothing: their handlers refuse any field.
var schemaNoArgs = obj(map[string]any{})

// schemaLimit is for tools whose only argument is a row limit.
var schemaLimit = obj(map[string]any{
	"limit": rowLimit("How many rows at most. Defaults to the maximum."),
})

var schemaSearchContent = obj(map[string]any{
	"query": str("Part of a title. Matched against the normalised form the " +
		"scanner records, so case and leading articles do not matter."),
	"content_type": str("Narrow to one kind: movie, series, music, book."),
	"limit":        rowLimit("How many works at most. Defaults to the maximum."),
})

var schemaDiscoverContent = obj(map[string]any{
	"query": str("The title to look up. Sent to the metadata provider as free text, " +
		"so it need not match anything already in the library — that is the point."),
}, "query")

var schemaGetExternalIDs = obj(map[string]any{
	"work_id": str("Forward lookup: a work (from search_content) whose external " +
		"ids you want. Give this OR edition_id OR a source+value pair."),
	"edition_id": str("Forward lookup: an edition whose external ids you want."),
	"source": str("Reverse lookup: the identifier scheme, e.g. tmdb or imdb. " +
		"Give together with value to find which work or edition carries it."),
	"value": str("Reverse lookup: the identifier's value, e.g. 603. Give with source."),
})

var schemaWantContent = obj(map[string]any{
	"work_id": str("An existing work, from search_content. Exact — prefer this " +
		"when the library already has the content."),
	"title": str("The title, for content the library has never seen. The work " +
		"is created from it, using the same normalisation a scan would, so wanting " +
		"something and later scanning it converge on one work. Give either this or " +
		"work_id, never both."),
	"content_type": str("Required with title: movie, series, music, book."),
	"year": integer("Part of the identity when known. A year inside the title is " +
		"understood too; an explicit one wins."),
	"quality_profile": str("The standard this want is measured against, named as a person " +
		"would: \"living-room\", \"everyday\", \"archival\". Required — \"this should " +
		"exist\" with no statement of what would count as existing cannot be evaluated."),
	"monitor": boolean("Keep looking for something better after it is satisfied. " +
		"Defaults to true."),
	"reason": str("A note for whoever reads this in six months. Never interpreted."),
}, "quality_profile")

var schemaMonitorContent = obj(map[string]any{
	"desired_item_id": str("The want, from get_missing_content or want_content."),
	"monitor": boolean("True to keep looking for something better, false to stop once " +
		"it is satisfied. Required — there is no safe default for a change nobody " +
		"asked for."),
}, "desired_item_id", "monitor")

// schemaFollowSource is source-agnostic on purpose (#396): there is no `source`
// or `provider` field. The caller gives a content intent (which series, podcast,
// channel or feed) and an identity (a URL or an explicit id), and the type is
// inferred where the URL allows it. `type` is the one exception (#415): a podcast
// RSS feed and an article RSS feed are the same shape at the URL, so following an
// rss_feed needs it named — it is not a routing knob, it disambiguates identity.
var schemaFollowSource = obj(map[string]any{
	"url": str("A URL identifying the source to follow — a TVDB series URL, a " +
		"youtube.com channel-feed URL, or any other http(s) feed URL (a podcast or " +
		"article RSS feed). The type is inferred from it where possible — you do not name " +
		"a source or a provider."),
	"tvdb_id": str("A TVDB series id, as an alternative to url when you have the id " +
		"directly. Numeric."),
	"type": strEnum("Only when the URL cannot say it on its own: a podcast RSS feed and an "+
		"article feed look identical, so pass rss_feed to archive a feed's articles rather "+
		"than treat it as a podcast. Leave empty to infer (a plain feed URL is a podcast).", "tv_series", "podcast", "youtube_channel", "rss_feed"),
	"work_id": str("An existing series or podcast work, from search_content. Give this " +
		"or title, never both."),
	"title": str("The series or podcast title, for a work the library has never seen. " +
		"The work is created from it the same way want_content does, so a follow and a " +
		"later scan converge on one work."),
	"year": integer("Part of the work's identity when known."),
	"quality_profile": str("The standard every episode this source archives is measured against, " +
		"named as a person would: \"living-room\". Required — every projected want inherits it."),
	"monitor": boolean("Keep looking for a better copy of each episode after it is satisfied. " +
		"Defaults to true."),
	"backfill": strEnum("How much back-catalogue to pull on the first poll. from_now (the "+
		"default) archives only episodes that air after you follow; full walks the whole "+
		"back-catalogue into wants — a real capacity commitment.", "from_now", "full"),
	"reason": str("A note for whoever reads this in six months — \"Kate watches this\". Never interpreted."),
	"want_subtitles": strArray("Languages (ISO-639-1 codes, e.g. [\"en\"]) to want a subtitle in for every " +
		"episode this source projects. Each poll projects a subtitle want per language; the " +
		"fetch driver acquires each once the episode's video is held. Optional; empty wants none."),
}, "quality_profile")

// schemaUnfollow stops a subscription. keep_archive defaults to true — stop
// polling, keep what was archived.
var schemaUnfollow = obj(map[string]any{
	"source_id": str("The followed source to stop, from list_followed."),
	"keep_archive": boolean("Keep the episodes already archived (the default, true). Phase 1 " +
		"always keeps the archive, so false is refused."),
}, "source_id")

// schemaPollSource forces one followed source to poll now.
var schemaPollSource = obj(map[string]any{
	"source_id": str("The followed source to poll now, from list_followed."),
}, "source_id")

// schemaSetSourceProfile repoints a subscription at a different quality profile.
var schemaSetSourceProfile = obj(map[string]any{
	"source_id": str("The followed source to repoint, from list_followed."),
	"quality_profile": str("The quality profile to move to, named as a person would: " +
		"\"published\" for articles and podcasts, \"everyday\" for video. Every want this " +
		"source has projected is re-judged against it at once. Optional if backfill is given."),
	"backfill": strEnum("How much back-catalogue polls project from now on. full makes the "+
		"next poll (queued for you) project every item the feed has ever listed — the "+
		"whole archive, a real capacity commitment. Optional if quality_profile is given.", "from_now", "full"),
	"want_subtitles": strArray("Replace the subtitle languages (ISO-639-1 codes, e.g. [\"en\"]) this " +
		"source wants a subtitle in for every episode. Felt on the next poll (queued for " +
		"you). Omit to leave unchanged; an empty array wants none. Optional."),
}, "source_id")

var schemaDesiredItemID = obj(map[string]any{
	"desired_item_id": str("The want to explain."),
}, "desired_item_id")

var schemaListJobs = obj(map[string]any{
	"state": strEnum("Only jobs in this state. `failed` is a spent attempt the queue "+
		"will retry with backoff; `dead` is terminal until an operator retries it. "+
		"Omit for any state.", "pending", "leased", "succeeded", "failed", "dead"),
	"type": str("Only jobs of this type, e.g. search_release, grab_release, " +
		"poll_downloads, ingest_acquisition. Omit for any type."),
	"limit": rowLimit("How many jobs at most, most recent first. Defaults to the maximum."),
})

var schemaBlobHash = obj(map[string]any{
	"blob_hash": str("The canonical blob digest, `blake3:` followed by 64 hex characters."),
}, "blob_hash")

var schemaSyncPeer = obj(map[string]any{
	"peer": str("The peer to reconcile against, by id or by name. It must be a " +
		"peer other than this node: a node does not synchronise with itself."),
}, "peer")

var schemaExplainRelease = obj(map[string]any{
	"quality_profile": str("The profile to score against, by name."),
	"releases": map[string]any{
		"type":     "array",
		"minItems": 1,
		"maxItems": maxRows,
		"items": obj(map[string]any{
			"id": str("Your identifier for this release. Used to break ties, so " +
				"supplying one makes the ranking independent of the order you sent them in."),
			"title": str("What the release is called. Never parsed — put what you " +
				"know into attributes instead."),
			"attributes": map[string]any{
				"type": "object",
				"description": "What you know about the release. LEAVE A KEY OUT when you " +
					"cannot tell: an absent attribute is reported as `undetermined`, which " +
					"is a different answer from a wrong one and sends a person somewhere " +
					"different. Guessing a value produces a confident wrong verdict.",
				"properties": map[string]any{
					"resolution": integer("Vertical lines: 480, 720, 1080, 2160. Not a label — " +
						"\"4K\", \"2160p\" and \"UHD\" are three spellings of one number."),
					"source":         str("remux, bluray, web-dl, webrip, hdtv, dvd, cam."),
					"video_codec":    str(""),
					"audio_codec":    str(""),
					"audio_channels": integer(""),
					"hdr":            boolean(""),
					"size_bytes":     integer(""),
					"language":       str(""),
				},
				"additionalProperties": false,
			},
		}),
	},
}, "quality_profile", "releases")

// schemaBrowseLibrary is browse_library's input (ADR-0075). Every field is an
// optional filter; with none it is the whole catalog, newest or A–Z. It is the
// browsing counterpart to search_content: search_content resolves a title,
// this walks a shelf.
var schemaBrowseLibrary = obj(map[string]any{
	"content_type": str("Narrow to one kind: movie, series, music, book."),
	"library_id":   str("Only works with something of theirs in this library."),
	"q": str("Part of a title. Matched against the normalised form the scanner " +
		"records, so case and leading articles do not matter."),
	"artist": str("Only this artist's works — the exact name list_artists returns. " +
		"Pair with content_type=music."),
	"author": str("Only this author's works — the exact name list_authors returns. " +
		"Pair with content_type=book."),
	"year":      integer("Only works of exactly this year."),
	"year_from": integer("Only works of this year or later (inclusive)."),
	"year_to":   integer("Only works of this year or earlier (inclusive)."),
	"sort": strEnum("title (the default) is A–Z by the normalised title; recent is "+
		"newest-added first.", "title", "recent"),
	"include_artwork": boolean("Attach each work's poster (null when it has none). Defaults to true."),
	"include_primary_asset": boolean("Attach the one file a card tap would play, with its size and " +
		"duration when known (null when the work holds no file). Defaults to true."),
	"limit": rowLimit("How many works at most. Defaults to the maximum."),
})

// schemaGrouping is shared by list_artists and list_authors: a name substring
// filter and a limit. The grouping is over the attribute a scan wrote, not an
// entity, so there is nothing else to ask for.
var schemaGrouping = obj(map[string]any{
	"q":     str("Part of the name to narrow to. Case-insensitive substring."),
	"limit": rowLimit("How many names at most. Defaults to the maximum."),
})

// schemaContinueRail is continue_rail's input (ADR-0075).
var schemaContinueRail = obj(map[string]any{
	"device_id": str("Only sessions on this renderer, from get_peer_status's siblings. Omit for all."),
	"limit":     rowLimit("How many works at most. Defaults to the maximum."),
})

// schemaFollowedSourceItems is followed_source_items' input (#430).
var schemaFollowedSourceItems = obj(map[string]any{
	"source_id": str("The followed source whose items to list, from list_followed."),
	"limit":     rowLimit("How many items at most, oldest feed-key first. Defaults to the maximum."),
}, "source_id")

// schemaSearchReleases is search_releases' input (§71).
var schemaSearchReleases = obj(map[string]any{
	"desired_item_id": str("The want to look for releases of."),
}, "desired_item_id")

// schemaAcquireRelease is acquire_release's input (§71).
//
// Both arguments are required. A candidate id with no want is ambiguous — the
// same release may be a candidate for several wants — and a want with no
// candidate would make this "acquire something", which is what the scorer is
// for.
var schemaAcquireRelease = obj(map[string]any{
	"desired_item_id": str("The want the release should satisfy."),
	"candidate_id": str("The candidate to acquire, from get_content_satisfaction or a " +
		"prior search. It must be one this want's last search returned."),
}, "desired_item_id", "candidate_id")

// The property helpers. Each returns the same map a literal would, so the
// published schema is unchanged; they exist so a schema reads as a list of
// fields rather than as a wall of "type" keys. An empty description is
// omitted, which is what a bare {"type": ...} literal said.

func prop(typ, desc string) map[string]any {
	p := map[string]any{"type": typ}
	if desc != "" {
		p["description"] = desc
	}
	return p
}

// str is a string property.
func str(desc string) map[string]any { return prop("string", desc) }

// integer is an integer property.
func integer(desc string) map[string]any { return prop("integer", desc) }

// boolean is a boolean property.
func boolean(desc string) map[string]any { return prop("boolean", desc) }

// strEnum is a string property restricted to values.
func strEnum(desc string, values ...any) map[string]any {
	p := str(desc)
	p["enum"] = values
	return p
}

// strArray is an array-of-strings property.
func strArray(desc string) map[string]any {
	p := prop("array", desc)
	p["items"] = map[string]any{"type": "string"}
	return p
}

// rowLimit is the `limit` every listing takes: an integer from 1 to maxRows.
func rowLimit(desc string) map[string]any {
	p := integer(desc)
	p["minimum"] = 1
	p["maximum"] = maxRows
	return p
}
