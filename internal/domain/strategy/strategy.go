// Package strategy maps a content type to how its wants are acquired (ADR-0082).
//
// The system knows five content types (ADR-0080) plus the work content types
// the follow path derives from a source type (`video` for a YouTube channel,
// `podcast` for a podcast feed). Acquisition used to treat them all alike: one
// pipeline, one video-shaped quality profile, an indexer search for everything.
// That judged a captured article by `resolution.gte`, and asked an indexer for
// bytes a feed had already handed over — the two failures ADR-0082 traces.
//
// A Strategy is the per-type answer to two questions the rest of the system
// asks: is this found by searching indexers or taken directly from the feed
// (Route), and — when a follow or want names no profile — which profile judges
// it (DefaultProfile). It is a pure lookup over a value table: no database, no
// config, nothing to tune at runtime, so "a document is never searched" is a
// table test and not a thing to watch a log for.
package strategy

import (
	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
)

// Work content types the follow path derives that are not identification
// content types (see resources.workContentType). Kept here as named constants
// so the table below reads in one vocabulary.
const (
	// video is a YouTube channel's work — searched like any other video.
	video = "video"
	// podcast is a podcast feed's work — taken directly from its enclosures.
	podcast = "podcast"
)

// Strategy is how one content type's wants are acquired.
type Strategy struct {
	// ContentType is the type this strategy governs, echoed back so a caller
	// that looked it up can log or compare it.
	ContentType string
	// Route is whether the bytes are searched for or taken directly (ADR-0082).
	Route acquisition.Route
	// DefaultProfile is the seeded quality-profile NAME a follow or want inherits
	// when it names none. Empty means the type has no profile that fits yet — its
	// satisfaction attributes do not exist in the policy vocabulary (music, book)
	// — and a caller must still name one. resolveProfile treats "" as "no
	// default", preserving its refusal exactly where the refusal is honest.
	DefaultProfile string
}

// table is the whole policy, as values. A type not listed falls to the zero
// strategy For hands back (Search, no default) — the safe answer for something
// the follow path invented that this table has not caught up with: search it
// like ordinary content and make the caller name a profile.
//
// # Only direct-route content has a default profile
//
// Search-route content — a film, a series, an album — has no default, so a want
// for it must still name a profile. That is not an omission: a video's quality
// bar is a real choice (living-room vs everyday vs archival), and §56's refusal
// of a want with no profile is the honest answer to "which one did you mean". A
// directly-taken document or podcast has no such choice — any copy the feed
// publishes is definitive — so there is one sensible default, `published`, and
// nothing for the caller to have meant instead.
var table = map[string]Strategy{
	identification.Movie:  {ContentType: identification.Movie, Route: acquisition.RouteSearch},
	identification.Series: {ContentType: identification.Series, Route: acquisition.RouteSearch},
	video:                 {ContentType: video, Route: acquisition.RouteSearch},
	identification.Music:  {ContentType: identification.Music, Route: acquisition.RouteSearch},
	identification.Book:   {ContentType: identification.Book, Route: acquisition.RouteSearch},
	// Document and podcast are taken directly from the feed and judged by
	// `published`, which carries no video gate and is terminal on any bytes: a
	// copy from the source that publishes it is definitive.
	identification.Document: {ContentType: identification.Document, Route: acquisition.RouteDirect, DefaultProfile: "published"},
	podcast:                 {ContentType: podcast, Route: acquisition.RouteDirect, DefaultProfile: "published"},
}

// For returns the acquisition strategy for a content type. An unknown type gets
// the conservative default — searched, with no assumed profile — rather than a
// panic or a zero Route: the follow path can mint a work content type this table
// has not seen, and the safe reading of one is "ordinary searchable content the
// caller must name a profile for", never "take it directly from a feed".
func For(contentType string) Strategy {
	if s, ok := table[contentType]; ok {
		return s
	}
	return Strategy{ContentType: contentType, Route: acquisition.RouteSearch}
}
