package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-tpm/tpm2/transport"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client/tpm"
)

func TestToUintPCRs(t *testing.T) {
	t.Parallel()
	got, err := toUintPCRs([]int{7, 0, 23})
	if err != nil {
		t.Fatalf("valid PCRs: %v", err)
	}
	if len(got) != 3 || got[0] != 7 || got[1] != 0 || got[2] != 23 {
		t.Fatalf("got %v, want [7 0 23]", got)
	}
	for _, bad := range [][]int{{-1}, {24}, {7, 99}, nil, {}} {
		if _, err := toUintPCRs(bad); err == nil {
			t.Errorf("toUintPCRs(%v): want an error", bad)
		}
	}
}

func TestSealDeviceKeyToTPMSurfacesOpenerError(t *testing.T) {
	t.Parallel()
	// The seal path can't reach a TPM in a unit test; a failing opener must be
	// surfaced (rather than the command panicking or writing an empty blob). The
	// happy path is proven against the reference simulator in the tpm package.
	failing := tpm.Opener(func() (transport.TPMCloser, error) { return nil, errors.New("no tpm here") })
	var buf bytes.Buffer
	err := sealDeviceKeyToTPM(&buf, make([]byte, 32), "123456", []uint{7}, failing, "/does/not/matter", "/seed")
	if err == nil || !strings.Contains(err.Error(), "no tpm here") {
		t.Fatalf("opener error: got %v, want it surfaced", err)
	}
	if buf.Len() != 0 {
		t.Errorf("wrote output despite the opener failing: %q", buf.String())
	}
}
