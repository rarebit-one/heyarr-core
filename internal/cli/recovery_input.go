package cli

import (
	"crypto/ed25519"
	"fmt"
	"strings"

	"github.com/rarebit-one/voidbind-go/encryption"
	"github.com/rarebit-one/voidbind-go/identity"
	"github.com/rarebit-one/voidbind-go/recovery"
)

// minShareWords is the length of the shortest SLIP-39 share (a 128-bit secret);
// a recovery-secret share (256-bit) is 33 words. A line of at least this many
// words is read as a share, never as a mistyped secret.
const minShareWords = 20

// parseRecoveryInput turns what the operator gave into the recovery secret: the
// bech32m secret itself (spaces and case as written are fine), or SLIP-39
// recovery shares, one per line, that combine to it (voidbind-go ADR-0011). A
// bad checksum in either is refused, never carried into a different identity.
func parseRecoveryInput(raw string) (recovery.Secret, error) {
	text := strings.TrimSpace(raw)
	secret, err := recovery.ParseSecret(text)
	if err == nil {
		return secret, nil
	}
	firstLine, _, _ := strings.Cut(text, "\n")
	if len(strings.Fields(firstLine)) < minShareWords {
		return recovery.Secret{}, err
	}
	var shares []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			shares = append(shares, line)
		}
	}
	secret, err = recovery.CombineShares(shares, nil)
	if err != nil {
		return recovery.Secret{}, fmt.Errorf("the recovery shares did not combine: %w", err)
	}
	return secret, nil
}

// recoveryIdentity is what a recovery secret derives, for display and checks.
type recoveryIdentity struct {
	UserID      string `json:"user_id"`
	Fingerprint string `json:"fingerprint"`
	RecoveryKey string `json:"recovery_key"`
}

func deriveRecoveryIdentity(secret recovery.Secret) (recoveryIdentity, error) {
	seed := recovery.DeriveUserSeed(secret)
	defer clear(seed)
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	encSeed := recovery.DeriveUserEncryptionSeed(secret)
	defer clear(encSeed)
	enc, err := encryption.NewPrivateKey(encSeed)
	if err != nil {
		return recoveryIdentity{}, err
	}
	return recoveryIdentity{
		UserID:      identity.FormatPublicKey(pub),
		Fingerprint: recovery.Fingerprint(pub),
		RecoveryKey: encryption.FormatPublicKey(enc.PublicKey().Bytes()),
	}, nil
}
