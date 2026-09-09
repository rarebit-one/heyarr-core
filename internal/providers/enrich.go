package providers

import (
	"context"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
)

// EnrichProvider fills in what a Work already held in the library IS — its
// canonical ids and its cover — from a keyed lookup (ADR-0087, M12 Phase 6).
//
// # This is the capability CapabilityEnrich routes to
//
// It is the sibling of SubtitleProvider and FeedProvider — an optional
// capability a provider declares by implementing, discovered by type assertion
// beside the base Provider — and it exists because a held album or book has an
// identity that was guessed from its filename at ingest and nothing has ever
// filled in from an authority. An enrich provider answers a question no metadata
// or subtitle provider does: "given a Work — its content type and the identity
// attributes ingest parsed (artist+album, author+title) — what is its canonical
// id, and where is its cover."
//
// # Keyed on the Work's CONTENT type, not a followed type
//
// A metadata provider (TVDB, TMDB) is routed by ServesType(followed.Type) for
// the poll loop; an enrich provider routes by a held Work's content type
// ("music" | "book"), because enrichment has no subscription — the content is
// already here, only its description is being filled in. ServesContentType is
// how the worker picks the adapter for a Work without learning which service it
// is, exactly as FeedProvider.ServesType does for the poll loop.
//
// # Values in, values out — the same line the rest of this package draws
//
// An EnrichQuery in (the content type and the identity attributes the caller
// already holds about the Work), an EnrichResult out. No transport, no
// credential and no pagination cross this line, exactly as none crosses Indexer
// or SubtitleProvider: a caller must not be able to tell a live MusicBrainz or
// Open Library adapter from a replayed fixture, because an enrich service is as
// un-reproducible in CI as an indexer is (ADR-0026) and fixtures are the only
// test it will ever have.
type EnrichProvider interface {
	Provider

	// ServesContentType reports whether this adapter can enrich a Work of the
	// given content type ("music" | "book"). The worker routes a due Work to the
	// first enrich provider that serves its type, so a MusicBrainz adapter says
	// music and an Open Library adapter says book, and neither is handed the
	// other's Works.
	ServesContentType(ct string) bool

	// Enrich looks a Work up and returns what the authority knows about it. The
	// bool reports whether anything matched: false with a nil error is the
	// modelled "nothing matched" outcome — a Work the provider's authority does
	// not know — which the caller backs off on rather than treating as an error.
	// An error is reserved for a call that could not be made (the service was
	// unreachable), which the caller must see rather than read as "this Work is
	// unknown".
	Enrich(ctx context.Context, q EnrichQuery) (EnrichResult, bool, error)
}

// EnrichQuery is what to look a held Work up by (ADR-0087).
//
// A VALUE with no transport in it. It carries what heyarr already knows about a
// held Work — its content type, the title ingest identified, and the identity
// attributes ingest parsed (artist+album for music, author for books) — and any
// external ids already recorded. A provider uses whatever subset it supports;
// nothing here knows or cares which.
type EnrichQuery struct {
	// ContentType is the held Work's content type: "music" | "book". A provider
	// that does not serve it returns the modelled empty result.
	ContentType string
	// Title is the Work's title as ingest identified it — the album title for
	// music, the book title for a book. For a book this is the noisy string a
	// filename produced (it may carry the author and provenance junk); the
	// adapter cleans it before searching (ADR-0088).
	Title string
	// Artist and Album narrow a music query. Artist is the Work's
	// attributes.artist; Album is the Work title, repeated here so a music
	// adapter reads one field rather than knowing the title IS the album.
	Artist string
	Album  string
	// Author narrows a book query — the Work's attributes.author. For the shelf
	// layout that produced the library's books this is often a shelf name rather
	// than a person (ADR-0088), so an adapter treats it as a hint, not a filter.
	Author string
	// Year disambiguates, and is zero when ingest did not parse one. Zero means
	// "do not constrain" rather than "the year 0".
	Year int
	// ExternalIDs is any id already recorded for the Work (source → value), so a
	// provider that can look up by a strong key uses it before falling back to a
	// title search. Empty when nothing has been recorded yet.
	ExternalIDs map[string]string
}

// EnrichResult is what an authority knows about a Work (ADR-0087, extended by
// ADR-0088).
//
// The provider's answer is ADDITIVE by default (ADR-0087 §4): ids, a cover, a
// description. The canonical Title/Author (ADR-0088 §2) are the display identity
// the authority reports; the worker adopts them only when Confidence clears its
// threshold, so a weak match decorates a Work but never renames it. Values out,
// no transport: the CoverURL is a secret.Value because a provider may return a
// tokened link, and a fixture must be indistinguishable from the live service.
type EnrichResult struct {
	// ExternalIDs is the canonical ids the authority holds for the Work, keyed by
	// source ("musicbrainz" | "openlibrary" | …). The worker writes each to the
	// external_ids table (ADR-0050) as the Work's first authoritative identity.
	ExternalIDs map[string]string
	// CoverURL is where the Work's cover image can be fetched, empty when the
	// authority has none. A secret.Value so a tokened link stays out of logs and
	// the fixture corpus; Reveal() it only where it is put on the wire.
	CoverURL secret.Value
	// Overview is a short description the authority holds, empty when none. Stored
	// under the Work's attributes; never an identity.
	Overview string
	// Title and Author are the CANONICAL display identity the authority reports —
	// the release/book title and its artist/author. Empty means "no correction
	// offered". The worker adopts them (ADR-0088) only when Confidence clears
	// enrichCorrectThreshold; below it they are ignored and the Work keeps its
	// ingest identity.
	Title  string
	Author string
	// Confidence is the adapter's own match strength, 0..1 — how well what it
	// searched for matches what it returned (a normalised token-set similarity,
	// and/or the service's own score). It is how the provider owns "how sure am
	// I" and the worker owns "is that sure enough" (ADR-0088 §2); a top hit that
	// barely resembles the query scores low and is not adopted as identity.
	Confidence float64
}
