// Package desired models DesiredItem — "this content should exist under these
// conditions" (spec §55). Milestone 3.
//
// # The row that makes Heyarr more than a catalogue
//
// Everything Milestone 1 and 2 built answers "what do I have". This answers
// "what should I have", and every other part of Milestone 3 is downstream of
// it: the state machine (§64) tracks one of these, evaluation (§63) scores
// candidates for one, and satisfaction (§56) is a question asked about one.
//
// # Wanting is not owning, so a want must survive having nothing
//
// The whole point is that a DesiredItem can name content with no Asset, no
// Blob and no bytes anywhere. That sounds obvious and it is the requirement
// most easily lost, because every fixture in the repository has assets: a
// design that only works once something exists passes every test and fails the
// first real use.
//
// So a want anchors to a WORK — the semantic entity (§11), which exists
// whether or not any bytes do — and never to an Asset, which is a file that
// exists by definition. Wanting a file you already have is not a coherent
// thing to say.
//
// # Two axes, not one
//
// Wanted and monitored are different (§60 keeps both words). "This should
// exist" and "keep looking for something better" are separate statements: an
// unmonitored want is finished the moment it is satisfied, and a monitored one
// keeps feeding the upgrade workflow. Running the upgrade loop over
// unmonitored items is how *arr installations re-download libraries nobody
// asked them to touch.
//
// # One version per title is what §61 says to avoid
//
// Two wants over the same content with different profiles — the living-room
// 2160p and the phone-sized copy — must both be expressible. The uniqueness
// constraint is therefore over (target, profile), never over the target alone.
//
// # This package touches nothing
//
// No os, no path/filepath, no database/sql, no persistence, no CAS — depguard
// enforces it (§18, ADR-0006/0007).
package desired
