package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The enrich_work handler (ADR-0087, ADR-0088, ADR-0122): fill in a held Work's
// canonical ids, description and artwork from an enrich provider — MusicBrainz
// for an album, Open Library for a book, TMDB for a film or series — and, when
// the match is confident and the provider offers one, correct the noisy display
// identity a filename produced.
//
// # Why an enrich never "fails" for a provider reason
//
// A Work no authority knows — a self-published book Open Library has never seen —
// is the ordinary case, not an error (the follow poll's stance, ADR-0057). And a
// provider that is unreachable is a condition a later pass fixes, not an immediate
// queue retry. So the only failures this handler returns — the transient kind a
// retry is for — are the store and the database; a provider miss or error backs
// the Work off on the enrich schedule and returns nil. The bookkeeping the beat
// reads (enrich_schedule) is what turns "not found" into "ask again later".
//
// # It fetches only what the Work is missing
//
// The handling-time context says which artwork faces the Work already resolves
// (a shipped poster, a cover lifted from the book file, an image an earlier pass
// fetched). A face that is present is not fetched again: a re-run (invariant 9)
// downloads nothing the node already holds, and a fetched image never displaces
// one the release shipped — the resolver prefers the shipped one (ADR-0105), and
// not fetching is cheaper than fetching and losing the rank. A poster and a
// backdrop are two faces, filed under two names, and one is never attached in
// the other's place: a film TMDB has no backdrop for gets a poster and nothing
// else, not a poster twice.

// enrichCorrectThreshold is how confident a match must be before its canonical
// title+author REPLACE the Work's display identity (ADR-0088 §3). Conservative:
// below it the Work still gains whatever ids/cover the match justified, but keeps
// its ingest identity — enrichment either improves a Work or leaves it as
// ingested, never degrades it. 0.75 means roughly three of every four query words
// are present in the canonical record.
const enrichCorrectThreshold = 0.75

// enrich backoff: a Work not fully enriched is asked about again on a widening
// cadence, capped, never abandoned (ADR-0057). Twelve hours is soon enough to pick
// up a cover added shortly after; two weeks is the ceiling for a Work no authority
// has.
const (
	enrichBase = 12 * time.Hour
	enrichCap  = 14 * 24 * time.Hour
)

func enrichDelay(fruitless int) time.Duration {
	d := enrichBase
	for range fruitless {
		d *= 2
		if d >= enrichCap {
			return enrichCap
		}
	}
	return d
}

// EnrichRecorder is the catalog surface the enrich handler needs. An interface so
// the handler's routing and write logic are testable without a database.
type EnrichRecorder interface {
	EnrichContext(ctx context.Context, workID string) (catalog.DueEnrichWork, bool, error)
	WriteWorkExternalID(ctx context.Context, workID, source, value string) error
	RecordWorkMetadata(ctx context.Context, workID string, meta catalog.EnrichedMetadata, now time.Time) error
	RecordFetchedArtwork(ctx context.Context, workID string, art catalog.FetchedArtwork, now time.Time) error
	CorrectWorkIdentity(ctx context.Context, workID, canonicalTitle, canonicalAuthor string, now time.Time) error
	RecordEnrichScheduled(ctx context.Context, workID string, fruitless int, now, next time.Time) error
}

// ArtworkBlobStore adopts fetched cover bytes into the CAS, hashing as it writes.
type ArtworkBlobStore interface {
	Put(ctx context.Context, r io.Reader) (hash string, size int64, err error)
}

// ArtworkFetcher fetches a cover image and reports the image type the response
// carried, so the asset records a truthful mime. The URL is a secret.Value (a
// provider may hand back a tokened link) revealed only where it goes on the wire.
type ArtworkFetcher interface {
	Fetch(ctx context.Context, url secret.Value) (body io.ReadCloser, mime string, err error)
}

// EnrichHandlerOptions configure the enrich_work handler.
type EnrichHandlerOptions struct {
	// Providers is the routed set of enrich providers. The handler routes a Work to
	// the first provider that serves its content type, so a node with both a music
	// and a book provider handles either.
	Providers []providers.EnrichProvider
	Recorder  EnrichRecorder
	Store     ArtworkBlobStore
	Fetcher   ArtworkFetcher
	Now       func() time.Time
	Logger    *slog.Logger
}

// EnrichHandler runs one enrich_work job.
func EnrichHandler(opts EnrichHandlerOptions) HandlerFunc {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	return func(ctx context.Context, job jobs.Job) error {
		payload, err := decodePayload[acquisition.EnrichWorkPayload](job)
		if err != nil {
			return err
		}
		if payload.WorkID == "" {
			return fmt.Errorf("enrich-work: the payload names no work")
		}
		workID := payload.WorkID

		wctx, ok, err := opts.Recorder.EnrichContext(ctx, workID)
		if err != nil {
			return err // a database read failing is the transient kind a retry is for.
		}
		if !ok {
			// No longer eligible — the Work went away or is not a Work enrichment
			// covers.
			return nil
		}

		backOff := func(reason string) error {
			next := now().Add(enrichDelay(wctx.Fruitless + 1))
			if err := opts.Recorder.RecordEnrichScheduled(ctx, workID, wctx.Fruitless+1, now(), next); err != nil {
				return err
			}
			log.Info("work not fully enriched, backing off",
				"work_id", workID, "reason", reason, "next_enrich_at", next)
			return nil
		}

		provider := opts.providerFor(wctx.ContentType)
		if provider == nil {
			// This node has an enrich provider (else the job would not be
			// registered) but none for this Work's type — a music-only node handed a
			// book. Back off; another peer may serve it.
			return backOff("no provider for content type")
		}

		res, found, err := provider.Enrich(ctx, providers.EnrichQuery{
			ContentType: wctx.ContentType,
			Title:       wctx.Title,
			Album:       wctx.Title,
			Artist:      wctx.Artist,
			Author:      wctx.Author,
			Year:        wctx.Year,
			ExternalIDs: wctx.ExternalIDs,
		})
		if err != nil {
			log.Warn("an enrich lookup failed", "work_id", workID, "provider", provider.Name(), "error", err)
			return backOff("provider error")
		}
		if !found {
			return backOff("no match")
		}

		// Ids first — they are the identity the cover and any correction hang off,
		// and the first production writer of external_ids (ADR-0050).
		for source, value := range res.ExternalIDs {
			if err := opts.Recorder.WriteWorkExternalID(ctx, workID, source, value); err != nil {
				return fmt.Errorf("enrich-work: recording %s id: %w", source, err) // db failure: retry.
			}
		}

		// A confident match corrects the display identity (ADR-0088). Below the
		// threshold the Work keeps its ingest identity and only gains ids/cover.
		if res.Confidence >= enrichCorrectThreshold && (res.Title != "" || res.Author != "") {
			if err := opts.Recorder.CorrectWorkIdentity(ctx, workID, res.Title, res.Author, now()); err != nil {
				return fmt.Errorf("enrich-work: correcting identity: %w", err) // db failure: retry.
			}
			log.Info("corrected work identity",
				"work_id", workID, "title", res.Title, "author", res.Author, "confidence", res.Confidence)
		}

		// What the authority said about the Work beyond its ids: the overview a
		// detail screen shows, the genres, its own year as provenance. Additive
		// (ADR-0087 §4) and idempotent, so it is written before the artwork
		// fetches that might not land this pass.
		if res.Overview != "" || len(res.Genres) > 0 || res.Year != 0 {
			if err := opts.Recorder.RecordWorkMetadata(ctx, workID, catalog.EnrichedMetadata{
				Source: enrichSource(wctx.ContentType), Overview: res.Overview, Genres: res.Genres, Year: res.Year,
			}, now()); err != nil {
				return fmt.Errorf("enrich-work: recording metadata: %w", err) // db failure: retry.
			}
		}

		// Artwork, one face at a time, and only the faces the Work is missing.
		hasPoster := wctx.HasPoster
		if !hasPoster && res.CoverURL.Reveal() != "" {
			if err := opts.attachArtwork(ctx, workID, wctx.ContentType, res.CoverURL, catalog.ArtworkPoster, now()); err != nil {
				// An image that would not fetch or adopt is not a job failure — the
				// id was recorded, and the next pass retries the image.
				log.Warn("could not attach a poster", "work_id", workID, "error", err)
			} else {
				hasPoster = true
			}
		}
		// A backdrop is owed only by a film or series, and only when the
		// authority has one: a Work whose provider offers no backdrop is as done
		// as it can be, and is not held in the fruitless streak for it.
		backdropOffered := res.BackdropURL.Reveal() != ""
		hasBackdrop := wctx.HasBackdrop || !wantsBackdrop(wctx.ContentType)
		if !hasBackdrop && backdropOffered {
			if err := opts.attachArtwork(ctx, workID, wctx.ContentType, res.BackdropURL, catalog.ArtworkBackdrop, now()); err != nil {
				log.Warn("could not attach a backdrop", "work_id", workID, "error", err)
			} else {
				hasBackdrop = true
			}
		}

		if hasPoster && (hasBackdrop || !backdropOffered) {
			// Fully enriched for now: reset the streak, schedule far enough out that
			// the beat does not re-ask before there is a reason to.
			if err := opts.Recorder.RecordEnrichScheduled(ctx, workID, 0, now(), now().Add(enrichCap)); err != nil {
				return err
			}
			log.Info("enriched a work", "work_id", workID, "provider", provider.Name(),
				"content_type", wctx.ContentType, "ids", len(res.ExternalIDs), "confidence", res.Confidence,
				"poster", hasPoster, "backdrop", hasBackdrop)
			return nil
		}
		// Ids recorded but a face did not land — the Work stays in the due set, so
		// back off progressively rather than re-asking every tick.
		if !hasPoster {
			return backOff("no cover")
		}
		return backOff("no backdrop")
	}
}

// wantsBackdrop reports whether a content type has a backdrop face at all. A
// film or series does; a book or album has one image, its cover.
func wantsBackdrop(contentType string) bool {
	return contentType == "movie" || contentType == "series"
}

// providerFor returns the first enrich provider that serves the content type, or
// nil when this node has none for it.
func (o EnrichHandlerOptions) providerFor(contentType string) providers.EnrichProvider {
	for _, p := range o.Providers {
		if p.ServesContentType(contentType) {
			return p
		}
	}
	return nil
}

// attachArtwork fetches one image, adopts its bytes into the CAS and records the
// role='artwork' asset under the given kind, which decides the filename the
// resolver ranks on. The store error is returned (transient, retryable); a fetch
// error is returned too and the caller treats it as "not this pass".
func (o EnrichHandlerOptions) attachArtwork(
	ctx context.Context, workID, contentType string, imageURL secret.Value, kind string, now time.Time,
) error {
	body, mime, err := o.Fetcher.Fetch(ctx, imageURL)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	hash, size, err := o.Store.Put(ctx, body)
	if err != nil {
		return err
	}
	return o.Recorder.RecordFetchedArtwork(ctx, workID, catalog.FetchedArtwork{
		BlobHash: hash, Size: size, MIME: mime, Source: coverSource(contentType), Kind: kind,
	}, now)
}

// coverSource names where an image came from, for the asset's attributes. A
// book's cover is Open Library's; a music cover is the Cover Art Archive's; a
// film's or series' poster and backdrop are TMDB's.
func coverSource(contentType string) string {
	switch contentType {
	case "music":
		return "coverartarchive"
	case "movie", "series":
		return "tmdb"
	}
	return "openlibrary"
}

// enrichSource names the authority that described a Work, for
// attributes.enrich_source — the same source the due predicate keys the Work's
// external id on (catalog enrichSourceCase), so the two never disagree about
// who identified what.
func enrichSource(contentType string) string {
	switch contentType {
	case "music":
		return "musicbrainz"
	case "movie", "series":
		return "tmdb"
	}
	return "openlibrary"
}
