package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/api/resources"
	"github.com/rarebit-one/heyarr-core/internal/auth"
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
// The split between this file and the handlers is deliberate: this is the
// VOCABULARY, readable in one screen, and reviewing the authorisation surface
// means reading the Scope column here rather than opening nine handlers.

func (s *Server) registerTools() {
	// The renderer lane (§68), in its own file: four verbs about the
	// physical world rather than about the catalog.
	s.registerRendererTools()

	s.tools.register(Tool{
		Name:     "search_content",
		Title:    "Search the library",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "Find works already in the library by title. Use this to resolve " +
			"what someone means before wanting it — a work found here can be wanted by " +
			"id, which is exact, rather than by description, which may create a second " +
			"work if it does not match what a scan would have produced.",
		InputSchema: schemaSearchContent,
		Handler:     s.searchContent,
	})

	s.tools.register(Tool{
		Name:     "get_external_ids",
		Title:    "Get external identifiers",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "Resolve external catalogue identifiers (tmdb, imdb) for a work or " +
			"edition, or reverse a source+value back to the work or edition that carries " +
			"it. Use this to reconcile an outside id to a heyarr work_id and back by id " +
			"rather than by a fuzzy title. Read-only; an unknown id returns an empty list.",
		InputSchema: schemaGetExternalIDs,
		Handler:     s.getExternalIDs,
	})

	s.tools.register(Tool{
		Name:     "want_content",
		Title:    "Want content",
		Scope:    auth.ScopeWrite,
		ReadOnly: false,
		Description: "Declare that content SHOULD exist under a quality profile, whether " +
			"or not it does. This is the central action: it works for content the library " +
			"has never seen, creating the work from a description. Name the profile the " +
			"way a person would — \"living-room\" — not by id.",
		InputSchema: schemaWantContent,
		Handler:     s.wantContent,
	})

	s.tools.register(Tool{
		Name:     "monitor_content",
		Title:    "Keep looking, or stop",
		Scope:    auth.ScopeWrite,
		ReadOnly: false,
		Description: "Turn monitoring on or off for a want. Monitoring is NOT the same as " +
			"wanting: an unmonitored want that is satisfied is finished, while a monitored " +
			"one keeps looking for something better. Turn it off when someone says \"this " +
			"copy is fine, stop\".",
		InputSchema: schemaMonitorContent,
		Handler:     s.monitorContent,
	})

	s.tools.register(Tool{
		Name:     "search_releases",
		Title:    "Look for releases now",
		Scope:    auth.ScopeWrite,
		ReadOnly: false,
		Description: "Ask the indexers for releases that would satisfy a want, now, " +
			"rather than waiting for its next scheduled search. It QUEUES the search and " +
			"returns a job — an indexer can take thirty seconds to refuse, so nothing " +
			"holds while it runs. Read the want back afterwards to see what was found " +
			"and what was chosen.",
		InputSchema: schemaSearchReleases,
		Handler:     s.searchReleases,
	})

	s.tools.register(Tool{
		Name:     "acquire_release",
		Title:    "Acquire a particular release",
		Scope:    auth.ScopeWrite,
		ReadOnly: false,
		Description: "Choose one specific release for a want and start fetching it — " +
			"§60's manual override. Use it when someone names the release they want " +
			"rather than letting the scorer decide. A candidate the quality profile " +
			"REJECTED is refused, and the refusal names the rule: change the profile if " +
			"it should be acceptable, rather than overriding it here.",
		InputSchema: schemaAcquireRelease,
		Handler:     s.acquireRelease,
	})

	s.tools.register(Tool{
		Name:     "follow_source",
		Title:    "Follow a source",
		Scope:    auth.ScopeWrite,
		ReadOnly: false,
		Description: "Subscribe to a source and archive everything it emits, forever — a " +
			"STANDING subscription, distinct from want_content, which gets one thing once. " +
			"Give a content intent (which series or podcast) and an identity (a url, or a " +
			"tvdb_id); the type is inferred, you never name a source or a provider. A TVDB id " +
			"or URL is a TV series; any other http(s) feed URL is a podcast.",
		InputSchema: schemaFollowSource,
		Handler:     s.followSource,
	})

	s.tools.register(Tool{
		Name:     "list_followed",
		Title:    "What is followed",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "List every followed source with how many items its feed has yielded, " +
			"how many are archived, when it was last polled and when it is due next, and " +
			"whether its feed adapter is healthy. Read this to answer \"is this working\".",
		InputSchema: schemaNoArgs,
		Handler:     s.listFollowed,
	})

	s.tools.register(Tool{
		Name:     "unfollow",
		Title:    "Stop following a source",
		Scope:    auth.ScopeWrite,
		ReadOnly: false,
		Description: "Stop a subscription. By default it stops future polls and KEEPS every " +
			"episode already archived (keep_archive true). Phase 1 always keeps the archive; " +
			"asking to remove it is refused.",
		InputSchema: schemaUnfollow,
		Handler:     s.unfollow,
	})

	s.tools.register(Tool{
		Name:     "get_missing_content",
		Title:    "What is not satisfied",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "List wants whose content is not satisfied — either nothing is held, " +
			"or what is held does not meet the profile. Use get_content_satisfaction on " +
			"one of them to find out which, and why.",
		InputSchema: schemaNoArgs,
		Handler:     s.getMissingContent,
	})

	s.tools.register(Tool{
		Name:     "get_upgrade_candidates",
		Title:    "What could be better",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "List wants that are satisfied and could still be improved — " +
			"monitored, holding acceptable content, and not yet at the profile's terminal " +
			"condition. A want being here does not mean a better release exists, only " +
			"that nothing about its state rules one out.",
		InputSchema: schemaNoArgs,
		Handler:     s.getUpgradeCandidates,
	})

	s.tools.register(Tool{
		Name:     "get_content_satisfaction",
		Title:    "Why a want is or is not met",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "Explain one want: whether the library holds bytes the profile " +
			"accepts, whether those bytes are on every peer that should hold them, and " +
			"WHICH RULE rejected each asset that did not qualify. This is the tool to " +
			"reach for when someone says \"I have this, why does Heyarr say it is missing\".",
		InputSchema: schemaDesiredItemID,
		Handler:     s.getContentSatisfaction,
	})

	s.tools.register(Tool{
		Name:     "explain_release",
		Title:    "Explain a release against a profile",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "Score one or more releases against a quality profile and return the " +
			"reasons — every rule considered, whether it passed, failed, scored, missed or " +
			"could not be determined. Writes nothing, so it is safe to use for answering " +
			"\"would this be accepted?\" before anything is acquired. An attribute left out " +
			"is reported as undetermined rather than as a failure, which is a different " +
			"thing and sends you to a different place.",
		InputSchema: schemaExplainRelease,
		Handler:     s.explainRelease,
	})

	s.tools.register(Tool{
		Name:     "get_peer_status",
		Title:    "Peer status",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "List the peers this instance knows about and what each is for. " +
			"A single peer is a supported deployment rather than a symptom — most " +
			"Heyarr installations are one machine — so do not report a fabric of one " +
			"as a replication fault. Ask get_content_satisfaction whether placement " +
			"is answering a real question: it reports `unproven` when the target set " +
			"is this node alone.",
		InputSchema: schemaNoArgs,
		Handler:     s.getPeerStatus,
	})

	s.tools.register(Tool{
		Name:     "get_replica_status",
		Title:    "Where bytes are",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "Report which peers hold a blob and whether their copy is verified. " +
			"A copy that is pending or corrupt is NOT a copy for placement purposes.",
		InputSchema: schemaBlobHash,
		Handler:     s.getReplicaStatus,
	})

	s.tools.register(Tool{
		Name:     "sync_peer",
		Title:    "Reconcile against a peer",
		Scope:    auth.ScopeWrite,
		ReadOnly: false,
		Description: "Ask this instance to reconcile the desired blob set against what " +
			"a peer is known to hold, now, rather than waiting for the next scheduled " +
			"cycle. Queues work rather than doing it: the diff enqueues a transfer per " +
			"gap and the bytes move afterwards, so this reply says only that the cycle " +
			"was accepted. It is the on-demand half of the reconciliation §57 asks for " +
			"on a beat and on demand — useful straight after enrolling a peer or " +
			"restoring one, and pointless to call in a loop.",
		InputSchema: schemaSyncPeer,
		Handler:     s.syncPeer,
	})

	s.tools.register(Tool{
		Name:     "verify_blob",
		Title:    "Re-verify stored bytes",
		Scope:    auth.ScopeWrite,
		ReadOnly: false,
		Description: "Queue a re-read of a blob's bytes to confirm they still hash to " +
			"what the catalog recorded. Queues work rather than doing it — the answer " +
			"arrives on the job, not in this reply — because re-hashing a large file is " +
			"not something to hold a request open for.",
		InputSchema: schemaBlobHash,
		Handler:     s.verifyBlob,
	})
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

// wantContent is the write intent, shared with POST /api/v1/desired.
//
// The whole body of this function is the argument shape and the delegation.
// That is the point: the intent lives in resources.WantContent, and both doors
// call it, so the acquisition row, the two events and the immediate
// reconciliation cannot happen through one door and not the other.
func (s *Server) wantContent(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		WorkID         string `json:"work_id"`
		Title          string `json:"title"`
		ContentType    string `json:"content_type"`
		Year           int    `json:"year"`
		QualityProfile string `json:"quality_profile"`
		Monitor        *bool  `json:"monitor"`
		Reason         string `json:"reason"`
	}
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

	item, err := s.resources.WantContent(ctx, req)
	if err != nil {
		return nil, classify(err)
	}
	return item, nil
}

// monitorContent is the other write intent, shared with PATCH /desired/{id}.
func (s *Server) monitorContent(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		DesiredItemID string `json:"desired_item_id"`
		Monitor       *bool  `json:"monitor"`
	}
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

// searchReleases is §71's search_releases.
//
// It queues the search rather than performing it, and says so, because a search
// is a job (invariant 4) that a different process may run and an indexer may
// take thirty seconds to refuse. An agent that needs the answer reads the
// want's candidates afterwards — get_content_satisfaction explains what is
// held, and explain_release scores what was offered.
func (s *Server) searchReleases(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		DesiredItemID string `json:"desired_item_id"`
	}
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

// acquireRelease is §71's acquire_release.
//
// It is §60's manual override reached by an agent: select this candidate, and
// arrange for it to be fetched. It refuses a candidate the quality profile
// rejected — an agent that could override a gate would turn the operator's own
// statement of what is acceptable into a suggestion, and the reason it gets
// back names the rule so it can say WHY rather than that it failed.
func (s *Server) acquireRelease(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		DesiredItemID string `json:"desired_item_id"`
		CandidateID   string `json:"candidate_id"`
	}
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

// followSource is §55's follow_source — the subscription intent, shared with
// POST /api/v1/followed-sources. The same "one intent, two doors" discipline as
// want_content: the source, its poll bookkeeping and its event are created
// through resources.FollowSource, so the two doors cannot drift.
func (s *Server) followSource(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		URL            string `json:"url"`
		TVDBID         string `json:"tvdb_id"`
		WorkID         string `json:"work_id"`
		Title          string `json:"title"`
		Year           int    `json:"year"`
		QualityProfile string `json:"quality_profile"`
		Monitor        *bool  `json:"monitor"`
		Backfill       string `json:"backfill"`
		Reason         string `json:"reason"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	out, err := s.resources.FollowSource(ctx, resources.FollowSourceRequest{
		URL: args.URL, TVDBID: args.TVDBID,
		WorkID: args.WorkID, Title: args.Title, Year: args.Year,
		QualityProfile: args.QualityProfile,
		Monitor:        args.Monitor, Backfill: args.Backfill, Reason: args.Reason,
	})
	if err != nil {
		return nil, classifyFollow(err)
	}
	return out, nil
}

// listFollowed is §55's list_followed, shared with GET /api/v1/followed-sources.
func (s *Server) listFollowed(ctx context.Context, _ json.RawMessage) (any, error) {
	out, err := s.resources.ListFollowed(ctx)
	if err != nil {
		return nil, classifyFollow(err)
	}
	return map[string]any{"followed_sources": out}, nil
}

// unfollow is §55's unfollow, shared with DELETE /api/v1/followed-sources/{id}.
// keep_archive defaults to true — stop polling, keep what was archived.
func (s *Server) unfollow(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		SourceID    string `json:"source_id"`
		KeepArchive *bool  `json:"keep_archive"`
	}
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

// classifyFollow maps a follow op's error onto a JSON-RPC code the way classify
// does for the desired ops, using resources.FollowClientFault so the MCP door
// and the REST door agree about whose fault an error is.
func classifyFollow(err error) error {
	if msg, isClient := resources.FollowClientFault(err); isClient {
		return &toolError{code: codeInvalidParams, err: fmt.Errorf("%s", msg)}
	}
	return err
}
