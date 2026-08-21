package downloads

// The poll job's identity (§58, invariant 4, ADR-0002).
//
// Heyarr does not hold a long-lived connection to a download client and does
// not learn about progress through a callback. It enqueues a durable, leased,
// idempotent job that asks where things are and writes what it learnt.
//
// §61 warns against polling being the ONLY integration model, not against it
// being Milestone 3's. An event-driven completion hook is an addition on top of
// this, not a replacement for it: a hook that fires while Heyarr is restarting
// is a hook whose event is lost, and something has to reconcile afterwards
// regardless.
//
// Declared in this package rather than in the worker so the controller can
// enqueue it without importing the worker, exactly as scanner.JobType and
// acquisition.ReconcileJobType are.
const (
	// PollJobType asks every configured download client what it is doing.
	PollJobType = "poll_downloads"

	// PollDedupeKey is the queue's idempotency key.
	//
	// ONE key for the whole pass rather than one per acquisition. Two
	// concurrent passes would each read the client's queue while the other
	// wrote its conclusions, and the loser would record progress that was
	// already stale. A pass already queued or running is the same pass.
	PollDedupeKey = "poll-downloads"
)
