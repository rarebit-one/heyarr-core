package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/api/resources"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// The tool surface (§71).
//
// Eleven verbs, each one §71 names. Two more that §71 names are ABSENT and
// recorded in deferred.go with the milestone that brings them — see the
// package doc for why absent beats stubbed.
//
// search_releases and acquire_release moved from that list to this one in M6:
// they were deferred for reasons that had stopped being true, which produces an
// agent waiting for a milestone that already shipped (#226).
//
// The split between the registrations and the handlers is deliberate: the
// registrations are the VOCABULARY, and reviewing the authorisation surface
// means reading their Scope column rather than opening every handler. They are
// grouped one lane per file (tools_*.go, renderers.go) so each lane still reads
// in one screen.

func (s *Server) registerTools() {
	// The renderer lane (§68), in its own file: four verbs about the
	// physical world rather than about the catalog.
	s.registerRendererTools()
	s.registerLibraryTools()     // tools_library.go
	s.registerAcquisitionTools() // tools_acquisition.go
	s.registerFollowedTools()    // tools_followed.go
	s.registerFabricTools()      // tools_fabric.go
}

// decodeArgs unmarshals a tool's arguments, rejecting unknown fields.
//
// An agent that sent {"titel": "..."} and got a cheerful empty result would
// have no way to learn it had misspelled anything. Refusing is how it finds
// out, and an agent is far better than a human at correcting once told.
func decodeArgs(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return invalidParams("the arguments are not valid: %s", err.Error())
	}
	return nil
}

// wantContentArgs is what wantContent decodes (held to its schema by schemaargs_test.go).
type wantContentArgs struct {
	WorkID         string `json:"work_id"`
	Title          string `json:"title"`
	ContentType    string `json:"content_type"`
	Year           int    `json:"year"`
	QualityProfile string `json:"quality_profile"`
	Monitor        *bool  `json:"monitor"`
	Reason         string `json:"reason"`
}

// wantContent is the write intent, shared with POST /api/v1/desired.
//
// The whole body of this function is the argument shape and the delegation.
// That is the point: the intent lives in resources.WantContent, and both doors
// call it, so the acquisition row, the two events and the immediate
// reconciliation cannot happen through one door and not the other.
func (s *Server) wantContent(ctx context.Context, raw json.RawMessage) (any, error) {
	var args wantContentArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}

	req := resources.WantContentRequest{
		WorkID:         args.WorkID,
		QualityProfile: args.QualityProfile,
		Monitor:        args.Monitor,
		Reason:         args.Reason,
	}
	if args.Title != "" {
		req.Work = &resources.WorkDescriptor{
			ContentType: args.ContentType,
			Title:       args.Title,
			Year:        args.Year,
		}
	}

	out, err := s.resources.WantContent(ctx, req)
	if err != nil {
		return nil, classify(err)
	}
	// A whole-series want establishes a follow instead of a one-off want
	// (ADR-0089); return whichever the intent produced.
	if out.Followed != nil {
		return out.Followed, nil
	}
	return out.Desired, nil
}

// monitorContentArgs is what monitorContent decodes (held to its schema by schemaargs_test.go).
type monitorContentArgs struct {
	DesiredItemID string `json:"desired_item_id"`
	Monitor       *bool  `json:"monitor"`
}

// monitorContent is the other write intent, shared with PATCH /desired/{id}.
func (s *Server) monitorContent(ctx context.Context, raw json.RawMessage) (any, error) {
	var args monitorContentArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.DesiredItemID == "" {
		return nil, invalidParams("desired_item_id is required")
	}
	if args.Monitor == nil {
		// Required rather than defaulted. "monitor_content" with no value is
		// ambiguous between on and off, and guessing either way is a change
		// nobody asked for.
		return nil, invalidParams("monitor is required — true to keep looking, false to stop")
	}

	item, err := s.resources.UpdateDesired(ctx, args.DesiredItemID,
		resources.UpdateDesiredRequest{Monitor: args.Monitor})
	if err != nil {
		return nil, classify(err)
	}
	return item, nil
}

// searchReleasesArgs is what searchReleases decodes (held to its schema by schemaargs_test.go).
type searchReleasesArgs struct {
	DesiredItemID string `json:"desired_item_id"`
}

// searchReleases is §71's search_releases.
//
// It queues the search rather than performing it, and says so, because a search
// is a job (invariant 4) that a different process may run and an indexer may
// take thirty seconds to refuse. An agent that needs the answer reads the
// want's candidates afterwards — get_content_satisfaction explains what is
// held, and explain_release scores what was offered.
func (s *Server) searchReleases(ctx context.Context, raw json.RawMessage) (any, error) {
	var args searchReleasesArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.DesiredItemID == "" {
		return nil, invalidParams("desired_item_id is required")
	}
	out, err := s.resources.SearchReleases(ctx, args.DesiredItemID)
	if err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// acquireReleaseArgs is what acquireRelease decodes (held to its schema by schemaargs_test.go).
type acquireReleaseArgs struct {
	DesiredItemID string `json:"desired_item_id"`
	CandidateID   string `json:"candidate_id"`
}

// acquireRelease is §71's acquire_release.
//
// It is §60's manual override reached by an agent: select this candidate, and
// arrange for it to be fetched. It refuses a candidate the quality profile
// rejected — an agent that could override a gate would turn the operator's own
// statement of what is acceptable into a suggestion, and the reason it gets
// back names the rule so it can say WHY rather than that it failed.
func (s *Server) acquireRelease(ctx context.Context, raw json.RawMessage) (any, error) {
	var args acquireReleaseArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.DesiredItemID == "" {
		return nil, invalidParams("desired_item_id is required")
	}
	if args.CandidateID == "" {
		return nil, invalidParams("candidate_id is required — name the release to acquire")
	}
	chosen, err := s.resources.AcquireRelease(ctx, args.DesiredItemID, args.CandidateID)
	switch {
	case errors.Is(err, catalog.ErrNoCandidate):
		// Named explicitly rather than left to classify, which would answer
		// "the tool failed" — and this server's own instructions tell an agent
		// that the reason is the deliverable and to quote it to the person it
		// is helping. "The tool failed" is not quotable.
		return nil, invalidParams("no candidate with that id for this want — " +
			"it may have been superseded by a later search; run search_releases and look again")
	case errors.Is(err, catalog.ErrNotAcceptable):
		return nil, invalidParams("that candidate was rejected by the quality profile — " +
			"change the profile if it should be acceptable, rather than overriding it here; " +
			"explain_release will say which rule rejected it")
	case err != nil:
		return nil, classify(err)
	}
	return map[string]any{
		"desired_item_id": args.DesiredItemID,
		"candidate_id":    chosen.CandidateID,
		"provider":        chosen.Provider,
		"title":           chosen.Title,
		"score":           chosen.Evaluation.Score,
		"accepted":        chosen.Evaluation.Accepted,
		"status":          "selected; a grab has been queued",
	}, nil
}

// followSourceArgs is what followSource decodes (held to its schema by schemaargs_test.go).
type followSourceArgs struct {
	URL            string   `json:"url"`
	TVDBID         string   `json:"tvdb_id"`
	Type           string   `json:"type"`
	WorkID         string   `json:"work_id"`
	Title          string   `json:"title"`
	Year           int      `json:"year"`
	QualityProfile string   `json:"quality_profile"`
	Monitor        *bool    `json:"monitor"`
	Backfill       string   `json:"backfill"`
	Reason         string   `json:"reason"`
	WantSubtitles  []string `json:"want_subtitles"`
}

// followSource is §55's follow_source — the subscription intent, shared with
// POST /api/v1/followed-sources. The same "one intent, two doors" discipline as
// want_content: the source, its poll bookkeeping and its event are created
// through resources.FollowSource, so the two doors cannot drift.
func (s *Server) followSource(ctx context.Context, raw json.RawMessage) (any, error) {
	var args followSourceArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	out, err := s.resources.FollowSource(ctx, resources.FollowSourceRequest{
		URL: args.URL, TVDBID: args.TVDBID, Type: args.Type,
		WorkID: args.WorkID, Title: args.Title, Year: args.Year,
		QualityProfile: args.QualityProfile,
		Monitor:        args.Monitor, Backfill: args.Backfill, Reason: args.Reason,
		WantSubtitles: args.WantSubtitles,
	})
	if err != nil {
		return nil, classifyFollow(err)
	}
	return out, nil
}

// discoverContentArgs is what discoverContent decodes (held to its schema by schemaargs_test.go).
type discoverContentArgs struct {
	Query string `json:"query"`
}

// discoverContent is #451's discover_content — the "not-yet-in-library" search,
// shared with POST /api/v1/discover through resources.Discover so the MCP door
// and the REST door cannot drift. Unlike search_content (a raw library read that
// hits s.reader directly), discovery needs the provider registry, which lives
// behind the resource API — so it delegates there, the same as list_followed.
func (s *Server) discoverContent(ctx context.Context, raw json.RawMessage) (any, error) {
	var args discoverContentArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Query) == "" {
		return nil, invalidParams("give me a query to discover on")
	}
	out, err := s.resources.Discover(ctx, resources.DiscoverRequest{Query: args.Query})
	if err != nil {
		return nil, classifyDiscover(err)
	}
	return map[string]any{"count": len(out), "results": out}, nil
}

// classifyDiscover turns a Discover error into a JSON-RPC error, the sibling of
// classify/classifyFollow: a caller's fault (an empty query) and a "no provider
// configured" both render as invalidParams — each is quotable and actionable —
// while a provider's own call failure stays ours.
func classifyDiscover(err error) error {
	if msg, isClient := resources.DiscoverClientFault(err); isClient {
		return invalidParams("%s", msg)
	}
	return err
}

// listFollowed is §55's list_followed, shared with GET /api/v1/followed-sources.
func (s *Server) listFollowed(ctx context.Context, _ json.RawMessage) (any, error) {
	out, err := s.resources.ListFollowed(ctx)
	if err != nil {
		return nil, classifyFollow(err)
	}
	return map[string]any{"followed_sources": out}, nil
}

// unfollowArgs is what unfollow decodes (held to its schema by schemaargs_test.go).
type unfollowArgs struct {
	SourceID    string `json:"source_id"`
	KeepArchive *bool  `json:"keep_archive"`
}

// unfollow is §55's unfollow, shared with DELETE /api/v1/followed-sources/{id}.
// keep_archive defaults to true — stop polling, keep what was archived.
func (s *Server) unfollow(ctx context.Context, raw json.RawMessage) (any, error) {
	var args unfollowArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.SourceID == "" {
		return nil, invalidParams("source_id is required — the source to stop, from list_followed")
	}
	keepArchive := true
	if args.KeepArchive != nil {
		keepArchive = *args.KeepArchive
	}
	if err := s.resources.Unfollow(ctx, args.SourceID, keepArchive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, invalidParams("there is no followed source with that id")
		}
		return nil, classifyFollow(err)
	}
	return map[string]any{"source_id": args.SourceID, "status": "unfollowed; the archive was kept"}, nil
}

// pollSourceArgs is what pollSource decodes (held to its schema by schemaargs_test.go).
type pollSourceArgs struct {
	SourceID string `json:"source_id"`
}

// pollSource is poll_source, shared with POST /api/v1/followed-sources/{id}/poll
// through resources.PollSource — the same enqueue the follow door and the follow
// beat use, so an on-demand poll and a scheduled one cannot drift. It queues the
// poll and says so; an unknown id is a not-found the agent can quote.
func (s *Server) pollSource(ctx context.Context, raw json.RawMessage) (any, error) {
	var args pollSourceArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.SourceID == "" {
		return nil, invalidParams("source_id is required — the source to poll, from list_followed")
	}
	out, err := s.resources.PollSource(ctx, args.SourceID)
	if err != nil {
		return nil, classifyFollow(err)
	}
	return out, nil
}

// setSourceProfileArgs is what setSourceProfile decodes (held to its schema by schemaargs_test.go).
type setSourceProfileArgs struct {
	SourceID       string    `json:"source_id"`
	QualityProfile string    `json:"quality_profile"`
	Backfill       string    `json:"backfill"`
	WantSubtitles  *[]string `json:"want_subtitles"`
}

// setSourceProfile is set_source_profile, shared with PATCH
// /api/v1/followed-sources/{id}. It reuses the exact repoint RepointSource runs,
// so the two doors move a subscription's strategy the same way (ADR-0082).
func (s *Server) setSourceProfile(ctx context.Context, raw json.RawMessage) (any, error) {
	var args setSourceProfileArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.SourceID == "" {
		return nil, invalidParams("source_id is required — the source to repoint, from list_followed")
	}
	if args.QualityProfile == "" && args.Backfill == "" && args.WantSubtitles == nil {
		return nil, invalidParams("give quality_profile (the profile to move to, by name), " +
			"backfill (from_now or full), or want_subtitles (the subtitle languages) — a repoint must change something")
	}
	out, err := s.resources.RepointSource(ctx, args.SourceID,
		resources.RepointRequest{QualityProfile: args.QualityProfile, Backfill: args.Backfill, WantSubtitles: args.WantSubtitles})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, invalidParams("there is no followed source with that id")
		}
		return nil, classifyFollow(err)
	}
	return out, nil
}

// classifyFollow maps a follow op's error onto a JSON-RPC code the way classify
// does for the desired ops, using resources.FollowClientFault so the MCP door
// and the REST door agree about whose fault an error is.
func classifyFollow(err error) error {
	if msg, isClient := resources.FollowClientFault(err); isClient {
		return &toolError{code: codeInvalidParams, err: fmt.Errorf("%s", msg)}
	}
	return err
}
