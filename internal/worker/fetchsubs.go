package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The fetch_subtitle handler (ADR-0085): fetch a subtitle-aspect want's subtitle
// from a provider and attach it, so the reconcile sweep marks the want satisfied.
//
// # Why a fetch never "fails" for a provider reason
//
// A subtitle that does not exist yet — an episode that just aired — is the
// ordinary case, not an error (the follow poll's stance, ADR-0057). And a
// provider that is unreachable or has spent its daily quota is a condition that a
// LATER fetch fixes, not an immediate queue retry that would hammer it. So the
// only failures this handler returns — the transient kind a queue retry is for —
// are the store and the database; a provider miss or a provider error backs the
// want off on the fetch schedule and returns nil. The bookkeeping the beat reads
// (subtitle_fetch_schedule) is what turns "not found" into "ask again later"
// rather than "ask again in thirty seconds".

// FetchSubsRecorder is the catalog surface the fetch handler needs. An interface
// so the handler's selection and attach logic are testable without a database.
type FetchSubsRecorder interface {
	SubtitleFetchContext(ctx context.Context, desiredItemID string) (catalog.SubtitleFetchContext, bool, error)
	RecordFetchedSubtitle(ctx context.Context, sourceAssetID string, sub catalog.FetchedSubtitle, now time.Time) error
	RecordSubtitleFetchScheduled(ctx context.Context, desiredItemID string, fruitless int, now, next time.Time) error
}

// SubtitleBlobStore adopts the downloaded subtitle bytes into the CAS, hashing
// as it writes (cas.Store.Put). An interface so the handler is testable without a
// real store.
type SubtitleBlobStore interface {
	Put(ctx context.Context, r io.Reader) (hash string, size int64, err error)
}

// SubtitleDownloader fetches the bytes at a resolved subtitle link. A subtitle is
// a few KB, so this is a plain bounded GET, not the transfer-tracking download
// client a torrent needs. The URL is a secret.Value — a per-fetch token — so it
// is revealed only here where it goes on the wire.
type SubtitleDownloader interface {
	Fetch(ctx context.Context, url secret.Value) (io.ReadCloser, error)
}

// FetchSubsHandlerOptions configure the fetch_subtitle handler.
type FetchSubsHandlerOptions struct {
	// Providers is the routed set of subtitle providers. The handler asks each in
	// turn until one returns a candidate, so a deployment with a fallback
	// provider degrades to it (ADR-0025); with one provider it is a set of one.
	Providers  []providers.SubtitleProvider
	Recorder   FetchSubsRecorder
	Store      SubtitleBlobStore
	Downloader SubtitleDownloader
	Now        func() time.Time
	Logger     *slog.Logger
}

// fetch backoff: a subtitle that is not there yet is asked for again on a widening
// cadence, capped, never abandoned — the follow poll's shape (ADR-0057). Six
// hours is soon enough to catch a subtitle uploaded shortly after an episode
// airs; a week is the ceiling for content nobody has subtitled.
const (
	subtitleFetchBase = 6 * time.Hour
	subtitleFetchCap  = 7 * 24 * time.Hour
)

func subtitleFetchDelay(fruitless int) time.Duration {
	d := subtitleFetchBase
	for range fruitless {
		d *= 2
		if d >= subtitleFetchCap {
			return subtitleFetchCap
		}
	}
	return d
}

// FetchSubsHandler runs one fetch_subtitle job.
func FetchSubsHandler(opts FetchSubsHandlerOptions) HandlerFunc {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	return func(ctx context.Context, job jobs.Job) error {
		var payload acquisition.FetchSubtitlePayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("fetch-subtitle: undecodable payload: %w", err)
		}
		if payload.DesiredItemID == "" {
			return fmt.Errorf("fetch-subtitle: the payload names no want")
		}
		wantID := payload.DesiredItemID

		fctx, ok, err := opts.Recorder.SubtitleFetchContext(ctx, wantID)
		if err != nil {
			return err // a database read failing is the transient kind a retry is for.
		}
		if !ok {
			// No longer eligible — the want went away, is not a subtitle want, or
			// its source video is gone. Nothing to do, and not an error.
			return nil
		}

		// Back off on any outcome that did not attach a subtitle, so the beat asks
		// again later rather than every tick. Overwritten below on success.
		backOff := func(reason string) error {
			next := now().Add(subtitleFetchDelay(fctx.Fruitless + 1))
			if err := opts.Recorder.RecordSubtitleFetchScheduled(ctx, wantID, fctx.Fruitless+1, now(), next); err != nil {
				return err
			}
			log.Info("no subtitle fetched, backing off",
				"desired_item_id", wantID, "reason", reason, "next_fetch_at", next)
			return nil
		}

		query := providers.SubtitleQuery{
			IMDBID:    fctx.IMDBID,
			TMDBID:    fctx.TMDBID,
			Season:    fctx.Season,
			Episode:   fctx.Episode,
			Languages: []string{fctx.Language},
		}

		cand, provider, found := opts.findBest(ctx, query, fctx.Language, log)
		if !found {
			return backOff("no candidate")
		}

		link, err := provider.ResolveSubtitle(ctx, cand.FileID)
		if err != nil {
			// The provider was reached but the download-link mint failed —
			// commonly a spent daily quota (ADR-0085). Back off, do not fail.
			log.Warn("resolving a subtitle link failed",
				"desired_item_id", wantID, "provider", provider.Name(), "error", err)
			return backOff("resolve failed")
		}

		body, err := opts.Downloader.Fetch(ctx, link.URL)
		if err != nil {
			log.Warn("downloading a subtitle failed",
				"desired_item_id", wantID, "provider", provider.Name(), "error", err)
			return backOff("download failed")
		}
		defer func() { _ = body.Close() }()

		hash, size, err := opts.Store.Put(ctx, body)
		if err != nil {
			return fmt.Errorf("fetch-subtitle: adopting the bytes: %w", err) // store failure: retry.
		}

		if err := opts.Recorder.RecordFetchedSubtitle(ctx, fctx.SourceVideoAssetID,
			catalog.FetchedSubtitle{BlobHash: hash, Size: size, Language: fctx.Language},
			now()); err != nil {
			return fmt.Errorf("fetch-subtitle: recording the subtitle: %w", err) // db failure: retry.
		}

		// Attached. The reconcile sweep will mark the want satisfied and it will
		// leave the due set; the schedule reset (fruitless 0) is bookkeeping for
		// the window until then, and a next far enough out that the beat does not
		// re-ask before reconcile catches up.
		if err := opts.Recorder.RecordSubtitleFetchScheduled(ctx, wantID, 0,
			now(), now().Add(subtitleFetchBase)); err != nil {
			return err
		}
		log.Info("fetched a subtitle",
			"desired_item_id", wantID, "provider", provider.Name(),
			"language", fctx.Language, "blob", hash, "remaining", link.Remaining)
		return nil
	}
}

// findBest asks each provider in turn and returns the best candidate the first
// one to answer offers, ranked for a caption worth serving: the wanted language,
// a non-hearing-impaired file over an SDH one, then the most-downloaded (a rough
// trust signal). A provider error is logged and the next is tried; an empty
// result moves to the next too.
func (o FetchSubsHandlerOptions) findBest(
	ctx context.Context, query providers.SubtitleQuery, language string, log *slog.Logger,
) (providers.SubtitleCandidate, providers.SubtitleProvider, bool) {
	lang := strings.ToLower(strings.TrimSpace(language))
	for _, p := range o.Providers {
		cands, err := p.FindSubtitle(ctx, query)
		if err != nil {
			log.Warn("a subtitle search failed", "provider", p.Name(), "error", err)
			continue
		}
		best, ok := bestCandidate(cands, lang)
		if ok {
			return best, p, true
		}
	}
	return providers.SubtitleCandidate{}, nil, false
}

// bestCandidate ranks the candidates that match the wanted language.
func bestCandidate(cands []providers.SubtitleCandidate, lang string) (providers.SubtitleCandidate, bool) {
	matching := make([]providers.SubtitleCandidate, 0, len(cands))
	for _, c := range cands {
		if c.FileID == "" {
			continue
		}
		// An empty candidate language is admitted — the query already asked for
		// the language, so a provider that does not echo it back is trusted; a
		// mismatched non-empty language is not.
		if cl := strings.ToLower(strings.TrimSpace(c.Language)); cl != "" && cl != lang {
			continue
		}
		matching = append(matching, c)
	}
	if len(matching) == 0 {
		return providers.SubtitleCandidate{}, false
	}
	sort.SliceStable(matching, func(i, j int) bool {
		// Prefer a clean subtitle over a hearing-impaired one.
		if matching[i].HearingImpaired != matching[j].HearingImpaired {
			return !matching[i].HearingImpaired
		}
		// Then the most downloaded.
		return matching[i].DownloadCount > matching[j].DownloadCount
	})
	return matching[0], true
}
