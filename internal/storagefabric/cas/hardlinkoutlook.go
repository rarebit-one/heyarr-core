package cas

import (
	"io/fs"
	"os"
	"path/filepath"
)

// The instruments a LinkOutlook can be reached with, named in the outlook so a
// warning says what it is evidence OF. #222 is the reason that is a field and
// not a comment: the guard was wrong because of which instrument it used, and
// nothing in its output said which one that was.
const (
	// InstrumentProbe attempted the operation. Ground truth.
	InstrumentProbe = "probe: a real link(2) into the store"
	// InstrumentMount compared mount ids (st_dev off Linux, where the mount
	// split this guards against cannot occur). An inference.
	InstrumentMount = "inference: the mount table"
)

// LinkOutlook is whether ingest from a source directory into the store can use
// the hardlink rung of ADR-0014's ladder — and, as load-bearing as the answer,
// how that was established.
type LinkOutlook struct {
	// CanHardlink is meaningful only when Known.
	CanHardlink bool
	// Known is false when nothing could be established. A caller must not read
	// "unknown" as "different": warning about a problem that may not exist is
	// how a warning stops being read.
	Known bool
	// Instrument is which of the two above answered.
	Instrument string
	// Evidence is what it saw — an errno, or the mount ids it compared.
	Evidence string
}

// HardlinkOutlook reports whether a hardlink from srcDir into the store at
// casRoot can succeed, preferring to ATTEMPT one over predicting it.
//
// # Why a probe, when SameMount already exists
//
// Every previous instrument here answered a proxy question and was eventually
// wrong about the real one, in a way its author could not have anticipated:
//
//   - st_dev asked "one filesystem?". Under ProtectSystem=strict the store and
//     the library are two bind mounts of ONE filesystem, so st_dev matched on
//     precisely the host where every ingest was copying (#222). The guard was
//     structurally incapable of firing.
//   - mount ids ask "one vfsmount?", which is the comparison link(2) itself
//     makes — strictly better, and still an inference. It cannot see a link
//     refused for any OTHER reason: fs.protected_hardlinks on a source the
//     service neither owns nor may write, EMLINK at the filesystem's link
//     limit, EPERM from an immutable source, a future containment mechanism
//     nobody here has heard of.
//
// A probe asks the kernel the question the ingest will ask, and reads the
// answer instead of deriving it. It cannot be blind to a cause it did not
// model, because it does not model any: the only way it says "hardlink works"
// is that a hardlink worked, seconds earlier, between these two directories.
//
// It needs a source file to link, so it degrades to the mount inference when
// the library is empty or unreadable — which is the one arrangement where the
// inference is also harmless, because no bytes have been ingested yet.
//
// Side effects are a link and an unlink under the store's tmp/ directory, and
// a ctime bump on one source inode. The source is never written, moved or
// renamed; the library may be mounted read-only and the probe still works,
// because link(2) writes only at the destination.
func HardlinkOutlook(srcDir, casRoot string) (LinkOutlook, error) {
	return hardlinkOutlook(srcDir, casRoot, os.Link)
}

// hardlinkOutlook is HardlinkOutlook with the kernel call injected, so a test
// can present the one condition it cannot construct without privileges: a
// link(2) that returns EXDEV while both paths report the same st_dev.
func hardlinkOutlook(srcDir, casRoot string, link func(oldname, newname string) error) (LinkOutlook, error) {
	if outlook, probed := probeHardlink(srcDir, casRoot, link); probed {
		return outlook, nil
	}
	same, known, err := SameMount(casRoot, srcDir)
	if err != nil {
		return LinkOutlook{}, err
	}
	evidence := "the store and the source resolve to different mounts"
	if same {
		evidence = "the store and the source resolve to the same mount"
	}
	if !known {
		evidence = "the mount of one of the two paths could not be determined"
	}
	return LinkOutlook{
		CanHardlink: same,
		Known:       known,
		Instrument:  InstrumentMount,
		Evidence:    evidence,
	}, nil
}

// probeHardlink attempts one hardlink from a file under srcDir into the store,
// removes it, and reports what the kernel said. The second return is whether
// the probe could be run at all — false means "ask something else", never
// "hardlink is unavailable".
func probeHardlink(srcDir, casRoot string, link func(oldname, newname string) error) (LinkOutlook, bool) {
	dstDir, ok := probeDir(casRoot)
	if !ok {
		return LinkOutlook{}, false
	}
	src, ok := probeSource(srcDir, casRoot)
	if !ok {
		return LinkOutlook{}, false
	}

	// The control. Without it a failed link is ambiguous: an unwritable store
	// would be reported as "hardlinks do not work here", which is a true
	// sentence about a completely different and much worse problem. If a plain
	// file cannot be created either, the probe has learned nothing about
	// linking and says so.
	control, err := os.CreateTemp(dstDir, "hardlink-probe-*.part")
	if err != nil {
		return LinkOutlook{}, false
	}
	target := control.Name() + ".link"
	_ = control.Close()
	defer func() {
		_ = os.Remove(control.Name())
		_ = os.Remove(target)
	}()

	if err := link(src, target); err != nil {
		return LinkOutlook{
			CanHardlink: false,
			Known:       true,
			Instrument:  InstrumentProbe,
			// The errno, verbatim. "invalid cross-device link" from two paths
			// with one device number is the #222 signature, and no summary of
			// it would have been as convincing as the kernel's own word.
			Evidence: err.Error(),
		}, true
	}
	return LinkOutlook{
		CanHardlink: true,
		Known:       true,
		Instrument:  InstrumentProbe,
		Evidence:    "a test hardlink from the source directory into the store succeeded",
	}, true
}

// probeDir picks where to attempt the link. tmp/ is where every other
// half-finished write in this package goes, so a probe that is interrupted
// leaves something ReapTemp already knows how to clean up. Nothing is created:
// a store that does not exist yet is not probed.
func probeDir(casRoot string) (string, bool) {
	for _, dir := range []string{filepath.Join(casRoot, tmpDir), casRoot} {
		// #nosec G703 -- dir is the configured cas.root, or that root joined
		// with a constant; nothing here comes from a request, and the stat
		// only asks whether a directory is already there.
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir, true
		}
	}
	return "", false
}

// probeSourceBudget bounds the walk. The question is answered by ANY regular
// file, so this is a cap on a pathological layout — a library whose first
// thousands of entries are empty directories — not a sample size.
const probeSourceBudget = 4096

// probeSource finds one regular file under srcDir to link from.
//
// It refuses anything inside casRoot. A store nested in the library root — the
// arrangement the deployment guide recommends — would otherwise offer its own
// blobs as the probe source, and a blob linking into the store proves only
// that the store can link to itself, which is true in exactly the layout where
// the library cannot.
func probeSource(srcDir, casRoot string) (string, bool) {
	absSrc, err := filepath.Abs(srcDir)
	if err != nil {
		return "", false
	}
	absCAS, err := filepath.Abs(casRoot)
	if err != nil {
		return "", false
	}

	found := ""
	budget := probeSourceBudget
	err = filepath.WalkDir(absSrc, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is not a verdict. Skip it and keep looking.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if budget--; budget <= 0 {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if path == absCAS {
				return filepath.SkipDir
			}
			return nil
		}
		// Regular files only: a symlink would probe whatever it points at, and
		// a device node or socket answers a question nobody asked.
		if !d.Type().IsRegular() {
			return nil
		}
		found = path
		return filepath.SkipAll
	})
	if err != nil {
		// A library that is not there, or not readable at all, is not a
		// verdict either: the inference answers next, and the scan will
		// report the missing root far more usefully than a probe can.
		return "", false
	}
	return found, found != ""
}
