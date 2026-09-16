// Package custody selects the device-key custody backend a device uses to open
// (unwrap) space keys sealed for it (ADR-0098). The device gateway and the vault
// CLI call [Select] and get a [client.Custody] — the backend's Unwrapper plus the
// wrap-target id to look the sealed copy up by — without either caller knowing
// which backend it is. This is the "select a backend by configuration, not by
// code change" the ADR calls for.
package custody

import (
	"crypto/ecdh"
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client/yubikey"
)

// The selectable backend names (ADR-0098). Software and YubiKey are wired; TPM
// (#570) and cruciform-offload (#571) are built or building but not yet
// selectable, and Select returns a clear error naming why rather than a confusing
// "unknown backend".
const (
	Software  = "software"
	YubiKey   = "yubikey"
	TPM       = "tpm"
	Cruciform = "cruciform"
)

// Options selects and configures a backend.
type Options struct {
	// Backend is the selector; "" is treated as Software.
	Backend string
	// DeviceDir is the device key directory the Software backend loads its X25519
	// key from. "" resolves the platform default (device.DefaultDir).
	DeviceDir string
	// YubiKeySocket is the gpg-agent Assuan socket the YubiKey backend dials. ""
	// discovers it via gpgconf.
	YubiKeySocket string
	// YubiKeyPIN supplies the card User PIN the YubiKey backend presents before an
	// on-card decipher. Required when Backend is YubiKey; never persisted here.
	YubiKeyPIN yubikey.PINFunc
}

// Select builds the configured custody backend, or reports why it cannot.
func Select(opts Options) (client.Custody, error) {
	switch opts.Backend {
	case "", Software:
		priv, err := loadSoftwareKey(opts.DeviceDir)
		if err != nil {
			return nil, err
		}
		return client.NewKeyUnwrapper(priv), nil
	case YubiKey:
		if opts.YubiKeyPIN == nil {
			return nil, fmt.Errorf("custody: the yubikey backend needs a PIN source")
		}
		return yubikey.New(opts.YubiKeySocket, opts.YubiKeyPIN)
	case TPM:
		return nil, fmt.Errorf("custody: the %q backend is not built yet (#570)", TPM)
	case Cruciform:
		return nil, fmt.Errorf("custody: the %q backend is not selectable yet — its wake/relay transport is not wired (#571)", Cruciform)
	default:
		return nil, fmt.Errorf("custody: unknown backend %q", opts.Backend)
	}
}

// loadSoftwareKey opens this machine's device store and loads its X25519
// encryption private key — the exportable key the software backend unwraps with.
func loadSoftwareKey(dir string) (*ecdh.PrivateKey, error) {
	if dir == "" {
		resolved, err := device.DefaultDir()
		if err != nil {
			return nil, err
		}
		dir = resolved
	}
	ds, err := device.NewStore(device.StoreOptions{Dir: dir})
	if err != nil {
		return nil, err
	}
	priv, err := ds.LoadEncryptionKey()
	if err != nil {
		return nil, fmt.Errorf("custody: loading this device's software encryption key: %w", err)
	}
	return priv, nil
}
