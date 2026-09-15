package discovery

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// captureCast is a multicaster that records every announcement instead of
// touching a socket, and signals the first one so a test can synchronise without
// polling or sleeping.
type captureCast struct {
	mu     sync.Mutex
	sends  []capturedSend
	closed bool

	got  chan struct{}
	once sync.Once
}

type capturedSend struct {
	iface iface
	msg   []byte
}

func newCaptureCast() *captureCast { return &captureCast{got: make(chan struct{})} }

func (c *captureCast) announce(i iface, msg []byte) error {
	c.mu.Lock()
	c.sends = append(c.sends, capturedSend{iface: i, msg: append([]byte(nil), msg...)})
	c.mu.Unlock()
	c.once.Do(func() { close(c.got) })
	return nil
}

func (c *captureCast) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func lanInterface() iface {
	return iface{Name: "lan0", Index: 2, Flags: up, Addrs: []net.IP{net.ParseIP("192.0.2.10")}}
}

func loopInterface() iface {
	return iface{Name: "lo", Index: 1, Flags: loopback, Addrs: []net.IP{net.ParseIP("127.0.0.1")}}
}

func TestAdvertiserAnnouncesOnTrustedInterfaceAndStopsCleanly(t *testing.T) {
	cast := newCaptureCast()
	adv := New(Options{
		Params:      Params{Port: 7777, TLS: true, APIPath: "/api/v1"},
		TrustedNets: mustNets(t, "192.0.2.0/24"),
		Interval:    time.Hour, // only the immediate announcement matters here
	})
	// A trusted LAN interface plus a loopback the selector must ignore.
	adv.lister = func() ([]iface, error) { return []iface{lanInterface(), loopInterface()}, nil }
	adv.newCast = func() (multicaster, error) { return cast, nil }

	started, err := adv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !started {
		t.Fatal("expected the advertiser to start on a trusted interface")
	}

	select {
	case <-cast.got:
	case <-time.After(2 * time.Second):
		t.Fatal("no announcement was sent within the guard window")
	}

	adv.Stop()

	cast.mu.Lock()
	defer cast.mu.Unlock()
	if !cast.closed {
		t.Error("Stop did not close the multicaster")
	}
	if len(cast.sends) == 0 {
		t.Fatal("no announcement was recorded")
	}
	first := cast.sends[0]
	if first.iface.Name != "lan0" {
		t.Errorf("announced on %q, want the trusted lan0 (loopback must be skipped)", first.iface.Name)
	}

	var msg dnsmessage.Message
	if err := msg.Unpack(first.msg); err != nil {
		t.Fatalf("the announcement was not a valid DNS message: %v", err)
	}
	var port uint16
	for _, ans := range msg.Answers {
		if srv, ok := ans.Body.(*dnsmessage.SRVResource); ok {
			port = srv.Port
		}
	}
	if port != 7777 {
		t.Errorf("advertised SRV port = %d, want 7777", port)
	}
}

func TestAdvertiserInertWithoutTrustedInterface(t *testing.T) {
	tests := []struct {
		name   string
		nets   []*net.IPNet
		ifaces []iface
	}{
		{
			name:   "empty trust boundary",
			nets:   nil,
			ifaces: []iface{lanInterface()},
		},
		{
			name:   "no interface inside the boundary",
			nets:   mustNets(t, "192.0.2.0/24"),
			ifaces: []iface{loopInterface()},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opened := false
			adv := New(Options{Params: Params{Port: 7777}, TrustedNets: tt.nets})
			adv.lister = func() ([]iface, error) { return tt.ifaces, nil }
			adv.newCast = func() (multicaster, error) {
				opened = true
				return newCaptureCast(), nil
			}

			started, err := adv.Start(context.Background())
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if started {
				t.Error("expected the advertiser to be inert")
			}
			if opened {
				t.Error("a multicast socket was opened when nothing should be advertised")
			}
			adv.Stop() // must be a clean no-op
		})
	}
}

// TestAdvertiserStopIsSafeWithoutStart guards the controller's unconditional
// Stop() on shutdown: an advertiser that never started must treat Stop as a
// no-op rather than panic.
func TestAdvertiserStopIsSafeWithoutStart(t *testing.T) {
	adv := New(Options{Params: Params{Port: 7777}})
	adv.Stop()
	adv.Stop()
}
