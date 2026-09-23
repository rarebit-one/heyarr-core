//go:build linux

package cas

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// mountNSEnv carries the fixture root to the copy of this test that runs
// inside a private mount namespace.
const mountNSEnv = "HEYARR_TEST_MOUNT_NS_BASE"

// mountNSProof is printed only after every assertion inside the namespace has
// passed. Without it a child that skipped — no permission to unshare — would
// exit 0 and the parent would report a proof that never happened, which is the
// exact failure mode #222 is a case of.
const mountNSProof = "MOUNT-NS-PROOF-COMPLETE"

// The real kernel condition from #222, constructed without root: ONE
// filesystem, TWO mounts, and a link(2) between them.
//
// Unprivileged user namespaces make this reproducible on an ordinary
// workstation — the test re-executes itself with CLONE_NEWUSER|CLONE_NEWNS,
// becomes root inside that namespace, and bind-mounts the library and the
// store onto themselves exactly as ProtectSystem=strict's ReadOnlyPaths and
// ReadWritePaths do. Inside, all three instruments are asked the same
// question, in one run, about one pair of directories:
//
//	st_dev        says "same filesystem"  — and is WRONG about link(2)
//	mount ids     say "different mounts"  — right
//	a real link() returns EXDEV           — right, and not a prediction
//
// A host that forbids unprivileged user namespaces (several distributions
// restrict them, and some CI images do) cannot run this, and the test SKIPS
// with the reason rather than passing quietly.
func TestHardlinkOutlookAcrossABindMountOfOneFilesystem(t *testing.T) {
	if base := os.Getenv(mountNSEnv); base != "" {
		proveInsideTheNamespace(t, base)
		return
	}

	base := t.TempDir()
	library := filepath.Join(base, "media")
	store := filepath.Join(base, "media", "heyarr", "cas")
	if err := os.MkdirAll(filepath.Join(store, tmpDir), 0o750); err != nil {
		t.Fatalf("creating the store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(library, "film.mkv"), []byte("bytes"), 0o600); err != nil {
		t.Fatalf("writing the library file: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = append(os.Environ(), mountNSEnv+"="+base)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		// Required when mapping a gid unprivileged.
		GidMappingsEnableSetgroups: false,
	}
	out, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(out), "--- FAIL") {
		t.Fatalf("inside a private mount namespace:\n%s", out)
	}
	if !strings.Contains(string(out), mountNSProof) {
		t.Skipf("this host cannot construct one filesystem with two mounts without privileges "+
			"(unprivileged user namespaces are restricted here), so the real #222 kernel "+
			"condition was NOT exercised — only its simulation in "+
			"TestTheProbeSeesALinkRefusalThatStDevAndTheMountTableCannot.\nerror: %v\noutput:\n%s",
			err, out)
	}
	t.Logf("proved against the real kernel:\n%s", out)
}

// proveInsideTheNamespace runs in the child, as root of a private user and
// mount namespace.
func proveInsideTheNamespace(t *testing.T, base string) {
	library := filepath.Join(base, "media")
	store := filepath.Join(base, "media", "heyarr", "cas")

	// Before any mounting: one filesystem, one mount, and the probe agrees.
	// The A of the A/B — asserted in the same run as the B, because a guard
	// that only ever says "fine" is how this went unnoticed for a release.
	before, err := HardlinkOutlook(library, store)
	if err != nil {
		t.Fatalf("HardlinkOutlook before mounting: %v", err)
	}
	if !before.CanHardlink || before.Instrument != InstrumentProbe {
		t.Fatalf("before mounting, a hardlink within one mount was reported impossible: %+v", before)
	}

	// Detach propagation, or the binds would travel back to the host's
	// namespace and outlive this process.
	if err := syscall.Mount("none", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		t.Skipf("cannot make the mount namespace private: %v", err)
	}
	// What ProtectSystem=strict does: the library and the store become
	// separate bind mounts OF THE SAME FILESYSTEM.
	if err := syscall.Mount(library, library, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		t.Skipf("cannot bind-mount the library: %v", err)
	}
	if err := syscall.Mount(store, store, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		t.Skipf("cannot bind-mount the store: %v", err)
	}
	// ReadOnlyPaths, faithfully: the library mount is read-only. The probe
	// must still work — link(2) writes only at the destination.
	roFlags := uintptr(syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_RDONLY |
		// The flags the parent mount already has are LOCKED in a user
		// namespace: a remount that drops one is refused, so they are carried.
		syscall.MS_NOSUID | syscall.MS_NODEV)
	if err := syscall.Mount("none", library, "", roFlags, ""); err != nil {
		t.Logf("could not remount the library read-only (%v); continuing with a writable source", err)
	}

	// 1. The old instrument. It must report "same" — that is the whole bug,
	//    and asserting it here is what makes this a regression test for the
	//    BLINDNESS rather than only for the fix.
	same, known, err := SameFilesystem(store, library)
	if err != nil || !known {
		t.Fatalf("SameFilesystem(%s, %s) = (%v, %v, %v)", store, library, same, known, err)
	}
	if !same {
		t.Fatalf("st_dev distinguished two bind mounts of one filesystem, which it cannot do — " +
			"this fixture is not reproducing #222")
	}

	// 2. The mount inference. It must report "different".
	same, known, err = SameMount(store, library)
	if err != nil || !known {
		t.Fatalf("SameMount(%s, %s) = (%v, %v, %v)", store, library, same, known, err)
	}
	if same {
		t.Fatalf("SameMount called two separate bind mounts the same mount")
	}

	// 3. The probe. It must be refused, by the kernel, with EXDEV — the same
	//    device, and still no hardlink.
	after, err := HardlinkOutlook(library, store)
	if err != nil {
		t.Fatalf("HardlinkOutlook after mounting: %v", err)
	}
	if after.Instrument != InstrumentProbe {
		t.Fatalf("instrument = %q, want the probe: %+v", after.Instrument, after)
	}
	if after.CanHardlink {
		t.Fatalf("the probe reported a hardlink possible across two mounts: %+v", after)
	}
	if !strings.Contains(after.Evidence, syscall.EXDEV.Error()) {
		t.Fatalf("evidence = %q, want the kernel's EXDEV (%q)", after.Evidence, syscall.EXDEV.Error())
	}

	t.Logf("same device, different mounts: st_dev said same, the probe said %q", after.Evidence)
	t.Log(mountNSProof)
}
