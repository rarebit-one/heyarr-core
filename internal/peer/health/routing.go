package health

// Health is advisory for reads and blocking for writes (§31, §32, M4-10).
//
// The two functions below are deliberately not symmetrical, and the asymmetry
// is the substance of this issue rather than an oversight in Destinations. Both
// live in one file so that the next reader meets them together.

// Sources returns the peers a read or a replication PULL may be served from.
//
// Only reachable peers survive. That is safe precisely because it is a read:
// §31 says every healthy Full Peer can serve content, so skipping one that is
// down costs a byte range served from somewhere else, and standing up the
// second peer is what makes "somewhere else" exist. An unknown peer — never
// heard from, never probed — is skipped too: it has not been shown to be up,
// and migration 00020 chose 'unknown' over 'reachable' as the default exactly
// so that an unprobed peer could not be routed to on an assumption.
//
// The failure this avoids is a client waiting out a TCP timeout against a
// machine that has been off since Tuesday while a healthy peer three feet away
// holds the same bytes.
func Sources(peers []Peer) []Peer {
	out := make([]Peer, 0, len(peers))
	for _, p := range peers {
		if p.State == StateReachable {
			out = append(out, p)
		}
	}
	return out
}

// Destinations returns every candidate peer, UNFILTERED — including the ones
// Sources just skipped.
//
// # Do not "fix" this to filter by health
//
// It looks like an oversight next to Sources and it is the opposite of one. A
// destination is work OWED to a peer: a blob it should hold and does not, a
// transfer queued towards it, a placement gap §34 wants closed. Work owed to a
// peer that is down stays owed. It does not stop being owed because the machine
// is rebooting.
//
// Filtering here would mean that every time a site reboots, the replication
// planner looks at the library, sees no destination that needs anything, and
// records that everything is converged. The peer comes back with the gap it
// left with, and nothing ever notices — because the moment that would have
// noticed was the moment the peer was unreachable. The library quietly stops
// converging, and it does so silently, which is the worst available failure: a
// durability promise that is false and reports itself as true.
//
// Skipping a source loses one read. Skipping a destination loses a replica, and
// loses it without saying so. That is why one filters and one does not.
//
// What health legitimately changes about a destination is SCHEDULING, not
// eligibility — how hard to retry, how long to back off, whether to start a
// transfer this minute. That belongs to the transfer queue, which can see the
// same State on each Peer here, and it is a different question from whether the
// work exists at all. The queue may defer; the plan may not forget.
func Destinations(peers []Peer) []Peer {
	out := make([]Peer, 0, len(peers))
	out = append(out, peers...)
	return out
}
