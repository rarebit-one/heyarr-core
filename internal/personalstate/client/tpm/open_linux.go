//go:build linux

package tpm

import (
	"fmt"

	"github.com/google/go-tpm/tpm2/transport"
	"github.com/google/go-tpm/tpm2/transport/linuxtpm"
)

// openTPMDevice opens the Linux kernel TPM resource-manager device.
func openTPMDevice(path string) (transport.TPMCloser, error) {
	t, err := linuxtpm.Open(path)
	if err != nil {
		return nil, fmt.Errorf("tpm: opening %s (is a TPM present?): %w", path, err)
	}
	return t, nil
}
