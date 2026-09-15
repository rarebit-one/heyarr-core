package discovery

import (
	"net"
	"reflect"
	"testing"
)

func mustNets(t *testing.T, cidrs ...string) []*net.IPNet {
	t.Helper()
	var out []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatalf("parsing %q: %v", c, err)
		}
		out = append(out, n)
	}
	return out
}

const (
	up        = net.FlagUp | net.FlagMulticast
	loopback  = net.FlagUp | net.FlagMulticast | net.FlagLoopback
	noMcast   = net.FlagUp
	downMcast = net.FlagMulticast
)

func TestSelectTrusted(t *testing.T) {
	// A trusted LAN and a documentation v6 net stand in for the estate ranges.
	trusted := mustNets(t, "192.0.2.0/24", "2001:db8::/32")

	tests := []struct {
		name      string
		ifaces    []iface
		nets      []*net.IPNet
		wantNames []string
		// wantAddrs maps a selected interface name to the addresses it should
		// advertise (narrowed to the trusted ones).
		wantAddrs map[string][]string
	}{
		{
			name: "a trusted, multicast, up interface is selected",
			ifaces: []iface{
				{Name: "lan0", Index: 2, Flags: up, Addrs: []net.IP{net.ParseIP("192.0.2.10")}},
			},
			nets:      trusted,
			wantNames: []string{"lan0"},
			wantAddrs: map[string][]string{"lan0": {"192.0.2.10"}},
		},
		{
			name: "a loopback interface is dropped even though 127/8 is a conventional trusted CIDR",
			ifaces: []iface{
				{Name: "lo", Index: 1, Flags: loopback, Addrs: []net.IP{net.ParseIP("127.0.0.1")}},
			},
			nets:      mustNets(t, "127.0.0.0/8", "192.0.2.0/24"),
			wantNames: nil,
		},
		{
			name: "a non-multicast interface is dropped",
			ifaces: []iface{
				{Name: "ptp0", Index: 3, Flags: noMcast, Addrs: []net.IP{net.ParseIP("192.0.2.11")}},
			},
			nets:      trusted,
			wantNames: nil,
		},
		{
			name: "a down interface is dropped",
			ifaces: []iface{
				{Name: "lan0", Index: 2, Flags: downMcast, Addrs: []net.IP{net.ParseIP("192.0.2.10")}},
			},
			nets:      trusted,
			wantNames: nil,
		},
		{
			name: "an interface off the trust boundary is dropped",
			ifaces: []iface{
				{Name: "wan0", Index: 4, Flags: up, Addrs: []net.IP{net.ParseIP("198.51.100.7")}}, // TEST-NET-2, untrusted
			},
			nets:      trusted,
			wantNames: nil,
		},
		{
			name: "a mixed interface advertises only its trusted addresses",
			ifaces: []iface{
				{Name: "lan0", Index: 2, Flags: up, Addrs: []net.IP{
					net.ParseIP("192.0.2.10"),   // trusted LAN
					net.ParseIP("198.51.100.7"), // untrusted, must not be published
					net.ParseIP("2001:db8::5"),  // trusted v6
				}},
			},
			nets:      trusted,
			wantNames: []string{"lan0"},
			wantAddrs: map[string][]string{"lan0": {"192.0.2.10", "2001:db8::5"}},
		},
		{
			name: "an empty trust boundary selects nothing (tier off)",
			ifaces: []iface{
				{Name: "lan0", Index: 2, Flags: up, Addrs: []net.IP{net.ParseIP("192.0.2.10")}},
			},
			nets:      nil,
			wantNames: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := selectTrusted(tt.ifaces, tt.nets)
			gotNames := ifaceNames(got)
			if len(gotNames) != len(tt.wantNames) || (len(gotNames) > 0 && !reflect.DeepEqual(gotNames, tt.wantNames)) {
				t.Fatalf("selected = %v, want %v", gotNames, tt.wantNames)
			}
			for _, i := range got {
				var addrs []string
				for _, ip := range i.Addrs {
					addrs = append(addrs, ip.String())
				}
				if want := tt.wantAddrs[i.Name]; !reflect.DeepEqual(addrs, want) {
					t.Errorf("%s advertised addrs = %v, want %v", i.Name, addrs, want)
				}
			}
		})
	}
}
