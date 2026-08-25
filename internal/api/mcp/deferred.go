package mcp

// The verbs §71 lists that this milestone does NOT carry.
//
// # Why they are absent rather than stubbed
//
// A tool that answers "not implemented" is worse than a missing tool. A missing
// tool is a vocabulary an agent can grow into; a stubbed one is a published
// promise with a hole in it — and ADR-0019 waited five milestones precisely to
// avoid publishing a vocabulary that would then have to change.
//
// They are recorded HERE, in code, rather than only in a document, for one
// reason: an agent that asks for a deferred verb gets told which milestone
// brings it, instead of being told it mistyped. That is the difference between
// an agent that waits and one that retries forever.
//
// It is also what makes the "every §71 verb is shipped or explicitly deferred"
// test enumerable rather than a promise — see the coverage test.

// deferral explains one absent verb.
type deferral struct {
	// Milestone is where the capability behind it arrives.
	Milestone string `json:"milestone"`
	// Reason is why it cannot exist yet, in terms of what is missing rather
	// than what is unfinished.
	Reason string `json:"reason"`
}

// deferredTools is every §71 verb this server does not implement.
//
// # It rots, and that is the failure mode to watch for
//
// This table is prose maintained by hand, and NOTHING FAILS when the world
// moves past it. The coverage test asserts every §71 verb is shipped or
// explicitly deferred, which stays green when a verb is deferred for a reason
// that has stopped being true — and an entry that is WRONG produces an agent
// that waits for a milestone which already shipped, which is worse than the
// missing tool this whole mechanism exists to avoid.
//
// It happened: search_releases and acquire_release were deferred here for
// "there is no search job" and "no download client is wired" long after both
// were false, and they were removed by shipping the verbs (#226). When a
// deferral's reason stops describing the world, the answer is to ship the verb
// or to write the true reason — never to leave the old one because the
// conclusion still happens to hold.
var deferredTools = map[string]deferral{
	"play_content": {
		Milestone: "M4+",
		Reason: "playback is device-mediated: it returns a credentialed URL scoped to a " +
			"registered device, and §71 pairs it with transfer_playback, which is not " +
			"built. Shipping half of a device-control vocabulary is what ADR-0019 waited " +
			"to avoid — and Milestone 2 refuses every plan that is not DIRECT, so the " +
			"verb would mostly refuse",
	},
	"transfer_playback": {
		Milestone: "M4+",
		Reason:    "moving a playback between devices is not built",
	},
}

// deferralFor returns the explanation for an absent §71 verb, or nil when the
// name is simply unknown. Nil matters: an agent that mistyped should not be
// told to wait for a milestone.
func deferralFor(name string) any {
	d, ok := deferredTools[name]
	if !ok {
		return nil
	}
	return map[string]any{
		"deferred":  true,
		"milestone": d.Milestone,
		"reason":    d.Reason,
	}
}

// instructions is what a client shows an agent about this server.
//
// It states the two boundaries that an agent cannot infer from the tool list:
// what this server deliberately cannot see, and that the reasons in a result
// are the point rather than decoration.
const instructions = `Heyarr manages a content library: what exists, what SHOULD exist, and why.

These tools are semantic actions over desired state, not a browsable API. Prefer
them to guessing: want_content creates desire, get_missing_content says what is
unmet, and explain_release says WHY a release was or was not good enough.

Results carry reasons with stable rule codes. When something is rejected, the
reason names the rule that rejected it — quote that to the person you are
helping rather than summarising it, because the code is what they can act on.

This server cannot see personal state. Playlists, ratings, reading positions and
history are encrypted and controller-side MCP holds no key for them. If you are
asked about those, say they are not reachable from here rather than guessing.`
