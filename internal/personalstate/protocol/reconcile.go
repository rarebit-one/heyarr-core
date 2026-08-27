package protocol

// reconcile.go is the pull side of the state-sync protocol (§44, §46). Sync is
// two questions over the causal DAG, answered without ever decrypting a change:
// a peer OFFERS its heads, and the holder computes which of its changes that peer
// is missing. It is deliberately incremental and resumable — a peer that has been
// offline (a phone, #330) names the little it holds and receives only what is
// new beneath the frontier, never a full-log replay.

import "sort"

// CausalHistory returns the set of change ids reachable from heads within have —
// the ids heads causally cover. A head not present in have contributes nothing
// (its ancestry is unknown here), which only ever makes the set smaller — the
// safe bias for its callers (snapshot subsumption, and the compaction bound that
// must never drop a change a partitioned peer still needs).
func CausalHistory(have []EncryptedChange, heads []string) map[string]bool {
	byID := make(map[string]EncryptedChange, len(have))
	for _, c := range have {
		byID[c.ChangeID] = c
	}
	seen := make(map[string]bool, len(have))
	queue := append([]string(nil), heads...)
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[id] {
			continue
		}
		c, ok := byID[id]
		if !ok {
			continue
		}
		seen[id] = true
		queue = append(queue, c.Parents...)
	}
	return seen
}

// Missing returns the changes in have that are NOT in the causal history of
// knownHeads — what a peer holding knownHeads lacks from have. It is pure DAG
// reachability over change ids: a knownHead this holder also has marks that
// change and all its ancestors as known; everything else in have is missing.
//
// A knownHead this holder does NOT have is skipped — it cannot be walked and its
// ancestry is unknown here — which only ever makes Missing send MORE, never less.
// Sending a change the peer already has is harmless (a change is idempotent under
// merge, §42); sending too few would stall convergence, so the bias is safe. The
// result is sorted by change id, so two holders answer one offer identically.
func Missing(have []EncryptedChange, knownHeads []string) []EncryptedChange {
	byID := make(map[string]EncryptedChange, len(have))
	for _, c := range have {
		byID[c.ChangeID] = c
	}

	known := make(map[string]bool, len(have))
	queue := append([]string(nil), knownHeads...)
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if known[id] {
			continue
		}
		c, ok := byID[id]
		if !ok {
			// A head we do not hold: nothing to mark known, nothing to walk.
			continue
		}
		known[id] = true
		queue = append(queue, c.Parents...)
	}

	var missing []EncryptedChange
	for _, c := range have {
		if !known[c.ChangeID] {
			missing = append(missing, c)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].ChangeID < missing[j].ChangeID })
	return missing
}

// HaveAll reports whether every change reachable from wantHeads is present in
// have — the "am I caught up?" check a client runs after applying a pull. It
// returns false as soon as a reachable id is absent, and the first such id, so a
// caller can ask for it specifically. wantHeads a holder does not recognise count
// as not-caught-up, because their ancestry cannot be confirmed present.
func HaveAll(have []EncryptedChange, wantHeads []string) (bool, string) {
	byID := make(map[string]EncryptedChange, len(have))
	for _, c := range have {
		byID[c.ChangeID] = c
	}
	seen := make(map[string]bool, len(have))
	queue := append([]string(nil), wantHeads...)
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		c, ok := byID[id]
		if !ok {
			return false, id
		}
		queue = append(queue, c.Parents...)
	}
	return true, ""
}
