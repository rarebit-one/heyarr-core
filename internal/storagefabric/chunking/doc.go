// Package chunking defines content-defined chunking. FastCDC lands in Milestone 5; only the interface exists today (spec §15, §16).
//
// Milestone 5's stances were recorded before its code. ADR-0034: a manifest is
// an optimisation, keyed by the blob's whole-object digest, and a blob is never
// addressed by its chunks. ADR-0035: the resume unit is a chunk the destination
// re-verified against a manifest it holds, never a byte offset. ADR-0036:
// repair stages a whole replacement and never writes in place.
package chunking
