package replication_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestReplicationCannotReadPersonalState holds the same Invariant 6 boundary the
// peer surface holds: the replicator fans out OPAQUE changes and decrypts none.
// It asserts, over the whole transitive import graph, that the plaintext-reading
// personal-state packages are unreachable — so a "helpful" dedup on a decrypted
// field cannot be written here. Mirrors the peerapi boundary test.
func TestReplicationCannotReadPersonalState(t *testing.T) {
	const pkg = "github.com/rarebit-one/heyarr-core/internal/personalstate/replication"
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
	if !deps["github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"] {
		t.Fatal("replication does not import personalstate/protocol — this boundary check is vacuous")
	}
	for _, f := range forbidden {
		if deps[f] {
			t.Errorf("replication transitively imports %s — it must move OPAQUE changes and read none (Invariant 6, §42, ADR-0049)", f)
		}
	}
}
