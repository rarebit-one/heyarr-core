package worker

import (
	"context"
	"encoding/json"
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

// The enrich_work handler (ADR-0087, ADR-0088): fill in a held music/book Work's
// canonical ids and cover from an enrich provider, and — when the match is
// confident — correct the noisy display identity a filename produced.
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
		var payload acquisition.EnrichWorkPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("enrich-work: undecodable payload: %w", err)
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
			// No longer eligible — the Work went away or is not a music/book Work.
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

		coverAttached := false
		if url := res.CoverURL.Reveal(); url != "" {
			if err := opts.attachCover(ctx, workID, wctx.ContentType, res.CoverURL, now(), log); err != nil {
				// A cover that would not fetch or adopt is not a job failure — the id
				// was recorded, and the next pass retries the image. Only a store
				// error (below) is transient enough to retry now; a fetch miss backs
				// off with everything else.
				log.Warn("could not attach a cover", "work_id", workID, "error", err)
			} else {
				coverAttached = true
			}
		}

		if coverAttached {
			// Fully enriched for now: reset the streak, schedule far enough out that
			// the beat does not re-ask before there is a reason to.
			if err := opts.Recorder.RecordEnrichScheduled(ctx, workID, 0, now(), now().Add(enrichCap)); err != nil {
				return err
			}
			log.Info("enriched a work", "work_id", workID, "provider", provider.Name(),
				"ids", len(res.ExternalIDs), "confidence", res.Confidence)
			return nil
		}
		// Ids recorded but no cover landed — the Work stays in the due set (it has no
		// artwork), so back off progressively rather than re-asking every tick.
		return backOff("no cover")
	}
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

// attachCover fetches the cover, adopts its bytes into the CAS and records the
// role='artwork' asset. The store error is returned (transient, retryable); a
// fetch error is returned too and the caller treats it as "no cover this pass".
func (o EnrichHandlerOptions) attachCover(
	ctx context.Context, workID, contentType string, coverURL secret.Value, now time.Time, log *slog.Logger,
) error {
	body, mime, err := o.Fetcher.Fetch(ctx, coverURL)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	hash, size, err := o.Store.Put(ctx, body)
	if err != nil {
		return err
	}
	return o.Recorder.RecordFetchedArtwork(ctx, workID, catalog.FetchedArtwork{
		BlobHash: hash, Size: size, MIME: mime, Source: coverSource(contentType),
	}, now)
}

// coverSource names where a cover came from, for the asset's attributes. A book's
// cover is Open Library's; a music cover is the Cover Art Archive's.
func coverSource(contentType string) string {
	if contentType == "music" {
		return "coverartarchive"
	}
	return "openlibrary"
}
