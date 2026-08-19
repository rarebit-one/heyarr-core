//go:build !linux && !darwin

package cas

import "fmt"

// reflink is unavailable on this platform, so Link degrades to a hardlink and
// then a copy (ADR-0014).
func reflink(_, _ string) error {
	return fmt.Errorf("%w: copy-on-write cloning is not supported on this platform", errDegrade)
}
