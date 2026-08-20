//go:build windows

package httpapi

import (
	"net"
	"os"
)

// listenUnixRestricted binds an AF_UNIX socket. Windows has supported them
// since Windows 10 1803, so the listener works — but it has no umask and the
// filesystem permissions that would matter are ACLs, which os.Chmod cannot
// express. The socket therefore inherits the ACL of its directory, which is
// what the data directory's own permissions already govern.
func listenUnixRestricted(path string) (net.Listener, error) {
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// Best effort, and genuinely best effort on this platform: Chmod maps only
	// to the read-only attribute here. It is kept so the intent survives a
	// future change rather than because it enforces anything.
	_ = os.Chmod(path, 0o600)
	return l, nil
}
