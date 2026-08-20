package cas

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSameFilesystemRecognisesOneFilesystem(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(p, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	same, known, err := SameFilesystem(a, b)
	if err != nil {
		t.Fatalf("SameFilesystem: %v", err)
	}
	if runtime.GOOS == "windows" {
		if known {
			t.Error("windows reported a device number, which device_windows.go says it does not")
		}
		return
	}
	if !known {
		t.Fatal("could not determine the filesystem of two directories in one temp dir")
	}
	if !same {
		t.Error("two directories in the same temp dir were reported as different filesystems")
	}
}

// The case the check exists for. /proc is always a different filesystem from a
// temp dir on Linux, and /dev is on macOS — so there is a real cross-device
// pair to assert on without mounting anything.
func TestSameFilesystemRecognisesTwoFilesystems(t *testing.T) {
	var other string
	switch runtime.GOOS {
	case "linux":
		other = "/proc"
	case "darwin":
		other = "/dev"
	default:
		t.Skip("no reliably distinct filesystem to compare against on this platform")
	}
	if _, err := os.Stat(other); err != nil {
		t.Skipf("%s is not available: %v", other, err)
	}

	same, known, err := SameFilesystem(t.TempDir(), other)
	if err != nil {
		t.Fatalf("SameFilesystem: %v", err)
	}
	if !known {
		t.Fatal("device numbers were not available")
	}
	if same {
		t.Errorf("a temp dir and %s were reported as the same filesystem — the check cannot "+
			"detect the case it exists for, which is a CAS and a library on different disks", other)
	}
}

func TestSameFilesystemReportsAMissingPathRatherThanGuessing(t *testing.T) {
	// Guessing "different" would warn about a problem that may not exist;
	// guessing "same" would hide one. Neither is acceptable, so it errors.
	_, _, err := SameFilesystem(t.TempDir(), filepath.Join(t.TempDir(), "definitely-not-here"))
	if err == nil {
		t.Fatal("a missing path was answered rather than reported")
	}
}
