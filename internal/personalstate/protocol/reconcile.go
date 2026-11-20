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
// merge, §42); sending too few would stall convergence, so the bias is safe.
//
// The result is in TOPOLOGICAL order: every change comes after each of its parents
// that is also in the result, ties broken by change id, so two holders answer one
// offer identically (#682, ADR-0101). A pusher that stops at its first failure
// therefore never leaves a target holding a child without its parent. Were it to,
// the target would offer the child as a head, every later offer would mark the
// parent known through the child's ancestry on the source, and the parent would
// never be resent.
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
	return topological(missing)
}

// topological orders changes parents-first (Kahn's algorithm). Only parents in
// the set constrain the order: a parent the set does not hold (the target has it,
// or it was compacted away) is already satisfied. Among the changes ready at each
// step the smallest id goes first, so the order is a pure function of the set.
// Content addressing makes a cycle impossible (a change's id covers its parents),
// but a malformed set must not lose changes: anything a cycle would strand is
// appended in id order rather than dropped.
func topological(changes []EncryptedChange) []EncryptedChange {
	if len(changes) < 2 {
		return changes
	}
	byID := make(map[string]EncryptedChange, len(changes))
	for _, c := range changes {
		byID[c.ChangeID] = c
	}
	pending := make(map[string]int, len(changes)) // in-set parents not yet emitted
	children := make(map[string][]string, len(changes))
	for id, c := range byID {
		seen := make(map[string]bool, len(c.Parents))
		for _, p := range c.Parents {
			if _, in := byID[p]; !in || seen[p] || p == id {
				continue
			}
			seen[p] = true
			pending[id]++
			children[p] = append(children[p], id)
		}
	}
	var ready []string
	for id := range byID {
		if pending[id] == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)

	out := make([]EncryptedChange, 0, len(byID))
	emitted := make(map[string]bool, len(byID))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		out = append(out, byID[id])
		emitted[id] = true
		for _, child := range children[id] {
			pending[child]--
			if pending[child] == 0 {
				// Insert keeping ready sorted, so the next pick is the smallest id.
				at := sort.SearchStrings(ready, child)
				ready = append(ready, "")
				copy(ready[at+1:], ready[at:])
				ready[at] = child
			}
		}
	}
	if len(out) < len(byID) {
		var stranded []string
		for id := range byID {
			if !emitted[id] {
				stranded = append(stranded, id)
			}
		}
		sort.Strings(stranded)
		for _, id := range stranded {
			out = append(out, byID[id])
		}
	}
	return out
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
