//go:build !linux && !darwin

package cas

import (
	"context"
	"errors"

	"github.com/rarebit-one/void-which-binds-go/hashing"
)

func (s *FS) CheckPrivateBlob(context.Context, hashing.Hash) error {
	return errors.New("cas: private blob proof is unsupported on this platform")
}
