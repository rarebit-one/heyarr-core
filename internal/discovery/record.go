package discovery

import (
	"fmt"
	"net"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	// ServiceType is the DNS-SD service the node advertises (ADR-0094): the
	// client-facing name that answers "is there a heyarr on this network?".
	ServiceType = "_heyarr._tcp"

	// Domain is the mDNS link-local domain. mDNS names always live under
	// ".local." (RFC 6762 §3); nothing here resolves in the global DNS.
	Domain = "local."

	// instanceLabel is the DNS-SD instance name. It is a FIXED, non-identifying
	// label rather than a friendly per-node name on purpose: a real name ("Living
	// Room", the peer's site) would both leak into a public repo's fixtures
	// (make hygiene) and put personal detail on the wire. Disambiguating an
	// active-active pair by a stable, non-personal token is a later refinement;
	// mechanism 1 (split-horizon DNS, nearest-server) already picks between a
	// pair, so mDNS naming them apart is not what makes discovery work.
	instanceLabel = "heyarr"

	// hostLabel is the SRV target — the .local name whose A/AAAA records carry
	// the interface address. It is fixed and non-identifying for the same reason
	// as instanceLabel; the address a client actually dials comes from the A/AAAA
	// records built per interface, not from the label.
	hostLabel = "heyarr.local."

	// defaultTTL is the record lifetime a client may cache the answer for, in
	// seconds. It doubles as the re-announce horizon: a responder that does not
	// answer live queries (this one) must refresh within it so a client that
	// joined late still learns the node exists.
	defaultTTL = 120
)

// Params is the interface-independent content of an advertisement: everything a
// client needs to build a base URL except the address, which is per-interface.
//
// It is deliberately minimal. The wire is a broadcast medium on a shared LAN, so
// it carries only what a client cannot derive: the port, whether that port
// speaks TLS, and the API base path. It names no host, no site, no person, and
// no library — those are either private or discoverable only after the client
// authenticates.
type Params struct {
	// Port is the TCP port the client API is reached on — the TLS port when TLS
	// is on, since that is the one listener a client dials (ADR-0072).
	Port uint16
	// TLS reports whether that port serves HTTPS. Advertising the TLS endpoint is
	// the default (ADR-0094 open question): a phone or browser wants the encrypted
	// origin. ADR-0079's plain-HTTP render listener is a separate concern — it
	// exists for televisions and is not what a discovering client should dial.
	TLS bool
	// APIPath is the base path the JSON API is mounted at (httpapi.APIPrefix). A
	// client joins scheme + host + port + this to reach the API.
	APIPath string
}

// TXT returns the DNS-SD TXT key/value strings, in the stable order a golden
// test can assert. `txtvers` is first by DNS-SD convention (RFC 6763 §6.4): a
// client reads it before trusting any other key, so the record format can change
// without silently misparsing on an old client.
func (p Params) TXT() []string {
	tls := "0"
	if p.TLS {
		tls = "1"
	}
	path := p.APIPath
	if path == "" {
		path = "/"
	}
	return []string{
		"txtvers=1",
		"path=" + path,
		"tls=" + tls,
	}
}

// ServiceName is the fully-qualified DNS-SD service name, e.g.
// "_heyarr._tcp.local.". A browsing client asks a PTR query for exactly this.
func ServiceName() string { return ServiceType + "." + Domain }

// InstanceName is the fully-qualified service instance name, e.g.
// "heyarr._heyarr._tcp.local.". It is the PTR target and the SRV/TXT owner.
func InstanceName() string { return instanceLabel + "." + ServiceName() }

// Message builds the unsolicited mDNS response announcing this instance on one
// interface, whose usable addresses are addrs. It is the pure heart of the
// package: a config plus a set of addresses in, a marshalled DNS packet out, no
// socket touched.
//
// The answer set is the DNS-SD quartet plus address records:
//
//   - PTR   _heyarr._tcp.local.        -> heyarr._heyarr._tcp.local.
//   - SRV   heyarr._heyarr._tcp.local. -> heyarr.local.:port
//   - TXT   heyarr._heyarr._tcp.local. -> {txtvers, path, tls}
//   - A/AAAA heyarr.local.             -> each address in addrs
//
// ttl of 0 uses defaultTTL. An empty addrs is an error: an SRV whose target
// resolves to nothing is an answer a client cannot act on, and building one
// would advertise a node no one can reach.
func (p Params) Message(addrs []net.IP, ttl uint32) ([]byte, error) {
	if p.Port == 0 {
		return nil, fmt.Errorf("discovery: refusing to advertise port 0")
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("discovery: refusing to advertise with no address to reach the node")
	}
	if ttl == 0 {
		ttl = defaultTTL
	}

	service, err := dnsmessage.NewName(ServiceName())
	if err != nil {
		return nil, fmt.Errorf("discovery: service name: %w", err)
	}
	instance, err := dnsmessage.NewName(InstanceName())
	if err != nil {
		return nil, fmt.Errorf("discovery: instance name: %w", err)
	}
	target, err := dnsmessage.NewName(hostLabel)
	if err != nil {
		return nil, fmt.Errorf("discovery: target name: %w", err)
	}

	// A response, authoritative for the records it carries — mDNS responders own
	// the .local names they answer for (RFC 6762 §6). The question section is
	// empty: this is an unsolicited announcement, not a reply to a query.
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{Response: true, Authoritative: true},
	}

	ptrHdr := dnsmessage.ResourceHeader{Name: service, Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET, TTL: ttl}
	msg.Answers = append(msg.Answers, dnsmessage.Resource{
		Header: ptrHdr,
		Body:   &dnsmessage.PTRResource{PTR: instance},
	})

	srvHdr := dnsmessage.ResourceHeader{Name: instance, Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET, TTL: ttl}
	msg.Answers = append(msg.Answers, dnsmessage.Resource{
		Header: srvHdr,
		Body:   &dnsmessage.SRVResource{Priority: 0, Weight: 0, Port: p.Port, Target: target},
	})

	txtHdr := dnsmessage.ResourceHeader{Name: instance, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET, TTL: ttl}
	msg.Answers = append(msg.Answers, dnsmessage.Resource{
		Header: txtHdr,
		Body:   &dnsmessage.TXTResource{TXT: p.TXT()},
	})

	for _, ip := range addrs {
		if v4 := ip.To4(); v4 != nil {
			var a [4]byte
			copy(a[:], v4)
			aHdr := dnsmessage.ResourceHeader{Name: target, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: ttl}
			msg.Answers = append(msg.Answers, dnsmessage.Resource{Header: aHdr, Body: &dnsmessage.AResource{A: a}})
			continue
		}
		if v6 := ip.To16(); v6 != nil {
			var aaaa [16]byte
			copy(aaaa[:], v6)
			aaaaHdr := dnsmessage.ResourceHeader{Name: target, Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET, TTL: ttl}
			msg.Answers = append(msg.Answers, dnsmessage.Resource{Header: aaaaHdr, Body: &dnsmessage.AAAAResource{AAAA: aaaa}})
		}
	}

	packed, err := msg.Pack()
	if err != nil {
		return nil, fmt.Errorf("discovery: packing the announcement: %w", err)
	}
	return packed, nil
}
