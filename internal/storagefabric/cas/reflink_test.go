package cas

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ADR-0014 claims that on a filesystem with block cloning, adopting a large
// file into the store costs metadata rather than bytes. That claim decides
// whether Heyarr is adoptable against an existing library or whether it demands
// the operator double their storage first — so it is measured, not assumed.
//
// The measurement is free space on the volume, not `du`. On APFS `du` reports
// the full logical size for every clone, so it shows a 100% "growth" for an
// operation that consumed nothing; the first version of this test believed it
// and reported a failure that was not real.
//
// The test validates its own instrument before trusting it: an ordinary copy
// must register, otherwise the measurement is too noisy and the result is
// skipped rather than reported as a pass.
func TestReflinkCostsMetadataNotBytes(t *testing.T) {
	if testing.Short() {
		t.Skip("writes hundreds of megabytes")
	}

	dir := t.TempDir()
	store, err := OpenFS(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}

	const sizeMiB = 256
	src := filepath.Join(dir, "big.bin")
	writeIncompressible(t, src, sizeMiB)

	// Control: an ordinary copy must show up, or the instrument is unusable.
	controlSrc := filepath.Join(dir, "control.bin")
	writeIncompressible(t, controlSrc, sizeMiB)
	before := freeKiB(t, dir)
	if err := copyFileForTest(controlSrc, filepath.Join(dir, "control-copy.bin")); err != nil {
		t.Fatal(err)
	}
	syncDisk()
	controlDelta := before - freeKiB(t, dir)
	if controlDelta < (sizeMiB*1024)/2 {
		t.Skipf("free-space measurement is too noisy to trust: an ordinary %d MiB copy "+
			"registered only %d KiB, so a clone measurement would prove nothing", sizeMiB, controlDelta)
	}

	// The measurement that matters.
	before = freeKiB(t, dir)
	desc, err := store.Link(t.Context(), src, Reflink)
	if err != nil {
		t.Fatal(err)
	}
	syncDisk()
	cloneDelta := before - freeKiB(t, dir)

	t.Logf("materialised as %s: a %d MiB file consumed %d KiB (an ordinary copy consumed %d KiB)",
		desc.Materialised, sizeMiB, cloneDelta, controlDelta)

	if desc.Materialised != Reflink {
		t.Skipf("this filesystem degraded to %s, so there is no cloning to measure", desc.Materialised)
	}
	// Generous threshold: the point is order-of-magnitude, not exactness, and
	// other processes share the volume.
	if cloneDelta > controlDelta/2 {
		t.Errorf("a clone consumed %d KiB against a copy's %d KiB — cloning is not saving space, "+
			"so ADR-0014's premise does not hold on this filesystem", cloneDelta, controlDelta)
	}
}

func writeIncompressible(t *testing.T, path string, sizeMiB int) {
	t.Helper()
	f, err := os.Create(path) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	// Compressible data would let a filesystem with transparent compression
	// make the control look free too, which would silently invalidate the test.
	buf := make([]byte, 1<<20)
	var seed uint64 = 0x9E3779B97F4A7C15
	for range sizeMiB {
		for i := range buf {
			seed = seed*6364136223846793005 + 1442695040888963407
			buf[i] = byte(seed >> 33)
		}
		if _, err := f.Write(buf); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
}

func copyFileForTest(src, dst string) error {
	data, err := os.ReadFile(src) // #nosec G304 -- test-controlled path
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}

// freeKiB reports free space on the volume containing path.
func freeKiB(t *testing.T, path string) int64 {
	t.Helper()
	out, err := exec.Command("df", "-k", path).Output()
	if err != nil {
		t.Skipf("df unavailable: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		t.Skipf("unexpected df output: %q", out)
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		t.Skipf("unexpected df output: %q", out)
	}
	free, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		t.Skipf("could not parse df output %q: %v", out, err)
	}
	return free
}

func syncDisk() {
	_ = exec.Command("sync").Run()
}
