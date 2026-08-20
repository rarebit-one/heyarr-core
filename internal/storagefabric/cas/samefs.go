package cas

import "fmt"

// SameFilesystem reports whether two paths are on the same filesystem, and
// whether that could be determined at all.
//
// It exists because of the gap between what ADR-0014 promises and what a
// deployment usually does. The ladder degrades reflink → hardlink → copy, and
// both of the cheap rungs require the source and the destination to be on the
// SAME filesystem — reflink because block cloning is a filesystem operation,
// hardlink because a hardlink is a second name for one inode and inodes do not
// span devices.
//
// So a CAS at /var/lib/heyarr/cas and a library at /srv/media are not "mostly
// fine, slightly slower". Every single ingest is a full byte copy, and adopting
// a library doubles its storage — which is the exact outcome ADR-0014 exists to
// avoid, arrived at silently, one file at a time.
//
// The second return value is whether the answer is known. It is false on
// Windows, and on any filesystem that does not expose a device number; the
// caller must not treat "unknown" as "different", because warning about a
// problem that may not exist is how a warning stops being read.
func SameFilesystem(a, b string) (same, known bool, err error) {
	devA, okA, err := deviceOf(a)
	if err != nil {
		return false, false, fmt.Errorf("cas: examining %s: %w", a, err)
	}
	devB, okB, err := deviceOf(b)
	if err != nil {
		return false, false, fmt.Errorf("cas: examining %s: %w", b, err)
	}
	if !okA || !okB {
		return false, false, nil
	}
	return devA == devB, true, nil
}
