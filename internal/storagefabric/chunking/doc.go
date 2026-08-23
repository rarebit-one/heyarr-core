// Package chunking splits a byte stream into content-defined chunks using
// FastCDC (spec §15, §16).
//
// A fixed-size chunker cuts every N bytes, so inserting one byte at the front
// of a file shifts every boundary after it and nothing downstream is reusable.
// FastCDC picks each boundary from a rolling hash of the bytes themselves, so
// an insertion perturbs only the boundaries near it and leaves the rest of the
// stream cutting exactly where it did before. That single property is what
// makes chunk reuse across modified files and repair of damaged replicas
// possible at all; chunking_test.go measures it as a number rather than
// asserting it in prose.
//
// The package is deliberately pure. It touches no filesystem, no database, no
// CAS and no domain type — depguard enforces that in .golangci.yml — which is
// why it can be verified as arithmetic rather than as a system. It computes a
// chunk's own digest with internal/hashing and nothing else: a Chunk is an
// offset, a length and a digest. It is never an identity. A Blob's identity
// stays the BLAKE3 digest of its whole byte sequence (§13, ADR-0005); chunk
// manifests are an optimisation for transfer and deduplication.
//
// Milestone 5's stances were recorded before its code. ADR-0034: a manifest is
// an optimisation, keyed by the blob's whole-object digest, and a blob is never
// addressed by its chunks. ADR-0035: the resume unit is a chunk the destination
// re-verified against a manifest it holds, never a byte offset. ADR-0036:
// repair stages a whole replacement and never writes in place.
package chunking
