//go:build unix

package httpapi

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestListenUnixRestrictedLeavesConcurrentDirectoriesUsable is the regression
// test for #151.
//
// Binding the local socket used to lower the process umask to 0o177 for the
// duration of the bind. umask is per-process, and `heyarr all` runs the API and
// the worker in one process, so any directory another goroutine created inside
// that window lost its search bit — 0o750 &^ 0o177 is 0o600 — and became a
// directory nothing could ever be written into again. That is what took out
// twelve of twelve ingests on one CI run: the content-addressed store's shard
// directory was created during start-up, which is when the socket is bound.
//
// The test creates directories the way the store does while binds run
// alongside, and asserts each one can still be written into. Against the
// umask implementation it fails in the first round or two; the counts are in
// the pull request.
func TestListenUnixRestrictedLeavesConcurrentDirectoriesUsable(t *testing.T) {
	t.Parallel()

	sockDir := shortTempDir(t)
	tree := t.TempDir()

	stop := make(chan struct{})
	var binders sync.WaitGroup
	binders.Add(1)
	go func() {
		defer binders.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			path := filepath.Join(sockDir, fmt.Sprintf("s%d", i))
			l, err := listenUnixRestricted(path)
			if err != nil {
				return
			}
			_ = l.Close()
		}
	}()
	defer func() {
		close(stop)
		binders.Wait()
	}()

	const rounds, workers = 200, 8
	var mu sync.Mutex
	var poisoned []string
	for round := 0; round < rounds; round++ {
		shardRoot := filepath.Join(tree, fmt.Sprint(round), "blobs", "blake3")
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				// Exactly what the store does: MkdirAll the two-level shard,
				// then write into it.
				shard := filepath.Join(shardRoot, fmt.Sprintf("%02x", w), "c1")
				if err := os.MkdirAll(shard, 0o750); err != nil {
					mu.Lock()
					poisoned = append(poisoned, fmt.Sprintf("mkdir: %v (%s)", err, describeChain(tree, shard)))
					mu.Unlock()
					return
				}
				if err := os.WriteFile(filepath.Join(shard, "blob"), []byte("x"), 0o600); err != nil {
					mu.Lock()
					poisoned = append(poisoned, fmt.Sprintf("write: %v (%s)", err, describeChain(tree, shard)))
					mu.Unlock()
				}
			}(w)
		}
		wg.Wait()
		mu.Lock()
		done := len(poisoned) > 0
		mu.Unlock()
		if done {
			break
		}
	}

	if len(poisoned) > 0 {
		t.Fatalf("binding the socket made %d concurrently created directories unusable; first: %s",
			len(poisoned), poisoned[0])
	}
}

// TestListenUnixRestrictedIsOwnerOnly holds the property the umask was there
// for in the first place: the socket is never reachable by anyone but its
// owner, including while it is being prepared.
func TestListenUnixRestrictedIsOwnerOnly(t *testing.T) {
	t.Parallel()

	path := filepath.Join(shortTempDir(t), "heyarr.sock")
	l, err := listenUnixRestricted(path)
	if err != nil {
		t.Fatalf("binding: %v", err)
	}
	defer func() { _ = l.Close() }()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the socket is not at the path it was published to: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("socket mode %v, want %v", got, os.FileMode(0o600))
	}

	// And it is a working listener at the published path, not just a file.
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("ok"))
		_ = conn.Close()
	}()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dialling the published socket: %v", err)
	}
	defer func() { _ = conn.Close() }()
	buf := make([]byte, 2)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("reading from the published socket: %v", err)
	}
	if string(buf) != "ok" {
		t.Errorf("read %q, want %q", buf, "ok")
	}
}

// TestListenUnixRestrictedCleansUp checks that neither the staging directory
// nor the published socket outlives the listener.
func TestListenUnixRestrictedCleansUp(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	path := filepath.Join(dir, "heyarr.sock")
	l, err := listenUnixRestricted(path)
	if err != nil {
		t.Fatalf("binding: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the socket outlived the listener: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the socket directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the bind left %d entries behind: %v", len(entries), entries)
	}
	// A second Close is what a shutdown path that closes listeners twice does,
	// and it must not report the missing socket as a failure of its own.
	_ = l.Close()
}

// describeChain reports the mode of every directory between root and path, so
// a failure says which level went wrong rather than only that one did.
func describeChain(root, path string) string {
	out := ""
	for p := path; len(p) >= len(root); p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		switch {
		case err != nil:
			out = fmt.Sprintf("%s=absent ", filepath.Base(p)) + out
		default:
			out = fmt.Sprintf("%s=%v ", filepath.Base(p), info.Mode().Perm()) + out
		}
		if p == root {
			break
		}
	}
	return out
}

// shortTempDir is t.TempDir with the platform's socket path limit in mind: a
// test name is part of the directory the toolchain hands out, and the sum can
// exceed 104 bytes on darwin before a socket name has been added.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hy")
	if err != nil {
		t.Fatalf("creating a temporary directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestListenUnixRestrictedFitsThePathBudget: the path that gets bound is the
// staged one, and the platform limit applies to it. Staging inside a directory
// costs bytes, and the first version of this fix spent more of them than the
// socket's own name, which stopped the two-peer acceptance scenario binding at
// all — its paths sit within ten bytes of darwin's 104.
func TestListenUnixRestrictedFitsThePathBudget(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	// The name every deployment uses. Staging must fit in the space it already
	// occupies, or a path that bound yesterday stops binding today.
	published := filepath.Join(dir, "heyarr.sock")
	staged := len(dir) + 1 + stagingNameLen + len("/s")
	if staged > len(published) {
		t.Errorf("staging needs %d bytes where the published path needs %d; a socket that fit no longer does",
			staged, len(published))
	}

	l, err := listenUnixRestricted(published)
	if err != nil {
		t.Fatalf("binding: %v", err)
	}
	_ = l.Close()
}
