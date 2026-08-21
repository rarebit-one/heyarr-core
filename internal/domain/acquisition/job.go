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

// The upgrade scan's identity (§60, M3-06).
//
// A separate job type from reconciliation, not a flag on it, because the two
// answer different questions on different schedules. Reconciliation asks "is
// this still satisfied" and must run often enough that a profile edit is
// noticed quickly. The upgrade scan asks "could this be better", which is a
// question worth asking far less often — it leads to a provider round trip and
// then to moving gigabytes, and nobody needs that considered every five
// minutes.
//
// Fusing them would tie the cheap frequent question to the expensive rare one,
// and the only way out would be a flag that made half of each run a no-op.

// UpgradeScanJobType is the job that looks for wants that could be improved.
const UpgradeScanJobType = "upgrade_scan"

// UpgradeScanDedupeKey is the queue's idempotency key.
//
// One key for the whole sweep, like reconciliation: two concurrent scans would
// each read the library while the other concluded, and the loser would spend
// the pass acting on answers that were already stale.
const UpgradeScanDedupeKey = "upgrade:scan"

// UpgradeScanPayload is what an upgrade scan carries.
type UpgradeScanPayload struct {
	// DesiredItemID scopes the scan to one want. Empty means every monitored
	// want, which is the scheduled case.
	DesiredItemID string `json:"desired_item_id,omitempty"`
}

// The search job's identity (§60, §63, invariant 4).
//
// Declared here alongside the reconciliation job, for the same reason: the
// controller enqueues it and the worker runs it, and neither should have to
// import the other.
//
// The HANDLER lands in M3-12. The type and its required capability land here
// because ADR-0025's degrade path is only demonstrable against a real job:
// "a search on a node with no indexer stays pending rather than failing" needs
// a search that can be enqueued.
const (
	// SearchJobType looks for releases that would satisfy a want.
	SearchJobType = "search_release"
)

// SearchDedupeKey is the queue's idempotency key for searching one want.
//
// Per want rather than one for everything: two wants should be searched
// concurrently, and collapsing them would make a library of two hundred wants
// take two hundred sequential searches to make one pass.
func SearchDedupeKey(desiredItemID string) string { return "search:" + desiredItemID }

// SearchPayload is what a search job carries.
type SearchPayload struct {
	DesiredItemID string `json:"desired_item_id"`
}

// IngestJobType brings a completed acquisition under management (§65, M3-13).
//
// A separate job from poll_downloads, and deliberately so. Polling asks a
// download client what it is doing and must stay quick — it runs on a beat
// over every transfer. Ingesting hashes a file that may be 40 GB and then
// materialises it. Doing that inside the poll would mean one large acquisition
// stalls the progress reporting for every other one, and a lease sized for a
// poll would expire in the middle of a hash.
//
// It is also the only shape in which invariant 1 is affordable. Heyarr hashes
// what arrived rather than trusting the client's claim, and a hash is real work
// on real bytes — the kind of work jobs exist for (§75).
const IngestJobType = "ingest_acquisition"

// IngestDedupeKey makes the job idempotent per want.
//
// Keyed on the WANT rather than on the transfer, because the thing that must
// not happen twice is one want ingesting twice. Two transfers for one want is
// already impossible — acquisitions is UNIQUE on desired_item_id (00016) — so
// keying on the transfer would add a second identity for the same constraint
// and let a re-queued transfer slip past the first.
func IngestDedupeKey(desiredItemID string) string { return "ingest-acq:" + desiredItemID }

// IngestPayload is what an ingest_acquisition job carries.
type IngestPayload struct {
	// DesiredItemID is the want whose acquisition completed. Everything else —
	// the transfer, its path, the selected candidate — is read from the
	// database at handling time rather than carried here.
	//
	// A payload that carried the path would be a payload that goes stale: the
	// poll job may have re-resolved it since, and a job that ran against a
	// path recorded five minutes ago is a job operating on a guess.
	DesiredItemID string `json:"desired_item_id"`
}
