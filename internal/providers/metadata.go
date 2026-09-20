package providers

import (
	"context"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
)

// FeedProvider enumerates the items a followed source emits (M12).
//
// # This is the metadata provider capability.go reserved
//
// CapabilityMetadata has existed since Milestone 3 "with nothing implementing
// it" precisely so that the first metadata provider would be an addition to
// configuration rather than a change to the registry (see capability.go). This
// is the contract that capability finally routes to: a FeedProvider advertises
// CapabilityMetadata, and a followed source's poll loop asks it "what items does
// this source have now?".
//
// # Values in, values out — the same line the rest of this package draws
//
// A ref in (the source-stable handle a FollowedSource carries — a TVDB series
// id, a podcast feed URL, a channel id; opaque to the caller, parsed by the
// adapter), a slice of followed.FeedItem out. No transport, no credential and no
// pagination cross this line, exactly as none crosses Indexer or Downloader: a
// caller must not be able to tell a live TVDB adapter from a replayed fixture,
// because a metadata service is as un-reproducible in CI as an indexer is
// (ADR-0026) and fixtures are the only test it will ever have.
//
// # Deliberately source-agnostic and provider-agnostic
//
// Enumerate returns the neutral followed.FeedItem, never a TVDB- or
// TMDB-shaped value, so the source's Type — not the choice of metadata service
// — is what a projection reads, and a second implementation (TMDB) slots in
// behind this interface without the poll loop changing. The caller expresses
// which source; the registry routes to whichever CapabilityMetadata provider
// answers for it.
type FeedProvider interface {
	Provider

	// Enumerate returns the items the source identified by ref currently has,
	// as neutral FeedItems. It is the feed adapter's whole job: for a TV series,
	// "which episodes should exist and when did/will they air"; for a podcast,
	// the entries; for a channel, the videos.
	//
	// It returns an error rather than a Health because, unlike Check, a failed
	// enumeration is a failure of the call the poll loop must see and retry —
	// the loop decides the hold-off, and folding "unreachable" into an empty
	// slice would silently report a source as having emitted nothing.
	Enumerate(ctx context.Context, ref string) ([]followed.FeedItem, error)

	// ServesType reports whether this adapter is the one that enumerates a source
	// of the given followed type. It is how the poll routes a source to the
	// RIGHT feed adapter rather than to whichever happens to be first: a TVDB
	// adapter serves tv_series, a podcast adapter podcast, a youtube adapter
	// youtube_channel, a webfeed adapter rss_feed. With one metadata provider
	// configured this is trivially true for that provider's own type; it earns
	// its place the moment a deployment configures two, where routing a podcast
	// poll to the TVDB adapter would enumerate nothing and look like a dead feed.
	// Declared on the interface, not inferred from the provider Kind, because the
	// registry holds providers as this interface and the mapping belongs with the
	// adapter that knows its own source shape.
	ServesType(t followed.Type) bool
}

// DiscoveryCandidate is one work a discovery search resolved from the metadata
// service — a work that may NOT be in the library yet (#451).
//
// It is the counterpart to Enumerate's followed.FeedItem: a neutral,
// provider-agnostic value so a TMDB implementation later returns the SAME shape
// a TVDB one does, and the caller never couples to either service's JSON. Where
// FeedItem is one item WITHIN a source (an episode), a candidate is the source
// itself — a series a caller could go on to follow, OR (ADR-0077's deferred
// half, now built) a movie/book/music work a caller could go on to WANT.
//
// # Type is a content-kind string, not a followed.Type, on purpose
//
// ADR-0077 originally typed this field as followed.Type, which structurally
// could only ever name the four feed-shaped kinds (tv_series, podcast,
// youtube_channel, rss_feed) — there was nowhere to put a movie, a book or an
// album, because none of those is a subscription with a calendar. That ADR
// named the fix: "a non-feed, want-scoped discovery candidate". This is it —
// Type is now any works.content_type value too (movie, book, music), and the
// caller (resources.Discover / the MCP door) branches on it to decide whether
// a candidate is actioned via follow_source (the four feed-shaped kinds) or
// want_content by title (movie, book, music — a one-off, not a subscription).
type DiscoveryCandidate struct {
	// Title is the work's name as the metadata service knows it.
	Title string
	// Year is the first-aired/release/publish year, zero when the service did
	// not give one — a real and distinct answer from any year.
	Year int
	// ExternalID is the source-native identity: a TVDB/TMDB series id (follow_source
	// takes it as tvdb_id), or a provider-native id for a want-scoped kind (a TMDB
	// movie id, an Open Library OLID, a MusicBrainz MBID). It is the whole point of
	// discovery — a free-text query resolved to an id the caller can act on, or at
	// least display, in one step, rather than a title that might create a second work.
	ExternalID string
	// Source names which metadata service produced this candidate ("tvdb", "tmdb",
	// "openlibrary", "musicbrainz"), so a caller can tell two providers' ids apart
	// even when their ExternalID strings could otherwise collide, and can show
	// where the candidate came from. Never empty — every provider sets its own name.
	Source string
	// Type is the content kind this candidate is: one of the four followed.Type
	// values (followed via follow_source) or a works content type — movie, book,
	// music (wanted via want_content by title, never followed — see above).
	Type string
	// Overview is a short human description, when the service supplies one, so a
	// person choosing between two same-named series has something to choose on.
	// Empty is fine — it is decoration, never an identity.
	Overview string
}

// DiscoverySearcher is an OPTIONAL capability a metadata provider may also
// satisfy: resolving a free-text query into candidate works, INCLUDING ones the
// library does not hold (#451).
//
// # Why it is a separate interface, not a method on FeedProvider
//
// Enumerate answers "which items does this KNOWN source have"; Discover answers
// "which sources match this text at all". They are different questions, and not
// every FeedProvider can answer the second: a podcast or RSS adapter is handed a
// feed URL and has nothing to search, while a webfeed adapter has no catalogue
// behind it. Folding Discover into FeedProvider would force those adapters to
// carry a method that could only refuse. So it is an extra interface a provider
// declares by implementing, and the registry surfaces the ones that do — the
// same "optional capability by type assertion" shape Indexer and Downloader use
// beside the base Provider.
//
// A provider that implements this still advertises CapabilityMetadata; discovery
// is a facet of being a metadata provider, not a fifth capability, so routing and
// health stay unchanged and a node's "what can search for new content" answer is
// "the metadata providers that also know how to look themselves up".
type DiscoverySearcher interface {
	Provider

	// Discover resolves a free-text query to candidate works. An empty result is
	// a modelled outcome — the query matched nothing — not an error; an error is
	// reserved for a call that could not be made (the service was unreachable, or
	// the credential was rejected), which the caller must see rather than read as
	// "nothing matched".
	Discover(ctx context.Context, query string) ([]DiscoveryCandidate, error)
}
