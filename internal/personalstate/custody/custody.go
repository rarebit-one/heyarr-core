// Package custody selects the device-key custody backend a device uses to open
// (unwrap) space keys sealed for it (ADR-0098). The device gateway and the vault
// CLI call [Select] and get a [client.Custody] — the backend's Unwrapper plus the
// wrap-target id to look the sealed copy up by — without either caller knowing
// which backend it is. This is the "select a backend by configuration, not by
// code change" the ADR calls for.
//
// The software backend is this device's own key, wherever the device store
// keeps it: its seed file, or — for a device generated with
// `heyarr device generate --custody sealedfile` — the passphrase-sealed file
// beside its record (void-which-binds-go ADR-0021, through devicekeys). A
// custody device's seed is never read from a file, because it has none.
//
// This package keeps its name although void-which-binds-go now has a custody
// package too (ADR-0021 records the choice); it imports that one as vbcustody.
package custody

import (
	"fmt"
	"time"

	vbcustody "github.com/rarebit-one/void-which-binds-go/custody"

	"github.com/rarebit-one/heyarr-core/internal/device/devicekeys"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client/cruciform"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client/tpm"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client/yubikey"
)

// The selectable backend names (ADR-0098). Software, YubiKey and TPM are wired;
// cruciform-offload (#571) is built but not yet selectable, and Select returns a
// clear error naming why rather than a confusing "unknown backend".
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
	// DevicePassphrase yields the passphrase of a custody device's sealed file
	// (ADR-0021), asked only when an unwrap needs it. Nil uses
	// devicekeys.DefaultPIN. A software device never calls it.
	DevicePassphrase vbcustody.PINFunc
	// DeviceUnlockTTL is how long a custody device's first unwrap keeps its
	// sealed file unlocked; zero is devicekeys.DefaultUnlockTTL.
	DeviceUnlockTTL time.Duration
	// YubiKeySocket is the gpg-agent Assuan socket the YubiKey backend dials. ""
	// discovers it via gpgconf.
	YubiKeySocket string
	// YubiKeyPIN supplies the card User PIN the YubiKey backend presents before an
	// on-card decipher. Required when Backend is YubiKey; never persisted here.
	YubiKeyPIN yubikey.PINFunc

	// TPMSealedKeyFile is the path to the sealed-key blob the TPM backend unseals
	// (provisioned by tpm.Seal). Required when Backend is TPM.
	TPMSealedKeyFile string
	// TPMDevice is the TPM resource-manager device the backend opens. "" uses
	// /dev/tpmrm0.
	TPMDevice string
	// TPMPIN supplies the policy PIN (the sealed object's authValue). Required when
	// Backend is TPM; never persisted here.
	TPMPIN tpm.PINFunc

	// CruciformPairFile is the path to the offload pairing config (written by
	// `heyarr device pair-offload`). Required when Backend is Cruciform.
	CruciformPairFile string
	// CruciformWake wakes the paired phone for an offload unwrap; nil skips the
	// wake, for the LAN-direct path or a phone already reachable. The concrete
	// wake (the RP wake endpoint the desktop calls) is wired by the caller; until
	// it is, offload opens only when the phone is already reachable.
	CruciformWake cruciform.WakeFunc
}

// Select builds the configured custody backend, or reports why it cannot.
func Select(opts Options) (client.Custody, error) {
	switch opts.Backend {
	case "", Software:
		h, err := devicekeys.Holder(devicekeys.Options{
			Dir:       opts.DeviceDir,
			PIN:       opts.DevicePassphrase,
			UnlockTTL: opts.DeviceUnlockTTL,
		})
		if err != nil {
			return nil, fmt.Errorf("custody: this device's encryption key: %w", err)
		}
		return h, nil
	case YubiKey:
		if opts.YubiKeyPIN == nil {
			return nil, fmt.Errorf("custody: the yubikey backend needs a PIN source")
		}
		return yubikey.New(opts.YubiKeySocket, opts.YubiKeyPIN)
	case TPM:
		if opts.TPMPIN == nil {
			return nil, fmt.Errorf("custody: the tpm backend needs a PIN source")
		}
		if opts.TPMSealedKeyFile == "" {
			return nil, fmt.Errorf("custody: the tpm backend needs a sealed-key file (`heyarr device seal-tpm` is disabled until void-which-binds-go's custody/tpm lands, ADR-0021)")
		}
		blob, err := tpm.ReadBlobFile(opts.TPMSealedKeyFile)
		if err != nil {
			return nil, err
		}
		return tpm.New(blob, opts.TPMPIN, tpm.OpenDevice(opts.TPMDevice))
	case Cruciform:
		if opts.CruciformPairFile == "" {
			return nil, fmt.Errorf("custody: the cruciform backend needs a pairing config (run `heyarr device pair-offload` first)")
		}
		cfg, err := cruciform.LoadPairConfig(opts.CruciformPairFile)
		if err != nil {
			return nil, fmt.Errorf("custody: loading the offload pairing config (run `heyarr device pair-offload` first): %w", err)
		}
		transport := &cruciform.RelayTransport{RelayBase: cfg.RelayBase, Wake: opts.CruciformWake}
		return cruciform.NewCustody(cfg, transport)
	default:
		return nil, fmt.Errorf("custody: unknown backend %q", opts.Backend)
	}
}
