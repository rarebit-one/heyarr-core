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

// schemaNoArgs is for tools that take nothing.
var schemaNoArgs = obj(map[string]any{
	"limit": map[string]any{
		"type":        "integer",
		"minimum":     1,
		"maximum":     maxRows,
		"description": "How many rows at most. Defaults to the maximum.",
	},
})

var schemaSearchContent = obj(map[string]any{
	"query": map[string]any{
		"type": "string",
		"description": "Part of a title. Matched against the normalised form the " +
			"scanner records, so case and leading articles do not matter.",
	},
	"content_type": map[string]any{
		"type":        "string",
		"description": "Narrow to one kind: movie, series, music, book.",
	},
	"limit": map[string]any{
		"type": "integer", "minimum": 1, "maximum": maxRows,
		"description": "How many works at most. Defaults to the maximum.",
	},
})

var schemaDiscoverContent = obj(map[string]any{
	"query": map[string]any{
		"type": "string",
		"description": "The title to look up. Sent to the metadata provider as free text, " +
			"so it need not match anything already in the library — that is the point.",
	},
}, "query")

var schemaGetExternalIDs = obj(map[string]any{
	"work_id": map[string]any{
		"type": "string",
		"description": "Forward lookup: a work (from search_content) whose external " +
			"ids you want. Give this OR edition_id OR a source+value pair.",
	},
	"edition_id": map[string]any{
		"type":        "string",
		"description": "Forward lookup: an edition whose external ids you want.",
	},
	"source": map[string]any{
		"type": "string",
		"description": "Reverse lookup: the identifier scheme, e.g. tmdb or imdb. " +
			"Give together with value to find which work or edition carries it.",
	},
	"value": map[string]any{
		"type":        "string",
		"description": "Reverse lookup: the identifier's value, e.g. 603. Give with source.",
	},
})

var schemaWantContent = obj(map[string]any{
	"work_id": map[string]any{
		"type": "string",
		"description": "An existing work, from search_content. Exact — prefer this " +
			"when the library already has the content.",
	},
	"title": map[string]any{
		"type": "string",
		"description": "The title, for content the library has never seen. The work " +
			"is created from it, using the same normalisation a scan would, so wanting " +
			"something and later scanning it converge on one work. Give either this or " +
			"work_id, never both.",
	},
	"content_type": map[string]any{
		"type":        "string",
		"description": "Required with title: movie, series, music, book.",
	},
	"year": map[string]any{
		"type": "integer",
		"description": "Part of the identity when known. A year inside the title is " +
			"understood too; an explicit one wins.",
	},
	"quality_profile": map[string]any{
		"type": "string",
		"description": "The standard this want is measured against, named as a person " +
			"would: \"living-room\", \"everyday\", \"archival\". Required — \"this should " +
			"exist\" with no statement of what would count as existing cannot be evaluated.",
	},
	"monitor": map[string]any{
		"type": "boolean",
		"description": "Keep looking for something better after it is satisfied. " +
			"Defaults to true.",
	},
	"reason": map[string]any{
		"type":        "string",
		"description": "A note for whoever reads this in six months. Never interpreted.",
	},
}, "quality_profile")

var schemaMonitorContent = obj(map[string]any{
	"desired_item_id": map[string]any{
		"type":        "string",
		"description": "The want, from get_missing_content or want_content.",
	},
	"monitor": map[string]any{
		"type": "boolean",
		"description": "True to keep looking for something better, false to stop once " +
			"it is satisfied. Required — there is no safe default for a change nobody " +
			"asked for.",
	},
}, "desired_item_id", "monitor")

// schemaFollowSource is source-agnostic on purpose (#396): there is no `source`
// or `provider` field. The caller gives a content intent (which series, podcast,
// channel or feed) and an identity (a URL or an explicit id), and the type is
// inferred where the URL allows it. `type` is the one exception (#415): a podcast
// RSS feed and an article RSS feed are the same shape at the URL, so following an
// rss_feed needs it named — it is not a routing knob, it disambiguates identity.
var schemaFollowSource = obj(map[string]any{
	"url": map[string]any{
		"type": "string",
		"description": "A URL identifying the source to follow — a TVDB series URL, a " +
			"youtube.com channel-feed URL, or any other http(s) feed URL (a podcast or " +
			"article RSS feed). The type is inferred from it where possible — you do not name " +
			"a source or a provider.",
	},
	"tvdb_id": map[string]any{
		"type": "string",
		"description": "A TVDB series id, as an alternative to url when you have the id " +
			"directly. Numeric.",
	},
	"type": map[string]any{
		"type": "string",
		"enum": []any{"tv_series", "podcast", "youtube_channel", "rss_feed"},
		"description": "Only when the URL cannot say it on its own: a podcast RSS feed and an " +
			"article feed look identical, so pass rss_feed to archive a feed's articles rather " +
			"than treat it as a podcast. Leave empty to infer (a plain feed URL is a podcast).",
	},
	"work_id": map[string]any{
		"type": "string",
		"description": "An existing series or podcast work, from search_content. Give this " +
			"or title, never both.",
	},
	"title": map[string]any{
		"type": "string",
		"description": "The series or podcast title, for a work the library has never seen. " +
			"The work is created from it the same way want_content does, so a follow and a " +
			"later scan converge on one work.",
	},
	"year": map[string]any{
		"type":        "integer",
		"description": "Part of the work's identity when known.",
	},
	"quality_profile": map[string]any{
		"type": "string",
		"description": "The standard every episode this source archives is measured against, " +
			"named as a person would: \"living-room\". Required — every projected want inherits it.",
	},
	"monitor": map[string]any{
		"type": "boolean",
		"description": "Keep looking for a better copy of each episode after it is satisfied. " +
			"Defaults to true.",
	},
	"backfill": map[string]any{
		"type": "string",
		"enum": []any{"from_now", "full"},
		"description": "How much back-catalogue to pull on the first poll. from_now (the " +
			"default) archives only episodes that air after you follow; full walks the whole " +
			"back-catalogue into wants — a real capacity commitment.",
	},
	"reason": map[string]any{
		"type":        "string",
		"description": "A note for whoever reads this in six months — \"Kate watches this\". Never interpreted.",
	},
}, "quality_profile")

// schemaUnfollow stops a subscription. keep_archive defaults to true — stop
// polling, keep what was archived.
var schemaUnfollow = obj(map[string]any{
	"source_id": map[string]any{
		"type":        "string",
		"description": "The followed source to stop, from list_followed.",
	},
	"keep_archive": map[string]any{
		"type": "boolean",
		"description": "Keep the episodes already archived (the default, true). Phase 1 " +
			"always keeps the archive, so false is refused.",
	},
}, "source_id")

var schemaDesiredItemID = obj(map[string]any{
	"desired_item_id": map[string]any{
		"type":        "string",
		"description": "The want to explain.",
	},
}, "desired_item_id")

var schemaBlobHash = obj(map[string]any{
	"blob_hash": map[string]any{
		"type":        "string",
		"description": "The canonical blob digest, `blake3:` followed by 64 hex characters.",
	},
}, "blob_hash")

var schemaSyncPeer = obj(map[string]any{
	"peer": map[string]any{
		"type": "string",
		"description": "The peer to reconcile against, by id or by name. It must be a " +
			"peer other than this node: a node does not synchronise with itself.",
	},
}, "peer")

var schemaExplainRelease = obj(map[string]any{
	"quality_profile": map[string]any{
		"type":        "string",
		"description": "The profile to score against, by name.",
	},
	"releases": map[string]any{
		"type":     "array",
		"minItems": 1,
		"maxItems": maxRows,
		"items": obj(map[string]any{
			"id": map[string]any{
				"type": "string",
				"description": "Your identifier for this release. Used to break ties, so " +
					"supplying one makes the ranking independent of the order you sent them in.",
			},
			"title": map[string]any{
				"type": "string",
				"description": "What the release is called. Never parsed — put what you " +
					"know into attributes instead.",
			},
			"attributes": map[string]any{
				"type": "object",
				"description": "What you know about the release. LEAVE A KEY OUT when you " +
					"cannot tell: an absent attribute is reported as `undetermined`, which " +
					"is a different answer from a wrong one and sends a person somewhere " +
					"different. Guessing a value produces a confident wrong verdict.",
				"properties": map[string]any{
					"resolution": map[string]any{
						"type": "integer",
						"description": "Vertical lines: 480, 720, 1080, 2160. Not a label — " +
							"\"4K\", \"2160p\" and \"UHD\" are three spellings of one number.",
					},
					"source": map[string]any{
						"type":        "string",
						"description": "remux, bluray, web-dl, webrip, hdtv, dvd, cam.",
					},
					"video_codec":    map[string]any{"type": "string"},
					"audio_codec":    map[string]any{"type": "string"},
					"audio_channels": map[string]any{"type": "integer"},
					"hdr":            map[string]any{"type": "boolean"},
					"size_bytes":     map[string]any{"type": "integer"},
					"language":       map[string]any{"type": "string"},
				},
				"additionalProperties": false,
			},
		}),
	},
}, "quality_profile", "releases")

// schemaSearchReleases is search_releases' input (§71).
var schemaSearchReleases = obj(map[string]any{
	"desired_item_id": map[string]any{
		"type":        "string",
		"description": "The want to look for releases of.",
	},
}, "desired_item_id")

// schemaAcquireRelease is acquire_release's input (§71).
//
// Both arguments are required. A candidate id with no want is ambiguous — the
// same release may be a candidate for several wants — and a want with no
// candidate would make this "acquire something", which is what the scorer is
// for.
var schemaAcquireRelease = obj(map[string]any{
	"desired_item_id": map[string]any{
		"type":        "string",
		"description": "The want the release should satisfy.",
	},
	"candidate_id": map[string]any{
		"type": "string",
		"description": "The candidate to acquire, from get_content_satisfaction or a " +
			"prior search. It must be one this want's last search returned.",
	},
}, "desired_item_id", "candidate_id")
