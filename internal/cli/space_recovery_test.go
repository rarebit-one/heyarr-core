package cli

// space_recovery_test.go covers the #360 default recovery-wrap: `space create`
// wraps a new space's key for the user's recovery key by default, so the space
// survives losing every device. These are unit tests over the resolution helpers
// (resolveRecoveryRecipient, resolveRecipients) — the end-to-end wrap is exercised
// by the acceptance demo; here we prove the recipient set is assembled correctly.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/useridentity"
)

// recoveryTestCmd builds a cobra command carrying just the --recovery flag, so a
// test can drive resolveRecoveryRecipient (which reads Flags().Changed and writes
// notes to ErrOrStderr) without standing up the whole space command.
func recoveryTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := &cobra.Command{Use: "create"}
	cmd.Flags().Bool("recovery", true, "")
	stderr := &bytes.Buffer{}
	cmd.SetErr(stderr)
	return cmd, stderr
}

// generatedRecoveryKey creates a user identity in a fresh temp dir and returns the
// dir and the recovery encryption key id it persisted.
func generatedRecoveryKey(t *testing.T) (dir, recoveryID string) {
	t.Helper()
	dir = t.TempDir()
	store, err := useridentity.NewStore(useridentity.StoreOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := store.Generate("me", false)
	if err != nil {
		t.Fatal(err)
	}
	if id.EncryptionKey == "" {
		t.Fatal("Generate persisted no recovery encryption key")
	}
	return dir, id.EncryptionKey
}

// TestResolveRecoveryRecipientDefaultsToIdentityKey: with an identity present and
// --recovery left at its default, the recovery key is resolved as a recipient.
func TestResolveRecoveryRecipientDefaultsToIdentityKey(t *testing.T) {
	dir, want := generatedRecoveryKey(t)
	cmd, stderr := recoveryTestCmd(t)

	got, err := resolveRecoveryRecipient(cmd, dir, true)
	if err != nil {
		t.Fatalf("resolveRecoveryRecipient: %v", err)
	}
	if got != want {
		t.Fatalf("recovery recipient = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected note when an identity exists: %q", stderr.String())
	}
}

// TestResolveRecoveryRecipientDisabled: --recovery=false resolves no recovery
// recipient and never touches the identity store.
func TestResolveRecoveryRecipientDisabled(t *testing.T) {
	cmd, _ := recoveryTestCmd(t)
	got, err := resolveRecoveryRecipient(cmd, t.TempDir(), false)
	if err != nil {
		t.Fatalf("resolveRecoveryRecipient: %v", err)
	}
	if got != "" {
		t.Fatalf("recovery recipient = %q, want empty when --recovery=false", got)
	}
}

// TestResolveRecoveryRecipientNoIdentitySkips: with no identity and --recovery left
// at its default, creation is NOT blocked — the recovery recipient is empty and a
// note explains why.
func TestResolveRecoveryRecipientNoIdentitySkips(t *testing.T) {
	cmd, stderr := recoveryTestCmd(t)
	got, err := resolveRecoveryRecipient(cmd, t.TempDir(), true)
	if err != nil {
		t.Fatalf("a missing identity should be a skip, not an error: %v", err)
	}
	if got != "" {
		t.Fatalf("recovery recipient = %q, want empty when there is no identity", got)
	}
	if !strings.Contains(stderr.String(), "no user identity found") {
		t.Fatalf("expected a note about the missing identity, got %q", stderr.String())
	}
}

// TestResolveRecoveryRecipientExplicitNoIdentityErrors: if the user EXPLICITLY
// passes --recovery and there is no identity to satisfy it, that is an error, not a
// silent skip — the request cannot be honoured.
func TestResolveRecoveryRecipientExplicitNoIdentityErrors(t *testing.T) {
	cmd, _ := recoveryTestCmd(t)
	if err := cmd.Flags().Set("recovery", "true"); err != nil { // mark it Changed
		t.Fatal(err)
	}
	_, err := resolveRecoveryRecipient(cmd, t.TempDir(), true)
	if err == nil {
		t.Fatal("explicit --recovery with no identity should error")
	}
	if !strings.Contains(err.Error(), "identity generate") {
		t.Fatalf("error should point at `identity generate`, got %v", err)
	}
}

// TestResolveRecipientsIncludesRecoveryAndDedups: the recovery id is added to the
// wrap set, and a recovery id that also appears as a named --recipient collapses to
// one — the space is not wrapped twice for the same key.
func TestResolveRecipientsIncludesRecoveryAndDedups(t *testing.T) {
	_, recoveryID := generatedRecoveryKey(t)

	out, err := resolveRecipients("", nil, false, recoveryID)
	if err != nil {
		t.Fatalf("resolveRecipients: %v", err)
	}
	if len(out) != 1 || out[0].ID != recoveryID {
		t.Fatalf("recipients = %+v, want exactly the recovery id %q", out, recoveryID)
	}

	// The same key named explicitly AND as the recovery recipient → one copy.
	deduped, err := resolveRecipients("", []string{recoveryID}, false, recoveryID)
	if err != nil {
		t.Fatalf("resolveRecipients dedup: %v", err)
	}
	if len(deduped) != 1 {
		t.Fatalf("recipients = %+v, want a single deduped copy", deduped)
	}
}
