package resources

import (
	"context"
	"database/sql"
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
	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Followed sources (§55, M12) — the follow surface, source-agnostic by design.
//
// # Why there is no `source` / `provider` / `feed_type` parameter (#396)
//
// A caller says "follow this" with a content intent (which series) and an
// identity (a URL, or an explicit external id) — never which adapter to use.
// The system INFERS the type from the identity: a TVDB id or URL is a TV series
// in Phase 1, and the other three source types are refused with a message that
// says so rather than pretending. Picking the adapter is the registry's job, the
// same stance the provider layer takes for indexers; a `feed_type` knob would
// hand the caller a decision they should not have to make and could get wrong.
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
	// the system infers the type. Phase 1 understands a TVDB series.
	URL    string `json:"url"`
	TVDBID string `json:"tvdb_id"`

	// The work identity — which series the items belong to, resolved exactly as
	// want_content resolves a work: an existing WorkID, or a Title (and optional
	// Year) that gets-or-creates the series work so a follow and a later scan
	// converge on one work. content_type is always "series" for a followed source
	// in Phase 1, so a caller need not say it.
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
)

// inferTVSeriesFeed turns a caller's identity into the feed_ref a followed
// source stores, inferring the type. Phase 1 implements tv_series only; anything
// that is not a TVDB series is refused loudly rather than stored unpolled.
func inferTVSeriesFeed(rawURL, tvdbID string) (feedRef string, err error) {
	tvdbID = strings.TrimSpace(tvdbID)
	rawURL = strings.TrimSpace(rawURL)

	if tvdbID != "" {
		if !reAllDigits.MatchString(tvdbID) {
			return "", &badRequest{fmt.Errorf(
				"tvdb_id must be a numeric TVDB series id, not %q", tvdbID)}
		}
		return tvdbID, nil
	}
	if rawURL == "" {
		return "", &badRequest{errors.New(
			"a followed source needs a feed identity — a tvdb_id, or a url")}
	}
	if !strings.Contains(strings.ToLower(rawURL), "thetvdb.com") {
		// Not a TVDB reference, so not something Phase 1 can follow. The message
		// names the limit rather than the URL, because "we cannot follow this"
		// and "this URL is malformed" are different answers.
		return "", &badRequest{errors.New(
			"following this source is not implemented yet — Phase 1 follows tv_series only " +
				"(give a TVDB series id or URL)")}
	}
	if m := reTVDBQueryID.FindStringSubmatch(rawURL); m != nil {
		return m[1], nil
	}
	if m := reTVDBPathID.FindStringSubmatch(rawURL); m != nil {
		return m[1], nil
	}
	return "", &badRequest{errors.New(
		"that TVDB URL has no numeric series id — pass the numeric id as tvdb_id " +
			"(a slug URL cannot be resolved without a lookup)")}
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

	feedRef, err := inferTVSeriesFeed(req.URL, req.TVDBID)
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
		profileID, e = a.resolveProfile(ctx, tx, req.QualityProfileID, req.QualityProfile)
		if e != nil {
			return e
		}
		workID = req.WorkID
		if req.WorkID == "" {
			workID, e = a.resolveWorkDescriptor(ctx, tx, WorkDescriptor{
				ContentType: "series", Title: req.Title, Year: req.Year,
			})
		}
		return e
	}); err != nil {
		return FollowedSourceView{}, err
	}

	src, err := a.catalog.CreateFollowSource(ctx, followed.Source{
		WorkID:           workID,
		Type:             followed.TypeTVSeries,
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
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/followed-sources", a.createFollowedSource)
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Delete("/followed-sources/{id}", a.deleteFollowedSource)
}
