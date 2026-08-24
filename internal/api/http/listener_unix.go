//go:build unix

package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
)

// stagingNameLen is the length of a staging directory's name, and it is a
// budget rather than a taste.
//
// A unix socket path has a hard platform limit — 104 bytes on darwin — and the
// path that has to fit is the one that gets BOUND, which is the staged one. A
// staging directory of 9 characters holding a socket called "s" costs
// len(".xxxxxxxx") + len("/s") = 11 bytes in place of the published basename,
// so any socket named "heyarr.sock" or longer stages in the same space or less
// and no configuration that used to bind stops binding. This is not
// theoretical: the first version of this used a descriptive name and pushed the
// acceptance run's two-peer paths from 94 bytes to 105.
const stagingNameLen = 9

// listenUnixRestricted binds a unix socket that only its owner can talk to.
//
// # Why this does not lower the umask
//
// The obvious implementation binds and then chmods, which leaves the socket
// world-writable between the two — and this socket is an unauthenticated path
// into the whole API when http.auth.enabled is false, so "small window" is not
// a security property. The obvious fix for THAT is to lower the umask across
// the bind, which is what this function used to do, and it was a worse bug:
// umask is per-PROCESS state, not per-goroutine. `heyarr all` runs the API and
// the worker in one process, so a umask of 0o177 held for the duration of a
// bind silently stripped the search bit from any directory another goroutine
// created at that instant — 0o750 &^ 0o177 is 0o600. The content-addressed
// store creates its shard directories on first use, which is during start-up,
// which is exactly when the socket is bound. A directory created 0o600 is one
// nothing can ever be written into again, so every later ingest into it failed
// with `mkdir …: permission denied` until the run died (#151), and a store
// poisoned that way stays poisoned across restarts.
//
// So: bind inside a private directory instead. The staging directory is created
// 0o700, so nothing else on the machine can reach the socket even while it is
// briefly permissive; the socket is chmod-ed and only then renamed onto its
// published path, which is atomic. No process-global state is touched, and the
// window the umask existed to close is closed by a directory rather than by
// timing.
func listenUnixRestricted(path string) (net.Listener, error) {
	// The staged path is the one that gets bound, so it is the one that has to
	// fit. Reported as the error a too-long published path gets, because the
	// caller's answer is the same: keep the TCP listener and tell the operator
	// to shorten http.unix_socket.
	if budget := len(filepath.Dir(path)) + 1 + stagingNameLen + 2; budget >= MaxUnixSocketPath() {
		return nil, fmt.Errorf("%w: %s needs %d bytes to bind and the limit on this platform is %d — "+
			"set http.unix_socket to a shorter path",
			errSocketPathTooLong, path, budget, MaxUnixSocketPath())
	}
	dir, err := makeStagingDir(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("httpapi: creating a private directory to bind %s in: %w", path, err)
	}
	staged := filepath.Join(dir, "s")
	// The directory is empty once the socket has been renamed out of it, so
	// removing it is tidy-up rather than correctness.
	defer func() { _ = os.Remove(dir) }()

	l, err := net.Listen("unix", staged)
	if err != nil {
		return nil, err
	}
	ul, ok := l.(*net.UnixListener)
	if !ok {
		_ = l.Close()
		return nil, fmt.Errorf("httpapi: binding %s produced a %T rather than a unix listener", path, l)
	}
	// The listener would otherwise unlink the staging name on Close, which by
	// then names nothing. Unlinking the published path is this listener's job.
	ul.SetUnlinkOnClose(false)

	if err := os.Chmod(staged, 0o600); err != nil {
		_ = l.Close()
		_ = os.Remove(staged)
		return nil, fmt.Errorf("httpapi: restricting %s: %w", path, err)
	}
	if err := os.Rename(staged, path); err != nil {
		_ = l.Close()
		_ = os.Remove(staged)
		return nil, fmt.Errorf("httpapi: publishing the socket at %s: %w", path, err)
	}
	return &publishedUnixListener{UnixListener: ul, path: path}, nil
}

// publishedUnixListener unlinks the path the socket was renamed onto, which is
// what SetUnlinkOnClose would have done had the socket never moved.
type publishedUnixListener struct {
	*net.UnixListener
	path   string
	closed atomic.Bool
}

// makeStagingDir creates a private directory to bind in.
//
// The name is random rather than descriptive because it has to fit in
// stagingNameLen bytes, and random is the only thing that stays unique between
// two processes sharing a data directory at that length. A crash between the
// mkdir and the rename leaves an empty dot-directory behind; that is the price
// of the path budget, and it is empty.
func makeStagingDir(parent string) (string, error) {
	var last error
	for attempt := 0; attempt < 8; attempt++ {
		var raw [(stagingNameLen - 1) / 2]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", err
		}
		dir := filepath.Join(parent, "."+hex.EncodeToString(raw[:]))
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		last = err
	}
	return "", last
}

func (l *publishedUnixListener) Close() error {
	err := l.UnixListener.Close()
	if l.closed.CompareAndSwap(false, true) {
		if rmErr := os.Remove(l.path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) && err == nil {
			err = fmt.Errorf("httpapi: removing %s: %w", l.path, rmErr)
		}
	}
	return err
}
