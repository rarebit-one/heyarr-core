package discovery

import (
	"fmt"
	"net"
)

// iface is one host interface reduced to what advertisement needs: an identity
// to aim the multicast send at, and the addresses it carries.
type iface struct {
	Name  string
	Index int
	Flags net.Flags
	Addrs []net.IP
}

// interfaceLister enumerates the host interfaces. It is injectable so interface
// gating is tested against a fixed, documentation-range set rather than the
// machine's real NICs — there is no way to assert "only trusted interfaces are
// chosen" against hardware a test does not control.
type interfaceLister func() ([]iface, error)

// systemInterfaces reads the real host interfaces and their addresses.
func systemInterfaces() ([]iface, error) {
	raw, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("discovery: listing interfaces: %w", err)
	}
	out := make([]iface, 0, len(raw))
	for _, ni := range raw {
		addrs, err := ni.Addrs()
		if err != nil {
			// An interface whose addresses cannot be read is one that cannot be
			// advertised on; drop it rather than fail the whole sweep.
			continue
		}
		var ips []net.IP
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				ips = append(ips, ipn.IP)
			}
		}
		out = append(out, iface{Name: ni.Name, Index: ni.Index, Flags: ni.Flags, Addrs: ips})
	}
	return out, nil
}

// selectTrusted keeps the interfaces heyarr may announce on and narrows each to
// the addresses it may announce.
//
// An interface qualifies when it is up, multicast-capable, not loopback, and
// carries at least one address inside the trusted CIDRs. Its returned Addrs is
// exactly those trusted addresses — the ones a trusted client reaches the node
// on, and the ones the A/AAAA records carry. An untrusted address on an
// otherwise-trusted interface (a second NIC bridging to the raw internet) is
// dropped, so the node never publishes an address off the trust boundary.
//
// An empty nets selects nothing: the guest trust boundary unset means the tier
// is off, and discovery borrows exactly that boundary. A loopback interface is
// dropped even though 127.0.0.0/8 is a conventional trusted CIDR — link-local
// multicast never leaves the host, so announcing there reaches no client.
func selectTrusted(ifaces []iface, nets []*net.IPNet) []iface {
	var out []iface
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagMulticast == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		var trusted []net.IP
		for _, ip := range i.Addrs {
			if containedIn(ip, nets) {
				trusted = append(trusted, ip)
			}
		}
		if len(trusted) == 0 {
			continue
		}
		sel := i
		sel.Addrs = trusted
		out = append(out, sel)
	}
	return out
}

func containedIn(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func ifaceNames(ifaces []iface) []string {
	names := make([]string, 0, len(ifaces))
	for _, i := range ifaces {
		names = append(names, i.Name)
	}
	return names
}
