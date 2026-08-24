//go:build unix

package cas

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// Nothing in this repository's gate injected a permission fault before #151,
// which is why a store that could not create a directory had never been
// exercised at all — six sightings of a real bug and not one test that could
// have caught any of them. These tests inject the fault deliberately.

// withUnwritable makes dir unwritable for the duration of fn, and restores it
// afterwards whatever happens — a test that leaves a 0o500 directory behind
// breaks t.TempDir's own cleanup.
func withUnwritable(t *testing.T, dir string, fn func()) {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod %s: %v", dir, err)
	}
	defer func() {
		if err := os.Chmod(dir, info.Mode().Perm()); err != nil {
			t.Fatalf("restoring %s: %v", dir, err)
		}
	}()
	// A process that can write anywhere regardless of mode — root, which a
	// container CI job sometimes is — cannot have this fault injected into it,
	// and a test that silently passed under it would be worse than one that
	// says why it did not run.
	probe := filepath.Join(dir, "root-check")
	if err := os.Mkdir(probe, 0o750); err == nil {
		_ = os.Remove(probe)
		t.Skip("this process can write into a 0o500 directory, so a permission fault cannot be injected")
	}
	fn()
}

// TestPermissionFaultDiagnosesAPoisonedShard is the shape #151 was originally
// reported as: one shard directory that cannot be written into, while the rest
// of the store is fine.
func TestPermissionFaultDiagnosesAPoisonedShard(t *testing.T) {
	root := t.TempDir()
	store, err := OpenFS(root)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}

	// Establish the shard this content lands in, then poison its first level
	// exactly as a directory created under a 0o177 umask would be.
	content := []byte("the bytes whose shard is about to be made unwritable")
	h := hashing.New()
	if _, err := h.Write(content); err != nil {
		t.Fatalf("hashing: %v", err)
	}
	final := store.blobPath(h.Sum())
	shardTop := filepath.Dir(filepath.Dir(final))
	if err := os.MkdirAll(shardTop, dirPerm); err != nil {
		t.Fatalf("creating the shard: %v", err)
	}
	var fault *PermissionFault
	withUnwritable(t, shardTop, func() {
		src := filepath.Join(t.TempDir(), "source")
		if err := os.WriteFile(src, content, 0o600); err != nil {
			t.Fatalf("writing the source: %v", err)
		}
		_, err := store.Link(context.Background(), src, Copy)
		if err == nil {
			t.Fatal("Link succeeded into an unwritable shard")
		}
		if !errors.As(err, &fault) {
			t.Fatalf("error is not a *PermissionFault: %v", err)
		}
		text := err.Error()
		// The parent chain is the point: it must name every level between the
		// store root and the refused path, with a mode for each.
		for _, want := range []string{
			"store diagnosis",
			"root: " + root,
			"umask:",
			"parent chain:",
			blobsDir,
			hashing.Algorithm,
			filepath.Base(shardTop),
			"no owner write bit",
			"an unrelated directory under the store root: created",
			"scope: " + string(ScopePath),
		} {
			if !strings.Contains(text, want) {
				t.Errorf("the diagnosis does not mention %q:\n%s", want, text)
			}
		}
		if fault.Scope != ScopePath {
			t.Errorf("scope %q, want %q\n%s", fault.Scope, ScopePath, text)
		}
		// A poisoned shard is one job's problem, not the store's.
		if errors.Is(err, ErrStoreUnwritable) {
			t.Errorf("a single poisoned shard was reported as a store-wide fault:\n%s", text)
		}
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("the underlying refusal was lost: %v", err)
		}
	})
	t.Logf("diagnosis emitted:\n%s", fault.Diagnosis)
}

// TestPermissionFaultDiagnosesAnUnwritableBlobTree is the shape the reopening
// of #151 reported: twelve ingests into twelve different shards, all refused,
// because the directory that holds every shard was unwritable rather than any
// one shard.
func TestPermissionFaultDiagnosesAnUnwritableBlobTree(t *testing.T) {
	root := t.TempDir()
	store, err := OpenFS(root)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	blobTree := filepath.Join(root, blobsDir, hashing.Algorithm)
	if err := os.MkdirAll(blobTree, dirPerm); err != nil {
		t.Fatalf("creating the blob tree: %v", err)
	}
	withUnwritable(t, blobTree, func() {
		src := filepath.Join(t.TempDir(), "source")
		if err := os.WriteFile(src, []byte("anything at all"), 0o600); err != nil {
			t.Fatalf("writing the source: %v", err)
		}
		_, err := store.Link(context.Background(), src, Copy)
		if err == nil {
			t.Fatal("Link succeeded into an unwritable blob tree")
		}
		var fault *PermissionFault
		if !errors.As(err, &fault) {
			t.Fatalf("error is not a *PermissionFault: %v", err)
		}
		if fault.Scope != ScopeBlobTree {
			t.Fatalf("scope %q, want %q\n%s", fault.Scope, ScopeBlobTree, err)
		}
		// This is the distinction the whole diagnosis exists for: the second,
		// unrelated shard was tried and it failed too, so the caller is told
		// this is not a per-job fault.
		if !errors.Is(err, ErrStoreUnwritable) {
			t.Errorf("a store-wide fault did not match ErrStoreUnwritable:\n%s", err)
		}
		if !strings.Contains(err.Error(), "refused:") {
			t.Errorf("the diagnosis does not report the second-directory probe:\n%s", err)
		}
		t.Logf("diagnosis emitted:\n%s", fault.Diagnosis)
	})
}

// TestPermissionFaultOnPut covers the other write path into the store, so a
// diagnosis is not something only Link produces. Put stages under tmp/, so
// that is the directory to take away from it.
func TestPermissionFaultOnPut(t *testing.T) {
	root := t.TempDir()
	store, err := OpenFS(root)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	withUnwritable(t, filepath.Join(root, tmpDir), func() {
		_, err := store.Put(context.Background(), strings.NewReader("bytes"))
		if err == nil {
			t.Fatal("Put succeeded with an unwritable staging directory")
		}
		var fault *PermissionFault
		if !errors.As(err, &fault) {
			t.Fatalf("error is not a *PermissionFault: %v", err)
		}
		if !strings.Contains(err.Error(), "parent chain:") || !strings.Contains(err.Error(), tmpDir) {
			t.Errorf("the diagnosis does not describe the staging directory:\n%s", err)
		}
		// The blob tree is fine, so this is one write's problem, not the
		// store's — and saying so is the whole point of the probes.
		if errors.Is(err, ErrStoreUnwritable) {
			t.Errorf("an unwritable tmp/ was reported as a store-wide fault:\n%s", err)
		}
		t.Logf("diagnosis emitted:\n%s", fault.Diagnosis)
	})
}

// TestPermissionFaultOnAnUnwritableStoreRoot is the widest scope: nothing new
// can be created at the top of the store at all.
func TestPermissionFaultOnAnUnwritableStoreRoot(t *testing.T) {
	root := t.TempDir()
	store, err := OpenFS(root)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	withUnwritable(t, root, func() {
		err := store.permissionFault("creating blob directory",
			filepath.Join(root, blobsDir), &fs.PathError{Op: "mkdir", Path: root, Err: fs.ErrPermission})
		var fault *PermissionFault
		if !errors.As(err, &fault) {
			t.Fatalf("error is not a *PermissionFault: %v", err)
		}
		if fault.Scope != ScopeStore {
			t.Errorf("scope %q, want %q\n%s", fault.Scope, ScopeStore, err)
		}
		if !errors.Is(err, ErrStoreUnwritable) {
			t.Errorf("an unwritable store root did not match ErrStoreUnwritable:\n%s", err)
		}
		if !fault.StoreWide() {
			t.Error("StoreWide reported false for an unwritable store root")
		}
		t.Logf("diagnosis emitted:\n%s", fault.Diagnosis)
	})
}

// TestPermissionFaultLeavesOtherFailuresAlone: a diagnosis of a store that is
// merely out of disk would be noise, and the error already says which.
func TestPermissionFaultLeavesOtherFailuresAlone(t *testing.T) {
	root := t.TempDir()
	store, err := OpenFS(root)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	err = store.permissionFault("creating blob directory", root, errors.New("no space left on device"))
	var fault *PermissionFault
	if errors.As(err, &fault) {
		t.Fatalf("a non-permission failure was diagnosed as one: %v", err)
	}
	if !strings.Contains(err.Error(), "cas: creating blob directory: no space left on device") {
		t.Errorf("the error was not wrapped the way it always was: %v", err)
	}
}

// TestChainReportsAbsentLevels: which levels EXIST is half the diagnosis, so a
// chain that runs out partway through has to say so rather than stop.
func TestChainReportsAbsentLevels(t *testing.T) {
	root := t.TempDir()
	levels := chain(root, filepath.Join(root, "blobs", "blake3", "42", "c1"))
	if len(levels) != 5 {
		t.Fatalf("chain reported %d levels, want 5: %v", len(levels), levels)
	}
	if !strings.Contains(levels[0], root) {
		t.Errorf("the chain does not start at the store root: %v", levels[0])
	}
	for _, want := range []string{"blobs", "blake3", "42", "c1"} {
		found := false
		for _, level := range levels {
			if strings.HasPrefix(level, want) && strings.Contains(level, "absent") {
				found = true
			}
		}
		if !found {
			t.Errorf("the chain does not report %q as absent: %v", want, levels)
		}
	}
}
