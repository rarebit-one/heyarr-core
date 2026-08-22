package identity

import (
	"crypto/ed25519"
	"fmt"
)

// Signer loads the private half of this node's identity.
//
// It is deliberately not a field on Identity and not returned by Ensure.
// Identity is handed to everything that needs to know *who this node is* — the
// startup log, the API, the CLI — and a private key on that struct is a
// private key that eventually gets logged, marshalled or embedded in a
// diagnostic. Ensure's contract is "the three copies agree"; this is a second,
// explicit act by the one caller that has to sign something.
//
// Today that caller is the peer transport (ADR-0012, M4-05), which needs the
// key to sign the self-signed certificate it presents. The certificate is
// regenerated in-process and never written down, so this is the only path from
// the 0600 file to anything on the wire.
//
// The permission and prefix checks Ensure applies are applied here too, from
// the same reader: a key that became group-readable between startup and now is
// exactly as exposed as one that was written that way, and a signer that
// loaded it anyway would be trusting a check that ran minutes ago.
func Signer(dataDir string) (ed25519.PrivateKey, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("identity: a data directory is required to load the private key")
	}
	seed, err := readKeyFile(KeyPath(dataDir))
	if err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
