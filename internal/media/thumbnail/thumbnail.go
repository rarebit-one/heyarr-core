// Package thumbnail defines the job type and payload for derived thumbnail
// generation (ADR-0109).
//
// Thumbnails are normal CAS blobs — content-addressed JPEG bytes indexed by the
// derived_thumbnails table. The job reads a source blob, decodes it, scales it
// to the requested bucket, re-encodes it as JPEG and stores it back in the CAS.
// The handler lives in internal/worker to avoid any import of persistence from
// here (invariant 2).
package thumbnail

import "strconv"

// JobType is the job queue kind for derive_thumbnail jobs.
const JobType = "derive_thumbnail"

// Recipe is a version constant that covers the full derivation pipeline:
// decoder selection, EXIF orientation correction, Catmull-Rom scaler,
// JPEG quality and output format. Incrementing Recipe invalidates all existing
// derived_thumbnails rows and causes them to be lazily re-derived.
const Recipe = 1

// Buckets is the ordered list of valid thumbnail long-edge pixel limits.
var Buckets = []int{256, 512, 1024}

// ValidBucket reports whether n is a supported bucket.
func ValidBucket(n int) bool {
	for _, b := range Buckets {
		if n == b {
			return true
		}
	}
	return false
}

// Payload is the job payload for a derive_thumbnail job.
type Payload struct {
	SourceHash string `json:"source_hash"`
	Bucket     int    `json:"bucket"`
}

// DedupeKey returns the idempotency key for a given source+bucket pair.
// Enqueuing with this key is a no-op when the job already exists (ADR-0008).
// Recipe is NOT included in the dedupe key: if a job for this (source, bucket)
// is already pending, let it run — it will see the current Recipe value and
// produce a current-recipe row.
func DedupeKey(sourceHash string, bucket int) string {
	return "derive-thumbnail:" + sourceHash + ":" + strconv.Itoa(bucket)
}
