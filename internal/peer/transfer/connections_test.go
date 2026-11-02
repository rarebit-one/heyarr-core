package transfer_test

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/hashing"
)

// heyarr-core#846: every pull built a fresh pinned client, and the keep-alive
// connection it left in that client's idle pool was never closed. On the reference host
// that was one ESTABLISHED socket per pulled blob, until the 28,232-port
// ephemeral range was full and every pull failed with EADDRNOTAVAIL. A pull
// must leave no connection behind.

// countingProxy forwards TCP to target and counts the connections still open.
// TLS passes through it untouched, so the pin is still checked end to end.
type countingProxy struct {
	ln   net.Listener
	open atomic.Int64
	wg   sync.WaitGroup
}

func startCountingProxy(t *testing.T, target string) *countingProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &countingProxy{ln: ln}
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", target)
			if err != nil {
				_ = in.Close()
				continue
			}
			p.open.Add(1)
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				defer p.open.Add(-1)
				done := make(chan struct{}, 2)
				pipe := func(dst, src net.Conn) {
					_, _ = io.Copy(dst, src)
					done <- struct{}{}
				}
				go pipe(out, in)
				go pipe(in, out)
				<-done // either side closing ends the connection
				_ = in.Close()
				_ = out.Close()
				<-done
			}()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return p
}

func (p *countingProxy) addr() string { return p.ln.Addr().String() }

// settlesAt waits briefly for the open count to reach want, and reports the
// count it last saw.
func (p *countingProxy) settlesAt(want int64) (int64, bool) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := p.open.Load()
		if got == want || time.Now().After(deadline) {
			return got, got == want
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPullsLeaveNoConnectionOpen(t *testing.T) {
	holder := newNode(t, "peer-holder", "holder")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(holder.member(), dst.member())

	const blobs = 8
	src := startSource(t, holder, root, nil)
	var hashes []hashing.Hash
	for i := range blobs {
		content := bytes.Repeat([]byte(fmt.Sprintf("blob %d of a backlog ", i)), 200)
		desc, err := src.store.Put(t.Context(), bytes.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, desc.Hash)
	}

	proxy := startCountingProxy(t, src.addr)
	via := sourceFor(holder, proxy.addr())
	dest := newDestination(t, dst)

	for _, h := range hashes {
		if _, err := dest.puller.Pull(t.Context(), via, h); err != nil {
			t.Fatalf("pulling %s: %v", h, err)
		}
	}
	// A manifest read is a second client path through the same constructor.
	_, _ = dest.puller.FetchManifest(t.Context(), via, hashes[0])

	if got, ok := proxy.settlesAt(0); !ok {
		t.Errorf("%d connections still open after %d pulls and a manifest read, want 0: "+
			"each pull's client leaves its connection behind (heyarr-core#846)", got, blobs)
	}
}
