package crdt

// drive_retention.go is the client-side RETENTION / GC reference view (ADR-0095,
// ADR-0018, task W2/#538). Version history and trash are references to immutable
// content-addressed blobs (ADR-0005): a blob is reclaimable only when NO current,
// historical or trashed entry references it. This file computes, under an explicit
// policy, which references a client would drop and therefore which blob ids become
// unreferenced — the input the CONTROL-PLANE GC needs to actually reclaim the bytes
// (that reclamation is out of scope here).
//
// It is a PURE, read-only function of the converged drive: it mutates nothing and
// emits no changes, so it cannot affect convergence, and two converged replicas
// compute the same report. Dropping a reference for real is a CRDT/control-plane
// action, deliberately NOT done here — a local prune of history is not a wire fact.

import (
	"sort"
	"time"
)

// RetentionPolicy is the explicit knobs ADR-0095 left open, made concrete for the
// client-side view. The live head of every path is ALWAYS retained regardless of
// policy — retention reclaims history and trash, never the current file.
type RetentionPolicy struct {
	// MaxVersionsPerPath caps how many PRIOR versions (superseded blobs) a live
	// path keeps, newest-precedence first. A negative value keeps all history; 0
	// keeps none (only the live head).
	MaxVersionsPerPath int
	// TrashTTL is how long a fully-deleted path's trashed blobs are kept. A
	// negative value keeps trash forever; 0 drops all trash. Age is measured
	// against the most recent content mtime of the trashed writes, since a delete
	// carries no wall-clock time in this model (a documented client-side proxy).
	TrashTTL time.Duration
}

// RetentionReport is the outcome of a retention pass.
type RetentionReport struct {
	// Unreferenced are the blob ids that no policy-retained entry references
	// ANYWHERE in the drive — safe for the control plane to reclaim. Sorted and
	// deduplicated. A blob still referenced by any live head, any kept version, or
	// any kept trashed entry (including at another path) never appears here, so the
	// current/live blob is never reported.
	Unreferenced []string
}

// Retain computes the retention reference view under policy at wall-clock time now.
// It does not modify the drive; see the file comment for why dropping a reference
// for real is a control-plane action, not this function's job.
func (d *Drive) Retain(policy RetentionPolicy, now time.Time) RetentionReport {
	kept := make(map[string]bool)
	all := make(map[string]bool)

	for _, rec := range d.entries {
		for _, v := range rec.values {
			if v.Blob != "" {
				all[v.Blob] = true
			}
		}

		live := rec.liveHeads()
		for _, h := range live {
			if h.Blob != "" {
				kept[h.Blob] = true // the live head(s) are never dropped
			}
		}

		if len(live) > 0 {
			// A live path: its non-live writes are version history. Keep the newest
			// MaxVersionsPerPath of them (all, if the policy is negative).
			liveKeys := make(map[posKey]bool, len(live))
			for _, h := range live {
				liveKeys[h.key] = true
			}
			prior := make([]driveValue, 0, len(rec.values))
			for k, v := range rec.values {
				if !liveKeys[k] && v.Blob != "" {
					prior = append(prior, v)
				}
			}
			sort.Slice(prior, func(i, j int) bool { return prior[i].key.greater(prior[j].key) })
			for i, v := range prior {
				if policy.MaxVersionsPerPath >= 0 && i >= policy.MaxVersionsPerPath {
					break
				}
				kept[v.Blob] = true
			}
			continue
		}

		// A fully-deleted path: every remaining write is trash. Keep it while the
		// trash is younger than the TTL (or forever, if the TTL is negative).
		if policy.TrashTTL < 0 {
			for _, v := range rec.values {
				if v.Blob != "" {
					kept[v.Blob] = true
				}
			}
			continue
		}
		var newest int64
		for _, v := range rec.values {
			if v.MTime > newest {
				newest = v.MTime
			}
		}
		age := time.Duration(now.Unix()-newest) * time.Second
		if age <= policy.TrashTTL {
			for _, v := range rec.values {
				if v.Blob != "" {
					kept[v.Blob] = true
				}
			}
		}
	}

	unref := make([]string, 0)
	for blob := range all {
		if !kept[blob] {
			unref = append(unref, blob)
		}
	}
	sort.Strings(unref)
	return RetentionReport{Unreferenced: unref}
}
