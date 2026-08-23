// Package manifests owns chunk manifests. A manifest is an optimisation and never an identity (spec §15, ADR-0034). Milestone 5.
//
// ADR-0034 is the record to read first: a manifest is keyed by the blob's
// whole-object digest, is never the key of anything, and may be discarded at
// any time with no loss of correctness. ADR-0035 makes a verified manifest
// chunk the unit of a resumed transfer, and ADR-0036 makes it the unit of a
// repair fetch — in both cases the destination still verifies the assembled
// whole-object digest itself.
package manifests
