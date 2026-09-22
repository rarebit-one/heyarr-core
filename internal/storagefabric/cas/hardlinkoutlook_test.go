package cas

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// library builds a source directory holding one file, and a store directory
// beside it, both in one temp dir — the layout a correctly deployed host has.
func library(t *testing.T) (src, casRoot string) {
	t.Helper()
	base := t.TempDir()
	src = filepath.Join(base, "media")
	casRoot = filepath.Join(base, "store")
	for _, d := range []string{src, filepath.Join(casRoot, tmpDir)} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("creating %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "film.mkv"), []byte("bytes"), 0o600); err != nil {
		t.Fatalf("writing the probe source: %v", err)
	}
	return src, casRoot
}

// The instrument's own contract: when a hardlink is possible it says so, and
// says it learned that by trying, not by predicting.
func TestHardlinkOutlookProbesWhenThereIsAFileToLink(t *testing.T) {
	src, casRoot := library(t)

	outlook, err := HardlinkOutlook(src, casRoot)
	if err != nil {
		t.Fatalf("HardlinkOutlook: %v", err)
	}
	if !outlook.Known {
		t.Fatalf("the outlook is unknown for two directories in one temp dir: %+v", outlook)
	}
	if !outlook.CanHardlink {
		t.Errorf("a hardlink within one filesystem was reported impossible: %+v", outlook)
	}
	if outlook.Instrument != InstrumentProbe {
		t.Errorf("instrument = %q, want the probe — a predictable case is still answered by trying", outlook.Instrument)
	}
}

// The probe must leave the filesystem as it found it: no stray link in the
// store, and the source back to one name. A diagnostic that accumulates files
// in the CAS is worse than the blindness it replaced.
func TestTheProbeCleansUpAfterItself(t *testing.T) {
	src, casRoot := library(t)
	source := filepath.Join(src, "film.mkv")

	if _, err := HardlinkOutlook(src, casRoot); err != nil {
		t.Fatalf("HardlinkOutlook: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(casRoot, tmpDir))
	if err != nil {
		t.Fatalf("reading the store's tmp dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the probe left %v behind in the store", names)
	}

	// Platform-split rather than guarded by runtime.GOOS: syscall.Stat_t does
	// not exist on Windows at compile time, so a runtime branch still fails vet.
	if names, known := sourceLinkCount(t, source); known && names != 1 {
		t.Errorf("the source has %d names after the probe, want 1 — the probe's link was not removed", names)
	}
}

// THE test this whole change exists for, and the one the old guard fails.
//
// The condition is a link(2) that is refused while BOTH paths report the same
// st_dev — #222's ProtectSystem=strict bind mounts. A real one needs mount
// privileges, so here it is constructed at the seam: the two directories are
// genuinely in one filesystem (so the old instruments answer "same", and this
// test asserts that they do, rather than taking it on trust) and the injected
// link returns the errno the kernel returns across mounts.
//
// It is therefore NOT proof that the kernel behaves as described — that is
// TestHardlinkOutlookAcrossABindMountOfOneFilesystem's job, which mounts for
// real. What it proves is the half that was actually broken: that given that
// errno, the guard now reaches the right verdict, where st_dev structurally
// could not.
func TestTheProbeSeesALinkRefusalThatStDevAndTheMountTableCannot(t *testing.T) {
	src, casRoot := library(t)

	// The blind spot, asserted rather than assumed: on this pair both
	// inferences say "same", so any guard built on either of them is silent
	// here no matter what link(2) would actually do.
	if same, known, err := SameFilesystem(casRoot, src); err != nil || !known || !same {
		t.Fatalf("SameFilesystem(%s, %s) = (%v, %v, %v), want same — "+
			"the premise of this test is that the old instrument sees nothing wrong",
			casRoot, src, same, known, err)
	}
	if same, known, err := SameMount(casRoot, src); err != nil || !known || !same {
		t.Fatalf("SameMount(%s, %s) = (%v, %v, %v), want same", casRoot, src, same, known, err)
	}

	tests := []struct {
		name        string
		linkErr     error
		wantCan     bool
		wantKnown   bool
		wantEvidenc string
	}{
		{
			// The #222 kernel answer: two mounts of one filesystem.
			name:        "EXDEV from two mounts of one filesystem",
			linkErr:     &os.LinkError{Op: "link", Err: syscall.EXDEV},
			wantKnown:   true,
			wantEvidenc: syscall.EXDEV.Error(),
		},
		{
			// Not a mount question at all, and no inference models it:
			// fs.protected_hardlinks refusing a source the service neither
			// owns nor may write. The first hypothesis in #222, refuted there
			// — but on another host it is the true cause, and the probe is the
			// only instrument here that would report it.
			name:        "EPERM from protected_hardlinks",
			linkErr:     &os.LinkError{Op: "link", Err: syscall.EPERM},
			wantKnown:   true,
			wantEvidenc: syscall.EPERM.Error(),
		},
		{
			name:        "the link succeeds",
			linkErr:     nil,
			wantCan:     true,
			wantKnown:   true,
			wantEvidenc: "succeeded",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outlook, err := hardlinkOutlook(src, casRoot, func(_, newname string) error {
				if tt.linkErr != nil {
					return tt.linkErr
				}
				return os.WriteFile(newname, []byte("linked"), 0o600)
			})
			if err != nil {
				t.Fatalf("hardlinkOutlook: %v", err)
			}
			if outlook.Known != tt.wantKnown || outlook.CanHardlink != tt.wantCan {
				t.Fatalf("outlook = %+v, want CanHardlink=%v Known=%v", outlook, tt.wantCan, tt.wantKnown)
			}
			if outlook.Instrument != InstrumentProbe {
				t.Errorf("instrument = %q, want the probe", outlook.Instrument)
			}
			if !strings.Contains(outlook.Evidence, tt.wantEvidenc) {
				t.Errorf("evidence = %q, want it to name %q — an operator has to be able to "+
					"tell which refusal this was", outlook.Evidence, tt.wantEvidenc)
			}
		})
	}
}

// No file to link means no probe. Falling back to the inference is correct;
// reporting the fallback AS a probe would be the same lie in a new place.
func TestHardlinkOutlookFallsBackToTheMountTableWhenThereIsNothingToLink(t *testing.T) {
	_, casRoot := library(t)
	empty := t.TempDir()

	outlook, err := HardlinkOutlook(empty, casRoot)
	if err != nil {
		t.Fatalf("HardlinkOutlook: %v", err)
	}
	if outlook.Instrument != InstrumentMount {
		t.Errorf("instrument = %q, want the mount inference for an empty source", outlook.Instrument)
	}
	if !outlook.Known {
		t.Errorf("the inference could not answer for two real directories: %+v", outlook)
	}
}

// A store nested inside the library root is the layout the deploy guide
// recommends, and it is a trap for the probe: the store's own blobs are files
// under the library root, and linking a blob into the store proves nothing
// about the library. The probe must not use one.
func TestTheProbeWillNotLinkTheStoresOwnBlobs(t *testing.T) {
	base := t.TempDir()
	libraryRoot := filepath.Join(base, "media")
	casRoot := filepath.Join(libraryRoot, "heyarr", "cas")
	if err := os.MkdirAll(filepath.Join(casRoot, tmpDir), 0o750); err != nil {
		t.Fatalf("creating the store: %v", err)
	}
	// A blob, and nothing else anywhere under the library root.
	if err := os.WriteFile(filepath.Join(casRoot, "blob"), []byte("stored"), 0o600); err != nil {
		t.Fatalf("writing a blob: %v", err)
	}

	outlook, err := HardlinkOutlook(libraryRoot, casRoot)
	if err != nil {
		t.Fatalf("HardlinkOutlook: %v", err)
	}
	if outlook.Instrument != InstrumentMount {
		t.Errorf("instrument = %q, want the inference — the only candidate source was a blob "+
			"in the store, which would have answered a different question", outlook.Instrument)
	}
}

// A real, unfaked EXDEV: two filesystems that exist on any Linux host. This is
// the weaker half of the kernel evidence — different device AND different
// mount — but it is a genuine errno from a genuine link(2), which is what
// distinguishes the probe from every inference.
func TestTheProbeReadsARealCrossDeviceRefusal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("no second filesystem that is reliably present off Linux")
	}
	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skipf("/dev/shm is not available, so there is no second filesystem to link across: %v", err)
	}
	casRoot := t.TempDir()
	if same, known, err := SameFilesystem(casRoot, "/dev/shm"); err != nil || !known || same {
		t.Skipf("the temp dir and /dev/shm are one filesystem here (same=%v known=%v err=%v), "+
			"so there is no cross-device pair to probe", same, known, err)
	}
	src, err := os.MkdirTemp("/dev/shm", "heyarr-probe-src-*")
	if err != nil {
		t.Skipf("cannot create a source directory on /dev/shm: %v", err)
	}
	defer func() { _ = os.RemoveAll(src) }()
	if err := os.WriteFile(filepath.Join(src, "film.mkv"), []byte("bytes"), 0o600); err != nil {
		t.Fatalf("writing the probe source: %v", err)
	}

	outlook, err := HardlinkOutlook(src, casRoot)
	if err != nil {
		t.Fatalf("HardlinkOutlook: %v", err)
	}
	if outlook.Instrument != InstrumentProbe {
		t.Fatalf("instrument = %q, want the probe", outlook.Instrument)
	}
	if outlook.CanHardlink || !outlook.Known {
		t.Fatalf("a link across two filesystems was reported possible: %+v", outlook)
	}
	t.Logf("the kernel's own words: %s", outlook.Evidence)
}
