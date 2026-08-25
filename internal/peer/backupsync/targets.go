package backupsync

import (
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

// FullPeerTargets is the set a backup is pushed to: every trusted Full Peer, and
// not this node (§50). A Partial, Cache, Archive or Compute peer is not a place
// a whole control plane belongs, and IsSelf is not a peer at all.
//
// It is derived fresh from a membership listing each cycle, which is what makes
// a peer revoked between cycles drop out of the next push — the fresh read IS
// the revocation mechanism, the same as everywhere else the membership store is
// consulted (ADR-0012).
func FullPeerTargets(members []membership.Member) []Target {
	var targets []Target
	for _, m := range members {
		if m.IsSelf || m.Mode != "full" {
			continue
		}
		targets = append(targets, Target{
			Peer:     mtls.Peer{PeerID: m.PeerID, Name: m.Name, PublicKey: m.PublicKey},
			Endpoint: m.Endpoint,
		})
	}
	return targets
}
