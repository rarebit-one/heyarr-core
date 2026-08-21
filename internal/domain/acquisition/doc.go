// Package acquisition owns the acquisition state machine and release-candidate
// evaluation (spec §63, §64). Milestone 3.
//
// # §64 is a linear presentation of three independent things
//
// The spec draws twelve boxes in a column:
//
//	MISSING → SEARCHING → CANDIDATES_FOUND → SELECTED → QUEUED → DOWNLOADING
//	→ VERIFYING → INGESTING → AVAILABLE → CONTENT_SATISFIED
//	→ PLACEMENT_CONVERGING → FULLY_SATISFIED
//
// They are not twelve steps along one road. The first nine are positions in an
// acquisition PIPELINE; the last three are the same pipeline position —
// "the bytes are here" — annotated with the answers to two independent
// questions §56 says to evaluate separately:
//
//	content satisfied    do we have bytes that the quality profile accepts?
//	placement satisfied  are those bytes on every Full Peer that should hold them?
//
// So this package stores a PHASE and two SATISFACTION axes, and derives §64's
// name from the three. ADR-0027 records why, because the tidy alternative — one
// ordinal column counting 0..11 — will look correct to the next reader and is
// exactly how CONTENT_SATISFIED and FULLY_SATISFIED collapse into each other.
// That collapse is the one outcome the milestone epic explicitly names.
//
// The collapse is not hypothetical. Obtaining a file and replicating it are
// different work, done by different subsystems, at different times, and either
// can regress without the other: a peer can go away long after the bytes
// arrived. An ordinal cannot express "content is fine and placement went
// backwards" without moving backwards through states that mean something else.
//
// # Nothing here is terminal, and that is deliberate
//
// ConsumptionSession (ADR-0024) is the precedent for the machine's shape — a
// pure transition table, tested for what is legal AND what is not — but it has
// terminal states and this does not. AVAILABLE is not an end: a monitored
// DesiredItem re-enters SEARCHING when something better might exist (§60's
// upgrade workflow), and an asset can go missing. Whether a want is "finished"
// is a question about the WANT (is it monitored, is its profile terminal), not
// about this machine, and answering it here would put policy in the pipeline.
//
// # Every edge emits
//
// Invariant 7, with no exceptions, and the event goes inside the transaction
// that wrote the row. An event that can exist without its row, or a row
// without its event, is the bug the invariant exists to prevent. The tests
// enumerate the transition table rather than listing edges by hand, so an edge
// added without an event fails rather than being noticed later.
//
// # This package touches nothing
//
// No os, no path/filepath, no database/sql, no persistence, no CAS — depguard
// enforces it (§18, ADR-0006/0007).
package acquisition
