package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/rarebit-one/void-which-binds-go/hashing"
	"github.com/rarebit-one/void-which-binds-go/recovery"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	psclient "github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spacerecover"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/vaultframe"
)

// vaultPushFile writes data to a temp file and pushes it through `vault push`.
func (h *psHarness) vaultPushFile(spaceID, vaultPath string, data []byte) {
	h.t.Helper()
	f := filepath.Join(h.t.TempDir(), "upload")
	if err := os.WriteFile(f, data, 0o600); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.cli("vault", "push", "--space", spaceID, "--path", vaultPath, f); err != nil {
		h.t.Fatalf("vault push %s: %v", vaultPath, err)
	}
}

// vaultPull reads a vault path back through `vault pull` — a fresh process'
// worth of state: it opens the space from scratch, history and all.
func (h *psHarness) vaultPull(spaceID, vaultPath string) []byte {
	h.t.Helper()
	out, err := h.cli("vault", "pull", spaceID, vaultPath)
	if err != nil {
		h.t.Fatalf("vault pull %s: %v", vaultPath, err)
	}
	return []byte(out)
}

// vaultLs lists the vault's live paths through `vault ls --json`.
func (h *psHarness) vaultLs(spaceID string) []string {
	h.t.Helper()
	out, err := h.cli("vault", "ls", spaceID, "--json")
	if err != nil {
		h.t.Fatal(err)
	}
	var entries []vaultEntryView
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		h.t.Fatalf("vault ls --json: %v\n%s", err, out)
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	sort.Strings(paths)
	return paths
}

// TestRekeyVaultSpaceKeepsEveryFile is #698 fixed (ADR-0103): re-keying a VAULT
// space revokes a recipient without losing a byte. A file pushed before the
// rotation (sealed under epoch 0) and one pushed after (epoch 1) both pull back
// byte-identical through the real commands, the drive still lists both because
// nothing was compacted, the revoked recipient's copy is gone, and the space is
// at epoch 1.
func TestRekeyVaultSpaceKeepsEveryFile(t *testing.T) {
	h := newPSHarness(t, auth.ScopeAdmin)
	spaceID := h.createSpace()
	other := h.addSecondRecipient(spaceID)

	fileA := bytes.Repeat([]byte("before the rotation\n"), 9000) // several frames
	fileB := []byte("after the rotation")
	h.vaultPushFile(spaceID, "docs/a.txt", fileA)

	view, err := h.rekey(spaceID, other)
	if err != nil {
		t.Fatalf("rekey: %v", err)
	}
	if view.KeyEpoch != 1 || len(view.Revoked) != 1 || view.Revoked[0] != other || len(view.Remaining) != 1 {
		t.Fatalf("rekey view = %+v", view)
	}

	h.vaultPushFile(spaceID, "docs/b.txt", fileB)

	if got := h.vaultPull(spaceID, "docs/a.txt"); !bytes.Equal(got, fileA) {
		t.Fatalf("the pre-rotation file pulled back as %d bytes, want %d identical", len(got), len(fileA))
	}
	if got := h.vaultPull(spaceID, "docs/b.txt"); !bytes.Equal(got, fileB) {
		t.Fatalf("the post-rotation file pulled back as %q", got)
	}
	if got := h.vaultLs(spaceID); len(got) != 2 || got[0] != "docs/a.txt" || got[1] != "docs/b.txt" {
		t.Fatalf("vault ls = %v, want both files", got)
	}

	keys, err := h.client.SpaceKeys(h.ctx, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	if keys.KeyEpoch != 1 {
		t.Fatalf("server key_epoch = %d, want 1", keys.KeyEpoch)
	}
	if wrappedFor(keys.WrappedKeys, other) || len(keys.WrappedKeys) != 1 || keys.WrappedKeys[0].Epoch != 1 {
		t.Fatalf("wraps after rekey = %+v, want only this device's, at epoch 1", keys.WrappedKeys)
	}
	if changes, err := h.client.Changes(h.ctx, spaceID); err != nil || len(changes) != 2 {
		t.Fatalf("changes = %d (%v), want both drive writes (nothing compacted)", len(changes), err)
	}
	if _, ok, err := h.client.Snapshot(h.ctx, spaceID); err != nil || ok {
		t.Fatalf("a re-key pushed a snapshot (%v, %v)", ok, err)
	}
}

// TestRekeyKeepsARacingWritersChange is the second hole #700's review found: a
// device that opened the space before a rotation still holds the old key and
// can seal a change — and a vault file — under it after the rotation landed. A
// fresh reader at the new epoch must still read both, through the history.
func TestRekeyKeepsARacingWritersChange(t *testing.T) {
	h := newPSHarness(t, auth.ScopeAdmin)
	spaceID := h.createSpace()
	other := h.addSecondRecipient(spaceID)

	writer, err := openSpace(h.ctx, h.client, h.custody(), spaceID)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := writer.Epoch(spaceID); e != 0 {
		t.Fatalf("writer opened at epoch %d", e)
	}

	if _, err := h.rekey(spaceID, other); err != nil {
		t.Fatal(err)
	}

	// The stale writer seals a file and the drive change recording it under
	// the epoch-0 key, exactly as `vault push` would have a moment earlier.
	old, _ := writer.SpaceKey(spaceID)
	plain := []byte("written by a device still at epoch 0")
	var content bytes.Buffer
	m, err := vaultframe.Seal(old, bytes.NewReader(plain), &content)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.client.PutVaultBlob(h.ctx, m.Content, bytes.NewReader(content.Bytes())); err != nil {
		t.Fatal(err)
	}
	sealed, err := vaultframe.SealManifest(old, m)
	if err != nil {
		t.Fatal(err)
	}
	mh := hashing.New()
	_, _ = mh.Write(sealed)
	manifestID := mh.Sum().String()
	if err := h.client.PutVaultBlob(h.ctx, manifestID, bytes.NewReader(sealed)); err != nil {
		t.Fatal(err)
	}
	drive, changes, err := loadDrive(h.ctx, h.client, writer, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := drive.Put("late.txt", manifestID, m.PlaintextSize, 1)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := statesync.EncodeChange(writer, spaceID, protocol.Heads(changes), ch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.PutChange(h.ctx, ec); err != nil {
		t.Fatal(err)
	}

	// A fresh reader opens at epoch 1 and reads the stale writer's file.
	reader, err := openSpace(h.ctx, h.client, h.custody(), spaceID)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := reader.Epoch(spaceID); e != 1 {
		t.Fatalf("reader opened at epoch %d, want 1", e)
	}
	if got := h.vaultPull(spaceID, "late.txt"); !bytes.Equal(got, plain) {
		t.Fatalf("the racing writer's file pulled back as %q", got)
	}
}

// TestRekeyRacingRotationsConflict: two devices open at the same epoch and both
// rotate. The first lands; the second is refused by the controller's
// compare-and-swap and reports a conflict to retry, and the space is left as
// the first rotation made it.
func TestRekeyRacingRotationsConflict(t *testing.T) {
	h := newPSHarness(t, auth.ScopeAdmin)
	spaceID := h.createSpace()
	other := h.addSecondRecipient(spaceID)

	first, err := openSpace(h.ctx, h.client, h.custody(), spaceID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := openSpace(h.ctx, h.client, h.custody(), spaceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rekeyOpenSpace(h.ctx, h.client, first, spaceID, nil); err != nil {
		t.Fatalf("first rotation: %v", err)
	}
	_, err = rekeyOpenSpace(h.ctx, h.client, second, spaceID, []string{other})
	if !errors.Is(err, errKeyEpochConflict) {
		t.Fatalf("second rotation = %v, want errKeyEpochConflict", err)
	}
	keys, err := h.client.SpaceKeys(h.ctx, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	if keys.KeyEpoch != 1 || !wrappedFor(keys.WrappedKeys, other) {
		t.Fatalf("after the race = epoch %d, other kept %v; want the first rotation's state", keys.KeyEpoch, wrappedFor(keys.WrappedKeys, other))
	}
	// Re-opening and rotating again is the documented retry, and it lands.
	if _, err := h.rekey(spaceID, other); err != nil {
		t.Fatalf("the retry: %v", err)
	}
}

// TestRekeyRefusesARecipientAddedMidRotation (#703): a device reads the
// recipients and builds its rotation; before it lands, another client adds a
// recipient through the re-wrap path, which does not move the epoch. The epoch
// compare-and-swap alone would let the rotation land and drop the newcomer's
// wrap as stale, locking the new device out without a word. The controller
// compares the recipient set too: the rotation is refused, the newcomer keeps
// its copy, and running the rotation again re-wraps it.
func TestRekeyRefusesARecipientAddedMidRotation(t *testing.T) {
	h := newPSHarness(t, auth.ScopeAdmin)
	spaceID := h.createSpace()
	revoked := h.addSecondRecipient(spaceID)

	mgr, err := openSpace(h.ctx, h.client, h.custody(), spaceID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := prepareRekey(h.ctx, h.client, mgr, spaceID, []string{revoked})
	if err != nil {
		t.Fatal(err)
	}
	added := h.addSecondRecipient(spaceID) // another client, between the read and the rotation

	_, err = pending.commit(h.ctx, h.client)
	if !errors.Is(err, errRecipientsChanged) || !strings.Contains(err.Error(), "run the rotation again") {
		t.Fatalf("rotation after a concurrent add = %v, want errRecipientsChanged", err)
	}
	keys, err := h.client.SpaceKeys(h.ctx, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	if keys.KeyEpoch != 0 || !wrappedFor(keys.WrappedKeys, added) || !wrappedFor(keys.WrappedKeys, revoked) {
		t.Fatalf("after the refusal = epoch %d, wraps %+v; want epoch 0 with the added and the revoked copies intact", keys.KeyEpoch, keys.WrappedKeys)
	}

	view, err := h.rekey(spaceID, revoked)
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if view.KeyEpoch != 1 || !slices.Contains(view.Remaining, added) {
		t.Fatalf("retry view = %+v, want epoch 1 keeping the added recipient", view)
	}
	keys, err = h.client.SpaceKeys(h.ctx, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	if !wrappedFor(keys.WrappedKeys, added) || wrappedFor(keys.WrappedKeys, revoked) {
		t.Fatalf("after the retry = %+v, want the added recipient re-wrapped and the revoked gone", keys.WrappedKeys)
	}
}

// TestRekeyRefusesARecipientRemovedMidRotation (#703): the mirror case. A
// device builds a rotation that keeps a recipient; before it lands, another
// client removes that recipient. Landing it would hand the removed recipient
// the new key and reverse the revocation, so it is refused and the removed
// recipient gets nothing.
func TestRekeyRefusesARecipientRemovedMidRotation(t *testing.T) {
	h := newPSHarness(t, auth.ScopeAdmin)
	spaceID := h.createSpace()
	removed := h.addSecondRecipient(spaceID)

	mgr, err := openSpace(h.ctx, h.client, h.custody(), spaceID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := prepareRekey(h.ctx, h.client, mgr, spaceID, nil) // keeps everyone
	if err != nil {
		t.Fatal(err)
	}
	if err := h.client.RevokeKey(h.ctx, spaceID, removed); err != nil { // another client
		t.Fatal(err)
	}

	if _, err := pending.commit(h.ctx, h.client); !errors.Is(err, errRecipientsChanged) {
		t.Fatalf("rotation after a concurrent removal = %v, want errRecipientsChanged", err)
	}
	keys, err := h.client.SpaceKeys(h.ctx, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	if keys.KeyEpoch != 0 || wrappedFor(keys.WrappedKeys, removed) {
		t.Fatalf("after the refusal = epoch %d, wraps %+v; want epoch 0 and the removed recipient still without a copy", keys.KeyEpoch, keys.WrappedKeys)
	}

	if _, err := h.rekey(spaceID); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	keys, err = h.client.SpaceKeys(h.ctx, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	if keys.KeyEpoch != 1 || wrappedFor(keys.WrappedKeys, removed) || len(keys.WrappedKeys) != 1 {
		t.Fatalf("after the retry = epoch %d, wraps %+v; want epoch 1 with only this device", keys.KeyEpoch, keys.WrappedKeys)
	}
}

// TestOpenSpaceRefusesAnIncompleteHistory: a space rotated twice, opened with
// one of its two history rows missing, is refused rather than opened with the
// content before the gap silently unreadable (ADR-0103).
func TestOpenSpaceRefusesAnIncompleteHistory(t *testing.T) {
	h := newPSHarness(t, auth.ScopeAdmin)
	spaceID := h.createSpace()
	other := h.addSecondRecipient(spaceID)
	if _, err := h.rekey(spaceID, other); err != nil {
		t.Fatal(err)
	}
	if _, err := h.rekey(spaceID, h.addSecondRecipient(spaceID)); err != nil {
		t.Fatal(err)
	}
	keys, err := h.client.SpaceKeys(h.ctx, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	history, err := h.client.KeyHistory(h.ctx, spaceID)
	if err != nil || len(history) != 2 {
		t.Fatalf("history = %d rows (%v), want 2", len(history), err)
	}
	m := psclient.New()
	partial := []psclient.HistoryEntry{{Epoch: history[1].Epoch, SealedPrev: history[1].SealedPrev}}
	err = m.OpenWithHistory(spaceID, keys.WrappedKeys[0].Wrapped, keys.KeyEpoch, partial, h.custody())
	if !errors.Is(err, psclient.ErrIncompleteHistory) {
		t.Fatalf("open with a missing row = %v, want ErrIncompleteHistory", err)
	}
}

// TestRekeyedVaultRecoversFromABlob: the whole recovery chain over a re-keyed
// vault (ADR-0022, ADR-0103). A vault wrapped for a recovery key is re-keyed
// (the recovery key re-wrapped at epoch 1), a recovery blob is exported, and a
// brand-new device recovers from the paper secret and the blob alone — and pulls
// both the file sealed before the rotation and the one after, byte-identical.
func TestRekeyedVaultRecoversFromABlob(t *testing.T) {
	h := newPSHarness(t, auth.ScopeAdmin)
	secret, err := recovery.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	recID, err := spacerecover.RecipientID(secret)
	if err != nil {
		t.Fatal(err)
	}
	spaceID := h.createSpace()
	h.addRecipient(spaceID, recID)
	other := h.addSecondRecipient(spaceID)

	fileA := bytes.Repeat([]byte("sealed at epoch 0\n"), 5000)
	fileB := []byte("sealed at epoch 1")
	h.vaultPushFile(spaceID, "a.bin", fileA)
	if view, err := h.rekey(spaceID, other); err != nil || view.KeyEpoch != 1 {
		t.Fatalf("rekey = %+v, %v", view, err)
	}
	h.vaultPushFile(spaceID, "b.bin", fileB)

	dir := t.TempDir()
	blob := filepath.Join(dir, "recovery.blob")
	if _, _, err := run(t, h.ctx, "--config", h.config, "space", "export-recovery", "--out", blob, "--recipient", recID); err != nil {
		t.Fatalf("export-recovery: %v", err)
	}
	secretFile := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secretFile, []byte(secret.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dir, "fresh-device")
	if _, _, err := run(t, h.ctx, "device", "generate", "--device-dir", fresh, "--name", "fresh"); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := run(t, h.ctx, "--config", h.config, "space", "--device-dir", fresh, "recover",
		"--secret-file", secretFile, "--from-blob", blob, "--rewrap"); err != nil {
		t.Fatalf("recover --from-blob --rewrap: %v (%s)", err, stderr)
	}

	h.deviceDir = fresh // read as the recovered device from here on
	if got := h.vaultPull(spaceID, "a.bin"); !bytes.Equal(got, fileA) {
		t.Fatalf("the recovered device pulled the pre-rotation file as %d bytes", len(got))
	}
	if got := h.vaultPull(spaceID, "b.bin"); !bytes.Equal(got, fileB) {
		t.Fatalf("the recovered device pulled the post-rotation file as %q", got)
	}
}
