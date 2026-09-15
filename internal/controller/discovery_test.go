package controller

import "testing"

func TestAdvertisePort(t *testing.T) {
	tests := []struct {
		name      string
		boundAddr string
		wantPort  uint16
		wantOK    bool
	}{
		{name: "a concrete LAN bind is advertisable", boundAddr: "192.0.2.10:7777", wantPort: 7777, wantOK: true},
		{name: "an IPv4 wildcard bind is advertisable", boundAddr: "0.0.0.0:7777", wantPort: 7777, wantOK: true},
		{name: "an IPv6 wildcard bind is advertisable", boundAddr: "[::]:8443", wantPort: 8443, wantOK: true},
		{name: "a loopback bind names no reachable port", boundAddr: "127.0.0.1:7777", wantOK: false},
		{name: "an IPv6 loopback bind names no reachable port", boundAddr: "[::1]:7777", wantOK: false},
		{name: "a socket-only node has no TCP addr", boundAddr: "", wantOK: false},
		{name: "a malformed addr is refused", boundAddr: "not-an-address", wantOK: false},
		{name: "port zero is refused", boundAddr: "192.0.2.10:0", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPort, gotOK := advertisePort(tt.boundAddr)
			if gotOK != tt.wantOK {
				t.Fatalf("advertisePort(%q) ok = %v, want %v", tt.boundAddr, gotOK, tt.wantOK)
			}
			if gotOK && gotPort != tt.wantPort {
				t.Errorf("advertisePort(%q) port = %d, want %d", tt.boundAddr, gotPort, tt.wantPort)
			}
		})
	}
}
