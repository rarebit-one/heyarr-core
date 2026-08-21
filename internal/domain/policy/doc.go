// Package policy models quality profiles and placement/durability policy
// (spec §62, §34). Milestone 3.
//
// # Three semantics, not one ranking
//
// §62 gives a profile three sections, and they are three different KINDS of
// statement rather than three degrees of one:
//
//	accept    a GATE.  Fail it and the candidate is rejected outright,
//	                   whatever else it offers.
//	prefer    a SCORE. Never a gate. A candidate that meets no preference
//	                   at all is still acceptable, merely worse.
//	terminal  a STOP.  The point at which the upgrade workflow has nothing
//	                   left to want, and stops looking.
//
// Collapse any two of them and you get the *arr behaviour §61 tells us to
// avoid. Fold `accept` into `prefer` and you have a preference that silently
// rejects. Fold `terminal` into `prefer` — treat it as "the highest
// preference" — and the upgrade loop has no ceiling and never terminates,
// because "better" is unbounded and there is always a bigger file.
//
// `terminal` is the one most likely to be mis-modelled, because on a casual
// reading it looks like the top of the scale. It is not a score at all. It is
// a predicate, and a profile with no terminal rules is legal and means "never
// stop looking" — which is a real thing to want and must be expressible
// without a sentinel.
//
// # A profile does not know how to score
//
// Evaluating a candidate against a profile is §63's job and lives in
// internal/domain/acquisition. A profile that knew how to score would be a
// profile you cannot table-test without inventing candidates, and a scorer
// that lived here would be one you cannot test without inventing profiles.
// This package owns the vocabulary — what may be asserted about a release —
// and the validation of an assertion. Nothing else.
//
// # This package touches nothing
//
// No os, no path/filepath, no database/sql, no persistence, no CAS — depguard
// enforces it (§18, ADR-0006/0007).
package policy
