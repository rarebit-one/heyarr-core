package cli

import (
	"crypto/ecdh"
)

// loadDeviceEncKey opens this machine's device store and loads its software
// X25519 encryption private key, for tests that open a wrap themselves. No
// production path holds the raw key: they unwrap through custody (a custody
// device refuses this with device.ErrKeyInCustody).
func loadDeviceEncKey(deviceDir string) (*ecdh.PrivateKey, error) {
	ds, err := openDeviceStore(deviceDir)
	if err != nil {
		return nil, err
	}
	return ds.LoadEncryptionKey()
}
