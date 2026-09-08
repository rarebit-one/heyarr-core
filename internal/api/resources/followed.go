package resources

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Followed sources (§55, M12) — the follow surface, source-agnostic by design.
//
// # Why there is no `source` / `provider` / `feed_type` parameter (#396)
//
// A caller says "follow this" with a content intent (which series or podcast)
// and an identity (a URL, or an explicit external id) — never which adapter to
// use. The system INFERS the type from the identity: a TVDB id or URL is a TV
// series, any other http(s) feed URL is a podcast, and the not-yet-implemented
// source types are refused with a message that says so rather than pretending.
// Picking the adapter is the registry's job, the same stance the provider layer
// takes for indexers; a `feed_type` knob would hand the caller a decision they
// should not have to make and could get wrong.
//
// # FOLLOW is a subscription; want_content stays the one-off
//
// follow_source establishes a STANDING subscription that archives every new item
// forever. It is deliberately distinct from want_content / acquire_release, which
// get one thing once. Both are first-class and both live behind these ops so the
// MCP door and the REST door express the same intent — the same "one intent, two
// doors" discipline WantContent is built on.

// FollowSourceRequest is the intent behind POST /followed-sources and MCP's
// follow_source: subscribe to a source, archiving everything it emits.
type FollowSourceRequest struct {
	// The feed identity — where to poll. Give a URL or an explicit external id;
	// the system infers the type where it can. A TVDB id or URL is a TV series; a
	// youtube.com channel-feed URL is a youtube_channel; any other http(s) feed
	// URL defaults to a podcast.
	URL    string `json:"url"`
	TVDBID string `json:"tvdb_id"`

	// Type optionally names the source type explicitly, for the cases inference
	// cannot resolve from the URL alone: a podcast RSS feed and an article RSS
	// feed are the same shape at the URL, so following an rss_feed (Phase 4)
	// requires the caller to say so. When given it is authoritative and must be an
	// implemented type consistent with the identity (e.g. not "podcast" for a
	// tvdb_id). When empty the type is inferred, and a plain feed URL stays a
	// podcast for backward compatibility.
	Type string `json:"type"`

	// The work identity — which series or podcast the items belong to, resolved
	// exactly as want_content resolves a work: an existing WorkID, or a Title
	// (and optional Year) that gets-or-creates the work so a follow and a later
	// scan converge on one work. content_type is derived from the inferred source
	// type (series or podcast), so a caller need not say it.
	WorkID string `json:"work_id"`
	Title  string `json:"title"`
	Year   int    `json:"year"`

	QualityProfileID string `json:"quality_profile_id"`
	QualityProfile   string `json:"quality_profile"`

	// Monitor defaults to true, carried onto every projected want.
	Monitor  *bool  `json:"monitor"`
	Backfill string `json:"backfill"`
	Reason   string `json:"reason"`

	// Retention is reserved. Phase 1 keeps everything a source emits, so a
	// retention policy that silently did nothing would be the quiet-failure knob
	// this codebase refuses to ship — a non-empty value is rejected by name.
	Retention string `json:"retention"`
}

// FollowedSourceView is a subscription as the API presents it, with the derived
// counts and health a caller reads to answer "is this working".
type FollowedSourceView struct {
	ID     string `json:"id"`
	WorkID string `json:"work_id"`
	// Title is the followed work's human name, inlined so a list of
	// subscriptions reads as titles rather than as work ids a client has to
	// join against /works to render (#430). Empty when the work is gone —
	// decoration must not fail the listing it decorates.
	Title string `json:"title"`
	// Type is the inferred source type — reported, never a request field.
	Type             string `json:"type"`
	FeedRef          string `json:"feed_ref"`
	QualityProfileID string `json:"quality_profile_id"`
	Monitor          bool   `json:"monitor"`
	Backfill         string `json:"backfill"`
	Reason           string `json:"reason,omitempty"`

	// ItemsKnown is how many items the feed has yielded; ItemsArchived how many
	// of those are held at the profile — the two numbers a person actually wants.
	ItemsKnown    int `json:"items_known"`
	ItemsArchived int `json:"items_archived"`

	// Health is the state of the adapter this source is polled through:
	// "healthy", "unhealthy", or "unknown" (never checked, or none configured).
	Health string `json:"health"`

	CreatedAt    time.Time  `json:"created_at"`
	LastPolledAt *time.Time `json:"last_polled_at,omitempty"`
	NextPollAt   *time.Time `json:"next_poll_at,omitempty"`
}

// tvdbSeriesID pulls a numeric TVDB series id out of a URL. TVDB series URLs
// carry the id either as a path segment (/series/12345, /dereferrer/series/12345)
// or as a query parameter (?id=12345). A slug URL (/series/the-series) has no
// numeric id without a lookup, so it is refused with a pointer to tvdb_id rather
// than guessed.
var (
	reTVDBPathID  = regexp.MustCompile(`series/(\d+)`)
	reTVDBQueryID = regexp.MustCompile(`[?&]id=(\d+)`)
	reAllDigits   = regexp.MustCompile(`^\d+$`)
	// A YouTube channel id is UC followed by 22 url-safe base64 chars; it appears
	// in a channel-feed URL's channel_id= query or as a /channel/<id> path
	// segment. A slug or @handle URL has no id without a lookup, handled like a
	// TVDB slug: refused with a pointer to the canonical feed URL.
	reYouTubeChannelID = regexp.MustCompile(`(?:channel_id=|/channel/)(UC[0-9A-Za-z_-]{22})`)
)

// youtubeFeedURL is the canonical channel-feed URL yt feeds live at. Built from
// the channel id so the stored feed_ref is the plain form the adapter fetches,
// whatever URL shape the caller passed.
func youtubeFeedURL(channelID string) string {
	return "https://www.youtube.com/feeds/videos.xml?channel_id=" + channelID
}

// inferFeed turns a caller's identity into the feed_ref a followed source stores
// AND the source type (#396, #415). It resolves the type from the identity where
// it can — a TVDB id/URL is tv_series, a youtube.com channel-feed URL is
// youtube_channel — and takes an explicit hint for the case inference cannot
// resolve: a podcast RSS feed and an article RSS feed are the same shape at the
// URL, so following an rss_feed needs the caller to say so. An empty hint keeps a
// plain feed URL a podcast, the backward-compatible default.
//
// A given hint is authoritative but must be consistent with the identity: a
// tvdb_id is a tv_series and saying otherwise is a caller error, refused by name
// rather than silently ignored.
func inferFeed(rawURL, tvdbID, typeHint string) (feedRef string, typ followed.Type, err error) {
	tvdbID = strings.TrimSpace(tvdbID)
	rawURL = strings.TrimSpace(rawURL)

	hint, err := parseTypeHint(typeHint)
	if err != nil {
		return "", "", err
	}
	// checkHint refuses a hint that contradicts an inferred type.
	checkHint := func(inferred followed.Type) error {
		if hint != "" && hint != inferred {
			return &badRequest{fmt.Errorf(
				"type %q contradicts the identity, which is a %s", hint, inferred)}
		}
		return nil
	}

	if tvdbID != "" {
		if !reAllDigits.MatchString(tvdbID) {
			return "", "", &badRequest{fmt.Errorf(
				"tvdb_id must be a numeric TVDB series id, not %q", tvdbID)}
		}
		if err := checkHint(followed.TypeTVSeries); err != nil {
			return "", "", err
		}
		return tvdbID, followed.TypeTVSeries, nil
	}
	if rawURL == "" {
		return "", "", &badRequest{errors.New(
			"a followed source needs a feed identity — a tvdb_id, or a url")}
	}

	lower := strings.ToLower(rawURL)
	if strings.Contains(lower, "thetvdb.com") {
		if err := checkHint(followed.TypeTVSeries); err != nil {
			return "", "", err
		}
		if m := reTVDBQueryID.FindStringSubmatch(rawURL); m != nil {
			return m[1], followed.TypeTVSeries, nil
		}
		if m := reTVDBPathID.FindStringSubmatch(rawURL); m != nil {
			return m[1], followed.TypeTVSeries, nil
		}
		return "", "", &badRequest{errors.New(
			"that TVDB URL has no numeric series id — pass the numeric id as tvdb_id " +
				"(a slug URL cannot be resolved without a lookup)")}
	}

	// A YouTube channel is recognised from the URL alone — the feed URL or a
	// /channel/<id> URL both carry the channel id — so it needs no hint, and its
	// feed_ref is normalised to the canonical channel-feed URL.
	if strings.Contains(lower, "youtube.com") || strings.Contains(lower, "youtu.be") {
		if err := checkHint(followed.TypeYouTubeChannel); err != nil {
			return "", "", err
		}
		if m := reYouTubeChannelID.FindStringSubmatch(rawURL); m != nil {
			return youtubeFeedURL(m[1]), followed.TypeYouTubeChannel, nil
		}
		return "", "", &badRequest{errors.New(
			"that YouTube URL has no channel id — pass the channel-feed URL " +
				"(youtube.com/feeds/videos.xml?channel_id=UC…) or a /channel/UC… URL " +
				"(an @handle or custom URL cannot be resolved without a lookup)")}
	}

	// A plain feed URL, whose feed_ref is the URL the adapter fetches per poll. It
	// must be an http(s) URL: a bare host or a mistyped scheme would be stored and
	// then fail every enumeration, looking like a dead feed rather than a typo.
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return "", "", &badRequest{fmt.Errorf(
			"a followed source's url must be an http(s) feed url, not %q", rawURL)}
	}
	// Podcast and article feeds are indistinguishable here, so the hint decides;
	// with no hint it stays a podcast, the type this fall-through has always meant.
	switch hint {
	case followed.TypeRSSFeed:
		return rawURL, followed.TypeRSSFeed, nil
	case "", followed.TypePodcast:
		return rawURL, followed.TypePodcast, nil
	default:
		// A youtube_channel or tv_series hint on a plain feed URL is a mismatch —
		// those types are recognised from their own URL shapes above, so a hint
		// naming one here contradicts the identity the caller actually gave.
		return "", "", &badRequest{fmt.Errorf(
			"type %q does not match a plain feed url — that url is followed as a podcast or rss_feed", hint)}
	}
}

// parseTypeHint validates an optional explicit source type. Empty is "infer".
// An unknown or not-yet-implemented type is refused by name rather than ignored,
// for the reason ParseType gives: a silently dropped type produces a source that
// is configured, healthy and never usefully polled.
func parseTypeHint(s string) (followed.Type, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	t, err := followed.ParseType(s)
	if err != nil {
		return "", &badRequest{err}
	}
	if !t.Implemented() {
		return "", &badRequest{fmt.Errorf("source type %q is not implemented yet", t)}
	}
	return t, nil
}

// workContentType is the §12 content type a source's items belong to, so a
// follow and a later scan converge on ONE work (resolveWorkDescriptor). A TV
// series' items are episodes under a series work; a podcast's episodes under a
// podcast work; a channel's videos under a video work; an article feed's entries
// under a document work.
func workContentType(t followed.Type) string {
	switch t {
	case followed.TypePodcast:
		return "podcast"
	case followed.TypeYouTubeChannel:
		return "video"
	case followed.TypeRSSFeed:
		return "document"
	default:
		return "series"
	}
}

// FollowSource creates a subscription from an intent (§55). Exported and shared
// by POST /followed-sources and MCP's follow_source, for the reason WantContent
// is: following is one intent, and both doors must create the source, its poll
// bookkeeping and its event through one path.
func (a *API) FollowSource(ctx context.Context, req FollowSourceRequest) (FollowedSourceView, error) {
	if strings.TrimSpace(req.Retention) != "" {
		return FollowedSourceView{}, &badRequest{errors.New(
			"a retention policy is not implemented yet — Phase 1 keeps everything a source emits")}
	}
	if req.WorkID != "" && strings.TrimSpace(req.Title) != "" {
		return FollowedSourceView{}, &badRequest{errors.New(
			"name the series with either work_id or title, not both")}
	}
	if req.WorkID == "" && strings.TrimSpace(req.Title) == "" {
		return FollowedSourceView{}, &badRequest{errors.New(
			"a followed source must name the series — by work_id, or by title if it is not catalogued yet")}
	}
	if req.QualityProfileID != "" && req.QualityProfile != "" {
		return FollowedSourceView{}, &badRequest{errors.New(
			"name the quality profile with either quality_profile_id or quality_profile, not both")}
	}

	feedRef, srcType, err := inferFeed(req.URL, req.TVDBID, req.Type)
	if err != nil {
		return FollowedSourceView{}, err
	}

	backfill := followed.Backfill(strings.TrimSpace(req.Backfill))
	if backfill == "" {
		backfill = followed.BackfillFromNow
	}
	monitor := true
	if req.Monitor != nil {
		monitor = *req.Monitor
	}

	// Resolve the profile and (get-or-create) the work first, in one transaction
	// — the same shape WantContent takes, and for the same reason: the catalog's
	// CreateFollowSource takes ids already resolved.
	var profileID, workID string
	if err := a.db.InTx(ctx, func(tx *sql.Tx) error {
		var e error
		// A follow that names no profile inherits the source's content-type
		// default (ADR-0082): an rss_feed is a `document` and inherits `published`,
		// never the video profile that would reject its captured articles.
		profileID, e = a.resolveProfile(ctx, tx, req.QualityProfileID, req.QualityProfile, workContentType(srcType))
		if e != nil {
			return e
		}
		workID = req.WorkID
		if req.WorkID == "" {
			workID, e = a.resolveWorkDescriptor(ctx, tx, WorkDescriptor{
				ContentType: workContentType(srcType), Title: req.Title, Year: req.Year,
			})
		}
		return e
	}); err != nil {
		return FollowedSourceView{}, err
	}

	src, err := a.catalog.CreateFollowSource(ctx, followed.Source{
		WorkID:           workID,
		Type:             srcType,
		FeedRef:          feedRef,
		QualityProfileID: profileID,
		Monitor:          monitor,
		Backfill:         backfill,
		Reason:           req.Reason,
	})
	if err != nil {
		return FollowedSourceView{}, err
	}

	// Poll it now rather than waiting up to a follow-beat tick, the same
	// immediacy WantContent gives an operator. Best-effort: the beat picks a
	// never-polled source up regardless, so a briefly-unavailable queue costs
	// latency, not correctness.
	if _, err := a.jobs.Enqueue(ctx, jobs.EnqueueOptions{
		Type:      followed.PollSourceJobType,
		Payload:   followed.PollSourcePayload{SourceID: src.ID},
		DedupeKey: followed.PollDedupeKey(src.ID),
	}); err != nil {
		a.log.Warn("could not enqueue the first poll for a new followed source",
			"source_id", src.ID, "error", err)
	}

	view := a.followViewFor(ctx, src, a.metadataHealthLabel())
	return view, nil
}

// ListFollowed returns every subscription with its derived counts and health,
// shared by GET /followed-sources and MCP's list_followed.
func (a *API) ListFollowed(ctx context.Context) ([]FollowedSourceView, error) {
	sources, err := a.catalog.ListFollowSources(ctx)
	if err != nil {
		return nil, err
	}
	health := a.metadataHealthLabel()
	out := make([]FollowedSourceView, 0, len(sources))
	for _, s := range sources {
		out = append(out, a.followViewFor(ctx, s, health))
	}
	return out, nil
}

// Unfollow stops a subscription, shared by DELETE /followed-sources/{id} and
// MCP's unfollow. keepArchive true (the default) stops future polls and keeps
// every Item and Asset already archived; false — removing the archive — is not
// implemented in Phase 1 and is refused rather than silently ignored.
func (a *API) Unfollow(ctx context.Context, id string, keepArchive bool) error {
	if !keepArchive {
		return &badRequest{errors.New(
			"removing the archive is not implemented yet — Phase 1 unfollow stops polling " +
				"and keeps what was archived (keep_archive defaults to true)")}
	}
	existed, err := a.catalog.DeleteFollowSource(ctx, id)
	if err != nil {
		return err
	}
	if !existed {
		return sql.ErrNoRows
	}
	return nil
}

// RepointRequest is the intent behind PATCH /followed-sources/{id} and MCP's
// set_source_profile: change which quality profile a subscription — and every
// want it has projected — is judged against (ADR-0082). Exactly one of the two
// ways to name the profile is given, the same rule follow and want take.
type RepointRequest struct {
	QualityProfileID string `json:"quality_profile_id"`
	QualityProfile   string `json:"quality_profile"`
}

// RepointSource changes a subscription's quality profile in place, shared by
// PATCH /followed-sources/{id} and MCP's set_source_profile (ADR-0082).
//
// It exists because follow and unfollow were the only doors onto a subscription
// (ADR-0057), so "this feed is on the wrong profile" forced an unfollow and
// refollow — which strands the refollowed wants behind their own already-done,
// content-addressed captures. This is the door that was missing: strategy is a
// property of the subscription an operator can correct, not only a thing chosen
// once at follow time.
//
// The repoint moves the source AND its wants together (RepointFollowedSource),
// then reconciles each moved want at once, so an asset the library already holds
// is re-judged against the new profile now rather than on the next sweep — the
// captured article that the old video profile refused becomes satisfied the
// moment this returns. The reconcile enqueue is best-effort and idempotent (the
// dedupe key): a briefly-unavailable queue costs latency, not correctness,
// because the reconcile beat re-judges everything regardless.
func (a *API) RepointSource(ctx context.Context, id string, req RepointRequest) (FollowedSourceView, error) {
	if req.QualityProfileID != "" && req.QualityProfile != "" {
		return FollowedSourceView{}, &badRequest{errors.New(
			"name the quality profile with either quality_profile_id or quality_profile, not both")}
	}
	if req.QualityProfileID == "" && req.QualityProfile == "" {
		return FollowedSourceView{}, &badRequest{errors.New(
			"a repoint must name the quality profile to move to, by quality_profile_id or quality_profile")}
	}

	// Resolve the profile name to an id, with no content-type default: a repoint
	// is an explicit act, so an empty profile is refused rather than guessed.
	var profileID string
	if err := a.db.InTx(ctx, func(tx *sql.Tx) error {
		var e error
		profileID, e = a.resolveProfile(ctx, tx, req.QualityProfileID, req.QualityProfile, "")
		return e
	}); err != nil {
		return FollowedSourceView{}, err
	}

	wants, err := a.catalog.RepointFollowedSource(ctx, id, profileID)
	if errors.Is(err, catalog.ErrNoFollowSource) {
		return FollowedSourceView{}, sql.ErrNoRows
	}
	if err != nil {
		return FollowedSourceView{}, err
	}

	for _, wantID := range wants {
		if _, e := a.jobs.Enqueue(ctx, jobs.EnqueueOptions{
			Type:      acquisition.ReconcileJobType,
			Payload:   acquisition.ReconcilePayload{DesiredItemID: wantID},
			DedupeKey: acquisition.ReconcileDedupeKey + ":" + wantID,
		}); e != nil {
			a.log.Warn("could not enqueue reconciliation for a repointed want",
				"desired_item_id", wantID, "error", e)
		}
	}

	return a.FollowedSourceDetail(ctx, id)
}

// PollSource enqueues an immediate poll for one followed source, shared by
// POST /followed-sources/{id}/poll and MCP's poll_source. It reuses the exact
// enqueue the follow door runs at follow time (FollowSource) and the follow
// beat runs on the tick (followScheduler.dispatch) — the same job type, the
// same dedupe key, the same worker — rather than reimplementing a poll, so an
// on-demand poll and a scheduled one cannot come to mean different things.
//
// Idempotent by the same mechanism those two paths rely on: the dedupe key
// (ADR-0008) means a source that already has a poll queued gets that live job
// back rather than a second one, so asking twice cannot double-poll. And it is
// deliberately an EXTRA poll — the source's scheduled next_poll_at is left
// untouched. Forcing a poll now is not evidence about when the next scheduled
// poll is due, and the worker's RecordPollOutcome still owns that schedule; a
// forced poll that also rescheduled would let an operator refreshing a feed
// quietly push its regular cadence around. A missing source is sql.ErrNoRows,
// which both doors render as a not-found.
func (a *API) PollSource(ctx context.Context, id string) (map[string]string, error) {
	src, err := a.followedSource(ctx, id)
	if err != nil {
		return nil, err
	}
	job, err := a.jobs.Enqueue(ctx, jobs.EnqueueOptions{
		Type:      followed.PollSourceJobType,
		Payload:   followed.PollSourcePayload{SourceID: src.ID},
		DedupeKey: followed.PollDedupeKey(src.ID),
	})
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"source_id": src.ID,
		"job_id":    job.ID,
		"status":    "queued",
	}, nil
}

// PollAllSources enqueues an immediate poll for every followed source, behind
// POST /followed-sources/poll — the "poll everything now" an operator reaches
// for to refresh many feeds at once, or after a controller was down. It reuses
// the per-source enqueue PollSource does, so it inherits the same dedupe
// idempotency (a source already carrying a queued poll is not polled twice) and
// the same leave-the-schedule-alone stance (these are extra polls).
//
// Best-effort per source, the same stance followScheduler.dispatch takes: one
// source failing to enqueue is logged and skipped rather than failing the whole
// sweep, so a single bad row does not deny the operator the rest of the refresh.
func (a *API) PollAllSources(ctx context.Context) (map[string]any, error) {
	sources, err := a.catalog.ListFollowSources(ctx)
	if err != nil {
		return nil, err
	}
	queued := make([]string, 0, len(sources))
	for _, s := range sources {
		if _, err := a.jobs.Enqueue(ctx, jobs.EnqueueOptions{
			Type:      followed.PollSourceJobType,
			Payload:   followed.PollSourcePayload{SourceID: s.ID},
			DedupeKey: followed.PollDedupeKey(s.ID),
		}); err != nil {
			a.log.Warn("could not enqueue a source poll", "source_id", s.ID, "error", err)
			continue
		}
		queued = append(queued, s.ID)
	}
	return map[string]any{
		"status":            "queued",
		"count":             len(queued),
		"queued_source_ids": queued,
	}, nil
}

// followViewFor builds the wire view for one stored source, filling the counts
// and the shared health label.
func (a *API) followViewFor(ctx context.Context, s catalog.StoredSource, health string) FollowedSourceView {
	view := FollowedSourceView{
		ID: s.ID, WorkID: s.WorkID, Type: string(s.Type), FeedRef: s.FeedRef,
		QualityProfileID: s.QualityProfileID, Monitor: s.Monitor,
		Backfill: string(s.Backfill), Reason: s.Reason,
		Health: health, CreatedAt: s.CreatedAt,
	}
	if !s.LastPolledAt.IsZero() {
		t := s.LastPolledAt
		view.LastPolledAt = &t
	}
	if !s.NextPollAt.IsZero() {
		t := s.NextPollAt
		view.NextPollAt = &t
	}
	known, archived, err := a.catalog.FollowStats(ctx, s.WorkID)
	if err != nil {
		// A counting failure must not blank the whole listing. The source is
		// still real; its counts are reported as zero and the caller reads the
		// health and timestamps, which are what say whether it is working.
		a.log.Warn("could not count a followed source's items", "source_id", s.ID, "error", err)
	}
	view.ItemsKnown, view.ItemsArchived = known, archived
	title, err := a.catalog.WorkTitle(ctx, s.WorkID)
	if err != nil {
		// Same stance as the counts: a title that cannot be read leaves the
		// subscription listed and unnamed, rather than failing the request.
		a.log.Warn("could not read a followed source's work title",
			"source_id", s.ID, "error", err)
	}
	view.Title = title
	return view
}

// metadataHealthLabel is the shared health of the feed adapters, the same
// hold-off signal the follow beat reads: healthy if any metadata provider is
// healthy, unhealthy if one exists and all are unhealthy, unknown otherwise
// (none configured, or none yet checked). It is a property of the node, not of
// one source, because Phase 1 polls every source through the one adapter.
func (a *API) metadataHealthLabel() string {
	if a.providers == nil {
		return "unknown"
	}
	var sawMetadata, sawUnhealthy bool
	for _, st := range a.providers.Statuses() {
		if !hasCapabilityNamed(st.Capabilities, providers.CapabilityMetadata) {
			continue
		}
		sawMetadata = true
		if st.Health.CheckedAt.IsZero() {
			continue // never checked — unknown, not unhealthy
		}
		if st.Health.Healthy {
			return "healthy"
		}
		sawUnhealthy = true
	}
	switch {
	case sawUnhealthy:
		return "unhealthy"
	case sawMetadata:
		return "unknown" // configured but never checked
	default:
		return "unknown" // none configured
	}
}

func hasCapabilityNamed(caps []providers.Capability, want providers.Capability) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

// createFollowedSource is POST /api/v1/followed-sources — a shell over FollowSource.
func (a *API) createFollowedSource(w http.ResponseWriter, r *http.Request) {
	var body FollowSourceRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	out, err := a.FollowSource(r.Context(), body)
	if err != nil {
		a.failFollowWrite(w, r, err)
		return
	}
	w.Header().Set("Location", httpapi.APIPrefix+"/followed-sources/"+out.ID)
	a.write(w, r, http.StatusCreated, out)
}

// listFollowedSources is GET /api/v1/followed-sources.
func (a *API) listFollowedSources(w http.ResponseWriter, r *http.Request) {
	out, err := a.ListFollowed(r.Context())
	if err != nil {
		a.fail(w, r, "followed source", err)
		return
	}
	a.write(w, r, http.StatusOK, map[string]any{"followed_sources": out})
}

// FollowedItemView is one Item a subscription's feed yielded, as the API
// presents it: what the source emitted, and what heyarr did about it (#430,
// ADR-0056/0057/0059).
//
// The acquisition half is a POINTER because it is genuinely absent for an item
// the source has not projected a want for — a backfill=from_now source knows
// about back-catalogue episodes it deliberately did not ask for. Reporting
// those with a null want, rather than omitting them, is what makes this listing
// an ARCHIVE (what the source has emitted) rather than a queue (what is being
// fetched).
type FollowedItemView struct {
	ID     string `json:"id"`
	WorkID string `json:"work_id"`
	// EditionID is the grouping the item belongs to — a season, for a series —
	// and is absent for a source type that has none (ADR-0056).
	EditionID string `json:"edition_id,omitempty"`
	// ItemKey is the source-stable identity the feed adapter supplied: an
	// "S02E05", a podcast GUID, a video id. It is what dedupes the item across
	// polls, and it is the listing's sort key.
	ItemKey string `json:"item_key"`
	Title   string `json:"title"`
	// PublishedAt is when the source said it emitted the item, absent when the
	// source did not say — a real and distinct answer from any instant.
	PublishedAt *time.Time      `json:"published_at,omitempty"`
	Attributes  json.RawMessage `json:"attributes"`
	CreatedAt   time.Time       `json:"created_at"`

	// Want is the item-scoped want this source projected, null when none.
	Want *FollowedItemWant `json:"want"`
	// Archived says the plain thing a person is asking: heyarr holds bytes for
	// this item that satisfy the source's profile. Derived from the want's
	// content axis so a client need not know §64's vocabulary to render a tick.
	Archived bool `json:"archived"`
}

// FollowedItemWant is the projected want's id and §64's three axes.
type FollowedItemWant struct {
	DesiredItemID string `json:"desired_item_id"`
	Phase         string `json:"phase"`
	Content       string `json:"content"`
	Placement     string `json:"placement"`
}

// FollowedSourceDetail reads one subscription's view — the intent behind GET
// /followed-sources/{id} and the MCP followed_source_items tool, so the two
// doors cannot drift. A missing subscription is sql.ErrNoRows, which both doors
// render as a not-found rather than a failure.
func (a *API) FollowedSourceDetail(ctx context.Context, id string) (FollowedSourceView, error) {
	src, err := a.followedSource(ctx, id)
	if err != nil {
		return FollowedSourceView{}, err
	}
	return a.followViewFor(ctx, src, a.metadataHealthLabel()), nil
}

// FollowedSourceItems pages the items one subscription has archived and merely
// knows about — the intent behind GET /followed-sources/{id}/items and the MCP
// followed_source_items tool. Paged by item_key (the feed-stable identity), so
// a re-poll inserting an older episode does not shuffle the pages. after is the
// previous page's last item_key, empty for the first page.
func (a *API) FollowedSourceItems(ctx context.Context, id, after string, limit int) ([]FollowedItemView, string, error) {
	src, err := a.followedSource(ctx, id)
	if err != nil {
		return nil, "", err
	}
	items, err := a.catalog.ProjectedItems(ctx, src.WorkID, after, limit+1)
	if err != nil {
		return nil, "", err
	}
	views := make([]FollowedItemView, 0, len(items))
	for _, it := range items {
		views = append(views, followedItemView(it))
	}
	p := newPage(views, limit,
		func(x FollowedItemView) []string { return []string{x.ItemKey} },
		"followed-source-items")
	return p.Items, p.NextCursor, nil
}

// getFollowedSource is GET /api/v1/followed-sources/{id} (#430). A detail
// screen had to re-read the whole list and pick its id out of it.
func (a *API) getFollowedSource(w http.ResponseWriter, r *http.Request) {
	view, err := a.FollowedSourceDetail(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		a.fail(w, r, "followed source", err)
		return
	}
	a.write(w, r, http.StatusOK, view)
}

// followedSource reads one subscription, mapping the catalog's own not-found
// sentinel onto sql.ErrNoRows so `fail` renders the 404 every other item route
// renders. Without it a missing subscription would be a 500.
func (a *API) followedSource(ctx context.Context, id string) (catalog.StoredSource, error) {
	src, err := a.catalog.FollowSource(ctx, id)
	if errors.Is(err, catalog.ErrNoFollowSource) {
		return catalog.StoredSource{}, sql.ErrNoRows
	}
	return src, err
}

// listFollowedSourceItems is GET /api/v1/followed-sources/{id}/items (#430):
// what this source has archived, and what it merely knows about.
//
// Paged by item_key rather than by id, because item_key is what the feed is
// stable on and what the (work_id, item_key) index orders — so the page
// boundary is the same order a person reads the feed in, and a re-poll that
// inserts an older episode does not shuffle the pages under them.
func (a *API) listFollowedSourceItems(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r, "followed-source-items", 1)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	after := ""
	if q.cursor != nil {
		after = q.cursor[0]
	}
	views, next, err := a.FollowedSourceItems(r.Context(), chi.URLParam(r, "id"), after, q.limit)
	if err != nil {
		a.fail(w, r, "followed source", err)
		return
	}
	a.write(w, r, http.StatusOK, page[FollowedItemView]{Items: views, NextCursor: next})
}

// followedItemView is the wire projection of one stored item and its want.
func followedItemView(it catalog.ProjectedItem) FollowedItemView {
	view := FollowedItemView{
		ID: it.ID, WorkID: it.WorkID, EditionID: it.EditionID, ItemKey: it.ItemKey,
		Title: it.Title, Attributes: it.Attributes, CreatedAt: it.CreatedAt,
	}
	if !it.PublishedAt.IsZero() {
		t := it.PublishedAt
		view.PublishedAt = &t
	}
	if it.Want.Projected() {
		view.Want = &FollowedItemWant{
			DesiredItemID: it.Want.DesiredItemID,
			Phase:         it.Want.Phase,
			Content:       it.Want.Content,
			Placement:     it.Want.Placement,
		}
		view.Archived = it.Want.Content == "satisfied"
	}
	return view
}

// deleteFollowedSource is DELETE /api/v1/followed-sources/{id}. keep_archive is
// a query parameter defaulting to true.
func (a *API) deleteFollowedSource(w http.ResponseWriter, r *http.Request) {
	keepArchive := true
	if v := strings.TrimSpace(r.URL.Query().Get("keep_archive")); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			httpapi.Fail(w, r, problem.BadRequest("keep_archive must be true or false"))
			return
		}
		keepArchive = parsed
	}
	if err := a.Unfollow(r.Context(), chi.URLParam(r, "id"), keepArchive); err != nil {
		a.failFollowWrite(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pollFollowedSource is POST /api/v1/followed-sources/{id}/poll — force one
// source to poll now rather than waiting for its scheduled next_poll_at. It
// enqueues and returns 202 Accepted, the same shape a manual want search takes,
// because the poll is a job the worker runs afterwards, not this request. The
// source's schedule is left as-is (this is an extra poll, not a reschedule).
func (a *API) pollFollowedSource(w http.ResponseWriter, r *http.Request) {
	out, err := a.PollSource(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		a.fail(w, r, "followed source", err)
		return
	}
	a.write(w, r, http.StatusAccepted, out)
}

// pollAllFollowedSources is POST /api/v1/followed-sources/poll — force every
// followed source to poll now. The bulk sibling of pollFollowedSource; it never
// 404s (an empty library is a valid, empty sweep) and leaves each schedule as-is.
func (a *API) pollAllFollowedSources(w http.ResponseWriter, r *http.Request) {
	out, err := a.PollAllSources(r.Context())
	if err != nil {
		a.fail(w, r, "followed source", err)
		return
	}
	a.write(w, r, http.StatusAccepted, out)
}

// patchFollowedSource is PATCH /api/v1/followed-sources/{id} — repoint a
// subscription at a different quality profile in place (ADR-0082), the door that
// spares an operator an unfollow-and-refollow to correct a strategy. It returns
// the updated detail, with items_archived already reflecting the re-judged
// wants.
func (a *API) patchFollowedSource(w http.ResponseWriter, r *http.Request) {
	var body RepointRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	out, err := a.RepointSource(r.Context(), chi.URLParam(r, "id"), body)
	if err != nil {
		a.failFollowWrite(w, r, err)
		return
	}
	a.write(w, r, http.StatusOK, out)
}

// failFollowWrite renders a write failure, mapping the (work, feed) uniqueness
// violation to a 409 the way failDesiredWrite maps a duplicate want.
func (a *API) failFollowWrite(w http.ResponseWriter, r *http.Request, err error) {
	var bad *badRequest
	switch {
	case errors.As(err, &bad):
		httpapi.Fail(w, r, problem.BadRequest(bad.err.Error()))
	case isUniqueViolation(err):
		httpapi.Fail(w, r, problem.Conflict(
			"that series is already followed through that feed"))
	case isForeignKeyViolation(err):
		httpapi.Fail(w, r, problem.BadRequest("the work or quality profile named does not exist"))
	default:
		a.fail(w, r, "followed source", err)
	}
}

// FollowClientFault classifies a FollowSource/Unfollow error for the MCP door,
// the sibling of ClientFault: it maps a caller's fault to a message and a bool,
// so the two doors agree about whose fault an error is.
func FollowClientFault(err error) (string, bool) {
	var bad *badRequest
	switch {
	case errors.As(err, &bad):
		return bad.err.Error(), true
	case isUniqueViolation(err):
		return "that series is already followed through that feed", true
	case isForeignKeyViolation(err):
		return "the work or quality profile named does not exist", true
	case errors.Is(err, sql.ErrNoRows):
		return "there is no followed source with that id", true
	}
	return "", false
}

// mountFollowedSources registers the routes. Following is ordinary operator
// traffic, so writes need `write` rather than `admin`.
func (a *API) mountFollowedSources(r chi.Router) {
	r.Get("/followed-sources", a.listFollowedSources)
	r.Get("/followed-sources/{id}", a.getFollowedSource)
	// What the subscription has actually got: the items its feed yielded, each
	// with the want it projected and that want's acquisition axes (#430). A
	// read, under the floor the router already requires.
	r.Get("/followed-sources/{id}/items", a.listFollowedSourceItems)
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/followed-sources", a.createFollowedSource)
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Delete("/followed-sources/{id}", a.deleteFollowedSource)
	// Repoint a subscription at a different quality profile in place (ADR-0082) —
	// change its strategy without unfollowing. Ordinary operator write traffic.
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Patch("/followed-sources/{id}", a.patchFollowedSource)
	// Force a poll now instead of waiting for the ~6h next_poll_at. The bulk
	// route is a static sibling of the per-source one, so chi's static-over-param
	// precedence routes /followed-sources/poll here and /followed-sources/{id}/poll
	// to the per-source handler. Both are extra polls that leave the schedule as-is.
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/followed-sources/poll", a.pollAllFollowedSources)
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/followed-sources/{id}/poll", a.pollFollowedSource)
}
