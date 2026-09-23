//go:build !unix

package cas

import "testing"

// sourceLinkCount has no answer where syscall.Stat_t does not exist (Windows).
//
// Reporting "unknown" rather than skipping keeps the rest of the test running
// there: the probe's cleanup of the store is checked on every platform, and
// only the link-count assertion — which is what actually needs stat(2) — is
// dropped.
func sourceLinkCount(*testing.T, string) (uint64, bool) { return 0, false }
