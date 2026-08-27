package peerapi_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestPeerSurfaceCannotReadPersonalState is the structural half of Invariant 6,
// §42 and ADR-0049: the peer moves opaque encrypted changes and NEVER decrypts or
// merges one. It asserts, over the WHOLE transitive import graph of the peer
// surface, that none of the plaintext-reading personal-state packages is
// reachable — so "a merge helper on the peer that peeks at a decrypted field"
// cannot be written without this test (and the matching depguard rule) going red.
//
// The peer may depend on internal/personalstate/protocol (the opaque wire change
// and its pure DAG reconciliation); it may NOT depend on the CRDT model, the
// device-side client, the CRDT<->change bridge, or the encryption package — those
// are where a stored byte becomes plaintext, and that only ever happens on an
// authorised device.
//
// SABOTAGE (the reviewer's break): add `import ".../personalstate/crdt"` to any
// file in this package and this test fails — as does the depguard rule at lint.
func TestPeerSurfaceCannotReadPersonalState(t *testing.T) {
	const pkg = "github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	forbidden := []string{
		"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt",
		"github.com/rarebit-one/heyarr-core/internal/personalstate/client",
		"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync",
		"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption",
	}

	out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	deps := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		deps[strings.TrimSpace(line)] = true
	}

	// Sanity: the opaque wire type MUST be reachable, or the check would pass
	// vacuously on a peer surface that imports no personal state at all.
	if !deps["github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"] {
		t.Fatal("the peer surface does not import personalstate/protocol — the state routes are not wired, so this boundary check is vacuous")
	}

	for _, f := range forbidden {
		if deps[f] {
			t.Errorf("the peer surface transitively imports %s — it must move OPAQUE changes and read none (Invariant 6, §42, ADR-0049)", f)
		}
	}
}
