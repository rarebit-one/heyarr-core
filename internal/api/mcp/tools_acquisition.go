package mcp

import "github.com/rarebit-one/heyarr-core/internal/auth"

// registerAcquisitionTools registers the want lifecycle: declaring and
// monitoring wants, searching for and choosing releases, and the reads that
// explain where a want stands.
func (s *Server) registerAcquisitionTools() {
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
		Name:     "get_missing_content",
		Title:    "What is not satisfied",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "List wants whose content is not satisfied — either nothing is held, " +
			"or what is held does not meet the profile. Use get_content_satisfaction on " +
			"one of them to find out which, and why.",
		InputSchema: schemaLimit,
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
		InputSchema: schemaLimit,
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
		Name:     "get_acquisition_status",
		Title:    "What a want is downloading right now",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "Report where a want is in the acquisition pipeline (idle, searching, " +
			"selected, queued, downloading, verifying, ingesting) and, when a download is in " +
			"flight, the transfer behind it: which release was chosen — its name carries the " +
			"resolution and size — how far it has downloaded, and any trouble the client " +
			"reported. This is the read for \"what is this want actually doing\" and \"why is " +
			"it still not here\", without opening the download client.",
		InputSchema: schemaDesiredItemID,
		Handler:     s.getAcquisitionStatus,
	})

	s.tools.register(Tool{
		Name:     "list_jobs",
		Title:    "Inspect the durable work queue",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "List the durable jobs Heyarr runs — searches, grabs, download polls, " +
			"ingests — filtered by state and/or type, most recent first, each with its last " +
			"error. This is the read behind \"why is nothing being acquired\": a search that " +
			"found nothing, a grab the download client refused, a poll that failed. A `failed` " +
			"job will retry with backoff; a `dead` one is terminal until an operator retries it.",
		InputSchema: schemaListJobs,
		Handler:     s.listJobs,
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
}
