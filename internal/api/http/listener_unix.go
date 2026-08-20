//go:build unix

package httpapi

import (
	"net"
	"sync"

	"golang.org/x/sys/unix"
)

// umaskMutex serialises the umask dance below. umask is per-process, so two
// servers starting concurrently — which happens in tests, and in `heyarr all`
// if the peer ever gains a listener — would otherwise restore each other's
// value.
var umaskMutex sync.Mutex

// listenUnixRestricted binds a unix socket that only its owner can talk to.
//
// The permission is applied by lowering the umask across the bind rather than
// by chmod-ing afterwards, because between bind and chmod the socket is
// world-writable and anything on the machine can connect. The window is small,
// but this socket is an unauthenticated path into the whole API when
// http.auth.enabled is false, and "small window" is not a security property.
func listenUnixRestricted(path string) (net.Listener, error) {
	umaskMutex.Lock()
	old := unix.Umask(0o177) // clears every bit except owner read/write
	l, err := net.Listen("unix", path)
	unix.Umask(old)
	umaskMutex.Unlock()
	return l, err
}
