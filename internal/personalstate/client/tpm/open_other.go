//go:build !linux

package tpm

import (
	"fmt"
	"runtime"

	"github.com/google/go-tpm/tpm2/transport"
)

// openTPMDevice is unsupported off Linux: the kernel resource-manager device and
// go-tpm's linuxtpm transport are Linux-only, and the fleet's TPM-gated custody is
// a Linux concern (ADR-0098). The package still builds everywhere (so the codec
// and RecipientID are usable), but opening a real device fails clearly.
func openTPMDevice(_ string) (transport.TPMCloser, error) {
	return nil, fmt.Errorf("tpm: device access is only supported on Linux, not %s", runtime.GOOS)
}
