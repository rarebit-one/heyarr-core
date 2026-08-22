package acquisition

import (
	"hash/fnv"
	"time"
)

// When to look, unprompted (#130).
//
// The search JOB works and is idempotent; what was missing is anything that
// decides when to run one. A want in MISSING with no search in flight sat
// forever, and — this is the actual defect — looked exactly like a want that
// was being worked on.
//
// This file is the policy half and it is deliberately PURE: no database, no
// queue, no clock of its own. Everything below is a function of a state, a
// count and a time, which is what makes "a want that keeps finding nothing is
// searched less often" a table test rather than a thing to watch a log for.
//
// # Two schedules, not one schedule with a flag
//
// A want that Heyarr does not hold and a want that is already satisfied both
// want searching, and they want it on cadences that differ by more than an
// order of magnitude. Expressing that as one interval with an `isUpgrade`
// multiplier reads tidily and then loses: every subsequent policy question —
// how far to back off, what ceiling to stop at, whether to search at all while
// indexers are down — has a different answer on each side, and each one adds
// another branch to the same function until the flag is a schedule wearing a
// disguise.
//
// So there are two Schedule VALUES. The mapping from a want's state to one of
// them is ScheduleFor, and it is the only place that decision is made.
//
// This is the same argument the upgrade scan already won against
// reconciliation (see UpgradeScanJobType): two questions, two costs, two
// cadences, two job types.

// Schedule is one cadence policy: how soon to search, and how quickly to give
// a want that keeps finding nothing more room.
type Schedule struct {
	// Name identifies the schedule in storage and in logs. It is stored, so
	// changing a name is a migration and not a rename.
	Name string
	// Base is the delay after a search that changed nothing.
	Base time.Duration
	// Max is the ceiling the backoff walks toward. A want never stops being
	// searched: the interval grows until it reaches Max and then stays there,
	// because a release that does not exist today may exist next month and a
	// want that has been abandoned by the scheduler is a want that silently
	// stops meaning anything.
	Max time.Duration
}

// The two schedules.
//
// Functions rather than package variables so nothing can reach in and retune
// the policy at runtime: a cadence that one caller can change is a cadence no
// reader can rely on.

// MissingSearches is the cadence for a want whose content does not satisfy —
// §64's MISSING, and also its AVAILABLE (bytes held, not good enough), because
// to a searcher those mean the same thing: keep looking for the real answer.
//
// # Why an hour, and why an hour is the aggressive end
//
// The cost of asking too often is not CPU. An indexer proxies trackers that
// have their own rate limits and their own patience, and the thing that breaks
// is somebody's tracker ACCOUNT — which is not a resource Heyarr can restore
// by restarting. That asymmetry decides the number: being an hour late to
// notice a release costs an hour, and being banned costs the library.
//
// An hour is chosen against the alternatives rather than picked. Minutes is
// what an *arr installation with a hundred wants does to a tracker, and it
// buys nothing: releases appear on a human timescale. A day is slow enough
// that "I wanted this last night and nothing happened" is the first thing an
// operator sees. An hour, decaying, is the compromise, and it is the FLOOR:
// nothing in this package ever searches one want more often than this.
func MissingSearches() Schedule {
	return Schedule{Name: "missing", Base: time.Hour, Max: 24 * time.Hour}
}

// UpgradeSearches is the cadence for a monitored want that is already
// satisfied — §64's CONTENT_SATISFIED, which §60 says may still want a better
// copy.
//
// A day at its fastest, two weeks at its slowest, against MISSING's hour and
// day. The gap is the point and it is the same gap the upgrade SCAN already
// takes against reconciliation: the question "could this be better" leads to a
// provider round trip and then, if the answer is yes, to moving gigabytes over
// somebody's home connection. Nobody needs that considered hourly. A want that
// is satisfied is, by definition, already doing its job.
func UpgradeSearches() Schedule {
	return Schedule{Name: "upgrade", Base: 24 * time.Hour, Max: 14 * 24 * time.Hour}
}

// Schedules is every schedule, for tests and for anything that needs to
// enumerate the policy rather than assume it.
func Schedules() []Schedule { return []Schedule{MissingSearches(), UpgradeSearches()} }

// ScheduleFor answers which schedule — if any — a want is on.
//
// This is the whole state-to-policy mapping, in one place. Nothing else may
// decide it, for the reason ADR-0027 gives about the §64 name: the moment two
// places branch on "is this an upgrade", the two schedules have a flag in
// front of them again.
//
// Returns false when the want should not be searched at all:
//
//   - Something is IN FLIGHT. A search now would race the acquisition this
//     want already started, and the search handler would refuse it anyway —
//     enqueueing work whose only outcome is a logged refusal is a way to make
//     the queue lie about how much is happening.
//   - The want is satisfied and NOT monitored. Monitoring is "keep looking for
//     something better", which is not the same as wanting (§60). An
//     unmonitored want that is satisfied is finished; searching it is how an
//     *arr installation re-downloads a library nobody asked it to touch.
func ScheduleFor(s State, monitored bool) (Schedule, bool) {
	if s.Phase.InFlight() {
		return Schedule{}, false
	}
	if s.Content != SatisfactionSatisfied {
		// Includes SatisfactionUnknown — nobody has looked yet, which is the
		// state a want is created in and the one most urgently wanting an
		// answer.
		return MissingSearches(), true
	}
	if !monitored {
		return Schedule{}, false
	}
	return UpgradeSearches(), true
}

// maxDoublings bounds the shift so the backoff cannot overflow a Duration on a
// want that has been fruitless for years.
const maxDoublings = 20

// Delay is how long to wait before the next search, after `fruitless`
// consecutive searches that left this want where it was.
//
// Exponential, doubling, capped at Max.
//
// # Why the backoff is across calls and not within one
//
// The indexer client already backs off WITHIN a call (#101): a 429 or a
// timeout is retried against that indexer with a growing pause. That is a
// different problem. Nothing there notices that this is the ninetieth time
// this month Heyarr has asked four indexers for a film that has not been
// released, and each of those asks was a perfectly successful, perfectly
// polite, entirely pointless request.
//
// A release that does not exist yet is the COMMON case, not an error — which
// is why the search job returns nil on finding nothing (see search.go) rather
// than failing into the queue's retry backoff. The queue's backoff is for work
// that went wrong. This one is for work that went right and found nothing,
// which is a want, not a fault, and needs its own decay.
func (s Schedule) Delay(fruitless int) time.Duration {
	if fruitless < 0 {
		fruitless = 0
	}
	d := s.Base << min(fruitless, maxDoublings)
	if d <= 0 || d > s.Max {
		return s.Max
	}
	return d
}

// NextAt is when a want should next be searched, given when it was just
// searched and how many consecutive searches have changed nothing.
//
// # The spread, and why it is derived rather than random
//
// Wants arrive in batches — an operator imports a watchlist, or a library
// scan creates forty of them in one second. Without a spread every one of
// those forty is due at the same instant forever, and the scheduler delivers
// them to the indexer as a burst. Bursts are what rate limiters are built to
// notice.
//
// So each want gets a stable offset of up to a quarter of its own delay,
// derived from its id. Derived and not random for the reason ADR-0017 gives
// about determinism: the same want on the same schedule always lands in the
// same slot, which is reproducible in a test and debuggable in production —
// "why did this one search at 03:14" has an answer. A random offset would
// re-roll on every pass, which spreads just as well and can never be
// explained.
func (s Schedule) NextAt(now time.Time, fruitless int, spreadKey string) time.Time {
	d := s.Delay(fruitless)
	return now.Add(d + spread(spreadKey, d))
}

// spread is the deterministic per-want offset, in [0, d/4).
func spread(key string, d time.Duration) time.Duration {
	if key == "" || d <= 0 {
		return 0
	}
	h := fnv.New64a()
	// Hash writes never fail.
	_, _ = h.Write([]byte(key))
	const buckets = 1024
	// int64 throughout, and the product cannot overflow: the widest quarter is
	// a quarter of the upgrade ceiling (3.5 days, ~3e14 ns) and the widest
	// bucket is 1023, so the product tops out four orders of magnitude below
	// the int64 ceiling.
	quarter := int64(d / 4)
	bucket := int64(h.Sum64() % buckets)
	return time.Duration(quarter * bucket / buckets)
}
