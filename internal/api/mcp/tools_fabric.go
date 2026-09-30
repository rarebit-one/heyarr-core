package mcp

import "github.com/rarebit-one/heyarr-core/internal/auth"

// registerFabricTools registers the node and fabric reads and actions:
// providers, peers, replicas, reconciliation and verification.
func (s *Server) registerFabricTools() {
	s.tools.register(Tool{
		Name:     "get_provider_status",
		Title:    "Indexer and download-client status",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "Report the configured indexers and download clients (and metadata providers), " +
			"what each can do, whether the last check found it usable and which version it reported. " +
			"This is the read behind \"why is nothing being acquired\": a node with none configured is " +
			"a supported deployment and says so with an empty list. No credential is ever reported.",
		InputSchema: schemaNoArgs,
		Handler:     s.getProviderStatus,
	})

	s.tools.register(Tool{
		Name:     "get_peer_status",
		Title:    "Peer status",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "List the peers this instance knows about and what each is for. " +
			"A single peer is a supported deployment rather than a symptom — most " +
			"Heyarr installations are one machine — so do not report a fabric of one " +
			"as a replication fault. Ask get_content_satisfaction whether placement " +
			"is answering a real question: it reports `unproven` when the target set " +
			"is this node alone.",
		InputSchema: schemaNoArgs,
		Handler:     s.getPeerStatus,
	})

	s.tools.register(Tool{
		Name:     "get_replica_status",
		Title:    "Where bytes are",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "Report which peers hold a blob and whether their copy is verified. " +
			"A copy that is pending or corrupt is NOT a copy for placement purposes.",
		InputSchema: schemaBlobHash,
		Handler:     s.getReplicaStatus,
	})

	s.tools.register(Tool{
		Name:     "sync_peer",
		Title:    "Reconcile against a peer",
		Scope:    auth.ScopeWrite,
		ReadOnly: false,
		Description: "Ask this instance to reconcile the desired blob set against what " +
			"a peer is known to hold, now, rather than waiting for the next scheduled " +
			"cycle. Queues work rather than doing it: the diff enqueues a transfer per " +
			"gap and the bytes move afterwards, so this reply says only that the cycle " +
			"was accepted. It is the on-demand half of the reconciliation §57 asks for " +
			"on a beat and on demand — useful straight after enrolling a peer or " +
			"restoring one, and pointless to call in a loop.",
		InputSchema: schemaSyncPeer,
		Handler:     s.syncPeer,
	})

	s.tools.register(Tool{
		Name:     "verify_blob",
		Title:    "Re-verify stored bytes",
		Scope:    auth.ScopeWrite,
		ReadOnly: false,
		Description: "Queue a re-read of a blob's bytes to confirm they still hash to " +
			"what the catalog recorded. Queues work rather than doing it — the answer " +
			"arrives on the job, not in this reply — because re-hashing a large file is " +
			"not something to hold a request open for.",
		InputSchema: schemaBlobHash,
		Handler:     s.verifyBlob,
	})
}
