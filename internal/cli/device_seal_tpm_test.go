package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestDeviceSealTPMIsDisabled: seal-tpm refuses whatever it is given — a
// software device, any flags — names ADR-0021, and writes nothing.
func TestDeviceSealTPMIsDisabled(t *testing.T) {
	t.Parallel()
	dir := deviceDir(t)
	generateDevice(t, dir)
	out := filepath.Join(t.TempDir(), "tpm.blob")
	for _, args := range [][]string{
		{"device", "seal-tpm", "--device-dir", dir},
		{"device", "seal-tpm", "--device-dir", dir, "--out", out, "--pcr", "7", "--tpm-device", "/dev/null"},
	} {
		_, _, err := run(t, context.Background(), args...)
		if !errors.Is(err, errSealTPMDisabled) {
			t.Fatalf("%v: %v, want errSealTPMDisabled", args, err)
		}
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a disabled seal-tpm wrote a blob")
	}
}
