package followed

import (
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
)

// When to ask a source what it now has (M12).
//
// The search beat decides when to look for a WANT; the follow beat decides when
// to ask a SOURCE what items it now emits. They are siblings, and the follow
// beat feeds the search beat: its only output is DesiredItems.
//
// # The tick is granularity, not cadence
//
// A followbeat controller ticks often (~30s) and asks "which sources are DUE",
// exactly as the search beat does. The cadence a feed host actually feels is a
// source's next_poll_at, which is this policy — hours, not seconds. Shortening
// the tick makes Heyarr notice a due source sooner; it does not poll anything
// more often.
//
// # Why this reuses acquisition.Schedule rather than growing its own
//
// The search schedule already solved the two hard parts and had them tested:
// an exponential backoff capped at a ceiling (a source that keeps emitting
// nothing is asked less often, never abandoned), and a DETERMINISTIC per-key
// spread so a batch of follows created in one import does not thundering-herd a
// feed host on the same tick forever (ADR-0017 — "why did this poll at 03:14"
// has an answer). A followed source pointed at a feed host has the same shape of
// cost and the same need for the same spread, so it reuses the same value type
// and the same NextAt rather than duplicating a second copy of a backoff that
// would then have to be kept in step.

// feedPollBase is the delay after a poll that found no new items. Conservative:
// a feed host is a third party, and a source that emits on a human timescale —
// a weekly episode, a daily article — is not made more current by being asked
// every few minutes. Six hours is frequent enough that a new episode is noticed
// the same day and slow enough to be a polite guest on someone else's server.
const feedPollBase = 6 * time.Hour

// feedPollMax is the ceiling the backoff walks toward for a source that has
// gone quiet — a series between seasons, a dormant channel. A day: quiet is not
// gone, and next month's return must still be noticed within a day of it.
const feedPollMax = 24 * time.Hour

// FeedPoll is the cadence policy for polling a followed source's feed.
//
// The "fruitless" count NextAt takes is consecutive polls that discovered no
// new item, so a source between seasons is asked progressively less often and a
// source that just emitted resets to the floor. The spread key is the source
// id, so two sources pointed at the same host land in different slots.
func FeedPoll() acquisition.Schedule {
	return acquisition.Schedule{Name: "feed-poll", Base: feedPollBase, Max: feedPollMax}
}

// PollSourceJobType is the worker job a followbeat controller enqueues to run
// one feed round-trip. It is a bare string here, next to the schedule, so a
// controller can enqueue it without importing the worker — exactly as
// acquisition.ReconcileJobType and scanner.JobType are declared away from their
// handlers.
const PollSourceJobType = "poll_source"

// PollDedupeKey is the queue's idempotency key for a source's poll, in the
// colon-separated shape acquisition's keys use (reconcile:desired,
// upgrade:scan). Two controllers, or a controller and an operator forcing a
// poll at the same moment, produce ONE poll — the same partial-unique-index
// mechanism ADR-0008 gives the search beat.
func PollDedupeKey(sourceID string) string {
	return "poll_source:" + sourceID
}
