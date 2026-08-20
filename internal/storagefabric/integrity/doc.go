// Package integrity verifies stored bytes against expected hashes and reclaims
// bytes nothing references (spec §57, ADR-0018).
//
// It holds the two operations in Heyarr that can destroy data, and it is built
// so that neither can do so quietly:
//
//   - Verification never deletes. A blob whose bytes no longer hash to their
//     own name is moved to quarantine and recorded, because it is evidence. On
//     a hardlink-ingested library the CAS and the operator's original file
//     share an inode, so a mismatch frequently means an external tool rewrote
//     that original — and on hyperion-1 hardlink is the outcome for every
//     ingested file rather than an edge case (#43).
//   - Garbage collection is dry-run by default at every level: the CLI, the job
//     payload's zero value and the CollectOptions zero value all mean "report,
//     change nothing". It is also two-pass, so the grace window that makes a
//     mistaken delete reversible has somewhere to start counting from.
//
// The package declares the catalog it needs as an interface and lets
// persistence implement it, rather than importing the catalog itself: the
// Storage Fabric must stay extractable and may not depend on Heyarr's content
// domain (§18, ADR-0007). Nothing here knows what a work, an edition or an
// asset is — only that some number of things reference a blob.
package integrity
