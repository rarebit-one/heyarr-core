package acquisition

// The reconciliation job's identity (§57, invariant 4, ADR-0002).
//
// Reconciliation is a JOB and not an in-process callback, and not a hook on
// ingest. Roles communicate only through the job table and HTTP (ADR-0002),
// even inside `heyarr all` — and a satisfaction answer that only existed as a
// side effect of an ingest would never be recomputed when the thing that
// changed was a quality profile or a peer going away (§57).
//
// Declared in the domain rather than in the worker so that the controller can
// enqueue it without importing the worker, exactly as scanner.JobType and
// integrity.VerifyJobType are.

// ReconcileJobType is the job that evaluates §56's two axes for every want.
const ReconcileJobType = "reconcile_desired"

// ReconcileDedupeKey is the queue's idempotency key.
//
// There is ONE key for the whole sweep rather than one per want, because two
// concurrent sweeps would each read the library while the other wrote its
// conclusions, and the loser would spend the pass recording answers that were
// already stale. A sweep already queued or running is the same sweep.
const ReconcileDedupeKey = "reconcile:desired"

// ReconcilePayload is what a reconciliation job carries.
type ReconcilePayload struct {
	// DesiredItemID scopes the sweep to one want. Empty means every want,
	// which is the scheduled case.
	//
	// The scoped form exists because a want that was just created should not
	// wait for the next full pass to find out whether it is already satisfied
	// — an operator who wants something they already own should see that
	// immediately rather than in five minutes.
	DesiredItemID string `json:"desired_item_id,omitempty"`
}
