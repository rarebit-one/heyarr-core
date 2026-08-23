//go:build !windows

// The crash tests re-exec this test binary and SIGKILL it from the inside.
// syscall.Kill does not exist on Windows, and a "kill" that unwinds the
// process cleanly would test the opposite of what this file is for, so the
// file is built only where a real, unstoppable kill is available.

package integrity_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// Environment the parent passes to the child process.
const (
	envCrashAt = "HEYARR_REPAIR_CRASH_AT"
	envCASRoot = "HEYARR_REPAIR_CAS"
	envGood    = "HEYARR_REPAIR_GOOD"
)

// The kill points, which are the steps of ADR-0036's sequence.
const (
	crashAfterStaging    = "after_staging"
	crashMidAssembly     = "mid_assembly"
	crashAfterVerify     = "after_verify"
	crashAfterQuarantine = "after_quarantine"
)

// TestRepairSurvivesBeingKilledInsideTheRepairWindow is the assertion this
// issue exists for.
//
// ADR-0036 says the store is in exactly one of two states at every instant —
// the pre-repair one or the post-repair one — plus a reapable staging file,
// and it says explicitly that the way to establish that is to kill the process
// rather than to argue that the rename is atomic. So this test runs the repair
// in a CHILD PROCESS and SIGKILLs it from the inside at each step: no defers
// run, no buffers flush, no cleanup happens. Then the parent opens the store
// the child left behind and reads it back.
//
// What is asserted after every kill:
//
//   - the blob is exactly the old bytes or exactly the new bytes, compared by
//     sha256 against backups the parent took first — never anything in
//     between;
//   - Has and Verify agree with each other and with what is on disk;
//   - nothing in the addressable tree answers to a digest it does not have;
//   - the staging residue is reapable, and was never addressable.
func TestRepairSurvivesBeingKilledInsideTheRepairWindow(t *testing.T) {
	if os.Getenv(envCrashAt) != "" {
		t.Skip("this process is the child of a crash test")
	}

	for _, tc := range []struct {
		point string
		// blobSurvives is whether the blob is still addressable after the
		// kill. It is false for exactly one point — between quarantine and
		// publish — and that asymmetry is the design: a crash there loses the
		// REPAIR and keeps the EVIDENCE (ADR-0018, ADR-0036).
		blobSurvives    bool
		quarantineHolds bool
		// stagingComplete is whether the reconstruction had been finished and
		// verified by the time the process died. It is what proves the kill
		// landed where the case says it did rather than somewhere convenient:
		// a "killed after verification" case whose staging file is a partial
		// assembly was not killed after verification.
		stagingComplete bool
	}{
		{point: crashAfterStaging, blobSurvives: true, quarantineHolds: false, stagingComplete: false},
		{point: crashMidAssembly, blobSurvives: true, quarantineHolds: false, stagingComplete: false},
		{point: crashAfterVerify, blobSurvives: true, quarantineHolds: false, stagingComplete: true},
		{point: crashAfterQuarantine, blobSurvives: false, quarantineHolds: true, stagingComplete: true},
	} {
		t.Run(tc.point, func(t *testing.T) {
			dir := t.TempDir()
			root := filepath.Join(dir, "cas")
			store, err := cas.OpenFS(root)
			if err != nil {
				t.Fatal(err)
			}

			// The good bytes, on disk for the child to serve from, and their
			// sha256 — taken BEFORE anything is damaged.
			content := pseudoRandom(4242, 32<<10)
			goodPath := filepath.Join(dir, "good.bin")
			if err := os.WriteFile(goodPath, content, 0o600); err != nil {
				t.Fatal(err)
			}
			desc, err := store.Put(t.Context(), bytes.NewReader(content))
			if err != nil {
				t.Fatal(err)
			}
			h := desc.Hash
			goodSum := sha256Bytes(content)

			m := buildManifest(t, h, content)
			if m.ChunkCount() < 8 {
				t.Fatalf("fixture produced %d chunks, want enough for two damaged ones", m.ChunkCount())
			}
			blobPath := blobFileIn(t, root, h)
			// Two damaged chunks, so there is a fetch to be killed on and a
			// second fetch to be killed halfway through the assembly on.
			damageFile(t, blobPath, m.Chunks[1].Offset+3, []byte("damage one"))
			damageFile(t, blobPath, m.Chunks[5].Offset+7, []byte("damage two"))
			damagedSum := sha256File(t, blobPath)
			if damagedSum == goodSum {
				t.Fatal("the fixture did not actually damage anything")
			}

			runChildAndExpectAKill(t, tc.point, root, goodPath)

			// --- read the store back ------------------------------------
			reopened, err := cas.OpenFS(root)
			if err != nil {
				t.Fatal(err)
			}
			present := blobFileIn(t, root, h) != ""
			if present != tc.blobSurvives {
				t.Errorf("after a kill at %s the blob is present=%v, want %v",
					tc.point, present, tc.blobSurvives)
			}
			has, err := reopened.Has(t.Context(), h)
			if err != nil {
				t.Fatal(err)
			}
			if has != present {
				t.Errorf("Has says %v and the file says %v — they must agree", has, present)
			}

			var onDisk string
			if present {
				onDisk = sha256File(t, blobFileIn(t, root, h))
				switch onDisk {
				case damagedSum, goodSum:
				default:
					t.Fatalf("the blob is neither its pre-repair self nor its post-repair self "+
						"after a kill at %s — there is a third state", tc.point)
				}
			}

			// The evidence. A crash between quarantine and publication must
			// have cost the repair and kept the damaged bytes.
			quarantined := quarantinedIn(t, root)
			if tc.quarantineHolds {
				if len(quarantined) != 1 {
					t.Fatalf("quarantine holds %d artefacts after a kill at %s, want the "+
						"damaged original", len(quarantined), tc.point)
				}
				if sha256File(t, quarantined[0]) != damagedSum {
					t.Error("the quarantined artefact is not the damaged bytes")
				}
				if _, err := os.ReadFile(quarantined[0]); err != nil { // #nosec G304 -- a test path
					t.Errorf("the preserved evidence is not readable: %v", err)
				}
			} else if len(quarantined) != 0 {
				t.Errorf("a kill at %s quarantined something: %v", tc.point, quarantined)
			}

			// The staging residue. It must exist (the child died holding it),
			// it must never have been addressable, and ReapTemp must clean it
			// up — an interrupted repair leaves waste, not damage.
			assertOnlyBlobFilesAreAddressable(t, root, h, damagedSum, goodSum)
			temps, err := reopened.TempFiles()
			if err != nil {
				t.Fatal(err)
			}
			if len(temps) != 1 {
				t.Fatalf("a kill at %s left %d staging files, want exactly the one "+
					"reconstruction ADR-0036 says the repair writes", tc.point, len(temps))
			}
			// Where the kill actually landed, read off the staging file
			// itself rather than trusted from the case name.
			stagingSum := sha256File(t, filepath.Join(root, "tmp", temps[0].Name))
			if complete := stagingSum == goodSum; complete != tc.stagingComplete {
				t.Errorf("a kill at %s left a staging file that is complete=%v, want %v",
					tc.point, complete, tc.stagingComplete)
			}
			reaped, err := reopened.ReapTemp(0)
			if err != nil {
				t.Fatal(err)
			}
			if reaped != len(temps) {
				t.Errorf("reaped %d of %d staging files; the residue must be reapable",
					reaped, len(temps))
			}
			after, err := reopened.TempFiles()
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != 0 {
				t.Errorf("%d staging files survived the reap", len(after))
			}

			// Verify agrees with Has and with the bytes. LAST, and
			// deliberately so: Verify quarantines what it finds corrupt
			// (ADR-0018), so running it earlier would change the very
			// quarantine state the assertions above read.
			vErr := reopened.Verify(t.Context(), h)
			var corrupt *cas.Corruption
			switch {
			case !present && !errors.Is(vErr, cas.ErrNotFound):
				t.Errorf("the blob is gone and Verify reports %v, want ErrNotFound", vErr)
			case present && onDisk == goodSum && vErr != nil:
				t.Errorf("the store holds the repaired bytes and Verify disagrees: %v", vErr)
			case present && onDisk == damagedSum && !errors.As(vErr, &corrupt):
				t.Errorf("the store holds the damaged bytes and Verify did not say so: %v", vErr)
			}
		})
	}
}

// Nothing is addressable mid-repair: while the reconstruction is in flight the
// blob's digest resolves to the ORIGINAL bytes, and no partially assembled
// file answers to any digest at all.
//
// This is the same claim the crash test makes, asserted from inside the window
// rather than after it: the child is killed with the assembly part-written,
// and the parent then reads the blob through the store's own addressing.
func TestNothingIsAddressableWhileARepairIsInFlight(t *testing.T) {
	if os.Getenv(envCrashAt) != "" {
		t.Skip("this process is the child of a crash test")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "cas")
	store, err := cas.OpenFS(root)
	if err != nil {
		t.Fatal(err)
	}
	content := pseudoRandom(777, 32<<10)
	goodPath := filepath.Join(dir, "good.bin")
	if err := os.WriteFile(goodPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	desc, err := store.Put(t.Context(), bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	h := desc.Hash
	m := buildManifest(t, h, content)
	damageFile(t, blobFileIn(t, root, h), m.Chunks[1].Offset+3, []byte("damage one"))
	damageFile(t, blobFileIn(t, root, h), m.Chunks[5].Offset+7, []byte("damage two"))
	damagedBytes, err := os.ReadFile(blobFileIn(t, root, h)) // #nosec G304 -- a test path
	if err != nil {
		t.Fatal(err)
	}

	runChildAndExpectAKill(t, crashMidAssembly, root, goodPath)

	reopened, err := cas.OpenFS(root)
	if err != nil {
		t.Fatal(err)
	}
	rc, _, err := reopened.Open(t.Context(), h)
	if err != nil {
		t.Fatalf("the blob's digest no longer resolves at all: %v", err)
	}
	defer func() { _ = rc.Close() }()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), damagedBytes) {
		t.Error("a reader opening the blob mid-repair got something other than the original " +
			"bytes — during the window the addressable blob is the original, unchanged")
	}

	// And the half-assembled file answers to nothing: the store walks only
	// what is addressable, and the staging file is not in it.
	seen := 0
	if err := reopened.Walk(t.Context(), func(cas.Descriptor) error { seen++; return nil }); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Errorf("the store walks %d addressable files mid-repair, want exactly the one blob", seen)
	}
}

// runChildAndExpectAKill re-execs this test binary to perform one repair and
// die inside it, and fails unless the child really was killed by a signal.
//
// The last part matters: a child that returned an error, or that exited
// cleanly, would leave the store in a tidy state and every assertion after it
// would be vacuous.
func runChildAndExpectAKill(t *testing.T, point, root, goodPath string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], // #nosec G204 -- this test binary
		"-test.run=TestRepairCrashChild", "-test.v")
	cmd.Env = append(os.Environ(),
		envCrashAt+"="+point,
		envCASRoot+"="+root,
		envGood+"="+goodPath,
	)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the child did not die (err=%v):\n%s", err, out)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("the child exited %v rather than being SIGKILLed:\n%s", exit, out)
	}
}

// TestRepairCrashChild is the child process. It runs one real repair against
// the store the parent prepared and kills itself at the requested step.
//
// It skips when the environment does not name a kill point, which is what
// makes it invisible to an ordinary `go test` run.
func TestRepairCrashChild(t *testing.T) {
	point := os.Getenv(envCrashAt)
	if point == "" {
		t.Skip("not a crash-test child: " + envCrashAt + " is unset")
	}
	root, goodPath := os.Getenv(envCASRoot), os.Getenv(envGood)
	content, err := os.ReadFile(goodPath) // #nosec G304 -- a path from the parent test
	if err != nil {
		t.Fatal(err)
	}
	store, err := cas.OpenFS(root)
	if err != nil {
		t.Fatal(err)
	}
	h, _, err := hashing.HashFile(goodPath)
	if err != nil {
		t.Fatal(err)
	}

	man := newFakeManifests()
	if err := man.Save(t.Context(), buildManifest(t, h, content)); err != nil {
		t.Fatal(err)
	}
	peer := newFakePeer()
	peer.hold(h, content)

	// The kill points are reached through the collaborators the repairer
	// already declares — the chunk source and the store — rather than through
	// a test hook in the production path. A seam that only exists for tests is
	// a seam the shipped code does not have.
	switch point {
	case crashAfterStaging:
		// The staging file exists and the first replacement has not been
		// written yet.
		peer.onFetch = func(n int) {
			if n == 1 {
				killSelf()
			}
		}
	case crashMidAssembly:
		// One replacement chunk is in the staging file, several intact ones
		// are copied, and the assembly is incomplete.
		peer.onFetch = func(n int) {
			if n == 2 {
				killSelf()
			}
		}
	}

	crashing := &crashStore{FS: store}
	switch point {
	case crashAfterVerify:
		// The assembly is complete and has been verified against the blob's
		// digest; nothing has been quarantined and nothing published.
		crashing.killBeforeQuarantine = true
	case crashAfterQuarantine:
		// The damaged original is in quarantine and the replacement has not
		// been published. This is the window ADR-0018's ordering exists for.
		crashing.killAfterQuarantine = true
	}

	repairer, err := integrity.NewRepairer(integrity.RepairOptions{
		Store: crashing, Manifests: man, Catalog: newFakeCatalog(), Source: peer, Clock: newClock(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := repairer.Repair(t.Context(), h)
	// Unreachable if the kill point was reached, which is the point.
	t.Fatalf("the repair finished without being killed at %s: %+v (err=%v)", point, result, err)
}

// crashStore is the real store with a kill wired either side of the
// quarantine step.
type crashStore struct {
	*cas.FS
	killBeforeQuarantine bool
	killAfterQuarantine  bool
}

func (c *crashStore) Quarantine(h hashing.Hash) (string, error) {
	if c.killBeforeQuarantine {
		killSelf()
	}
	path, err := c.FS.Quarantine(h)
	if c.killAfterQuarantine {
		killSelf()
	}
	return path, err
}

// killSelf is a real, uncatchable process kill: no deferred cleanup, no
// flushed buffers, no chance for the repairer to tidy up on the way out.
func killSelf() {
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	// Not reached. Present so the compiler and a reader both know this
	// function does not return normally.
	select {}
}

// --- helpers that work on a store root rather than on a fixture -------------

// blobFileIn finds a blob's file under root, or returns "" if it is not there.
func blobFileIn(t *testing.T, root string, h hashing.Hash) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(filepath.Join(root, "blobs"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() && d.Name() == h.Hex() {
			found = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func quarantinedIn(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "quarantine"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, filepath.Join(root, "quarantine", e.Name()))
	}
	return out
}

// assertOnlyBlobFilesAreAddressable checks that the addressable tree holds
// nothing the repair put there under a name it does not have, and that no
// staging file was published under any name.
func assertOnlyBlobFilesAreAddressable(t *testing.T, root string, h hashing.Hash, damagedSum, goodSum string) {
	t.Helper()
	err := filepath.WalkDir(filepath.Join(root, "blobs"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() != h.Hex() {
			t.Errorf("the addressable tree gained %s, which is not the blob under repair", path)
			return nil
		}
		if sum := sha256File(t, path); sum != damagedSum && sum != goodSum {
			t.Errorf("%s holds bytes that are neither the pre-repair nor the post-repair file", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
