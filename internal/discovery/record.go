package discovery

import (
	"fmt"
	"net"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	// ServiceTypeMedia is the DNS-SD service a heyarr node (config.ProfileMedia)
	// advertises (ADR-0094): the client-facing name that answers "is there a
	// heyarr on this network?".
	ServiceTypeMedia = "_heyarr._tcp"

	// ServiceTypePersonal is the DNS-SD service a Mnemosyne node
	// (config.ProfilePersonal, ADR-0107) advertises. It is a different type on
	// purpose: the two services share a binary's worth of code but not an API
	// surface or a token namespace, so a heyarr client browsing `_heyarr._tcp`
	// must never be offered a Mnemosyne node as a heyarr server.
	ServiceTypePersonal = "_mnemosyne._tcp"

	// ServiceType is the media service type, kept for callers that predate the
	// personal profile. New code should name the Service it means.
	ServiceType = ServiceTypeMedia

	// Domain is the mDNS link-local domain. mDNS names always live under
	// ".local." (RFC 6762 §3); nothing here resolves in the global DNS.
	Domain = "local."

	// defaultTTL is the record lifetime a client may cache the answer for, in
	// seconds. It doubles as the re-announce horizon: a responder that does not
	// answer live queries (this one) must refresh within it so a client that
	// joined late still learns the node exists.
	defaultTTL = 120
)

// Service is the DNS-SD identity a node announces under: the service type a
// browsing client asks a PTR query for, and the fixed label that names the
// instance and the SRV target.
//
// Label is a FIXED, non-identifying token rather than a friendly per-node name
// on purpose: a real name ("Living Room", the peer's site) would both leak into
// a public repo's fixtures (make hygiene) and put personal detail on the wire.
// Disambiguating an active-active pair by a stable, non-personal token is a
// later refinement; mechanism 1 (split-horizon DNS, nearest-server) already
// picks between a pair, so mDNS naming them apart is not what makes discovery
// work. The address a client actually dials comes from the A/AAAA records built
// per interface, not from the label.
type Service struct {
	// Type is the DNS-SD service type, e.g. "_heyarr._tcp".
	Type string
	// Label is the instance name and the host label of the SRV target.
	Label string
}

// The two services this repository ships, one per config.Profile. The zero
// Service means ServiceMedia so a Params that predates the personal profile
// still announces exactly what it always did.
var (
	// ServiceMedia is heyarr: `heyarr._heyarr._tcp.local.` -> `heyarr.local.`.
	ServiceMedia = Service{Type: ServiceTypeMedia, Label: "heyarr"}
	// ServicePersonal is Mnemosyne: `mnemosyne._mnemosyne._tcp.local.` ->
	// `mnemosyne.local.`.
	ServicePersonal = Service{Type: ServiceTypePersonal, Label: "mnemosyne"}
)

// Name is the fully-qualified DNS-SD service name, e.g. "_heyarr._tcp.local.".
// A browsing client asks a PTR query for exactly this.
func (s Service) Name() string { return s.Type + "." + Domain }

// InstanceName is the fully-qualified service instance name, e.g.
// "heyarr._heyarr._tcp.local.". It is the PTR target and the SRV/TXT owner.
func (s Service) InstanceName() string { return s.Label + "." + s.Name() }

// HostName is the SRV target — the .local name whose A/AAAA records carry the
// interface address, e.g. "heyarr.local.".
func (s Service) HostName() string { return s.Label + "." + Domain }

// ServiceName is ServiceMedia.Name(): the media service's fully-qualified
// DNS-SD name, "_heyarr._tcp.local.".
func ServiceName() string { return ServiceMedia.Name() }

// InstanceName is ServiceMedia.InstanceName(): "heyarr._heyarr._tcp.local.".
func InstanceName() string { return ServiceMedia.InstanceName() }

// Params is the interface-independent content of an advertisement: everything a
// client needs to build a base URL except the address, which is per-interface.
//
// It is deliberately minimal. The wire is a broadcast medium on a shared LAN, so
// it carries only what a client cannot derive: the port, whether that port
// speaks TLS, and the API base path. It names no host, no site, no person, and
// no library — those are either private or discoverable only after the client
// authenticates.
type Params struct {
	// Service is the DNS-SD service to announce as. The zero value is
	// ServiceMedia; a Mnemosyne node (config.ProfilePersonal) sets
	// ServicePersonal so heyarr clients browsing `_heyarr._tcp` never see it.
	Service Service
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

// service resolves the Service to announce as, defaulting the zero value to
// ServiceMedia.
func (p Params) service() Service {
	if p.Service == (Service{}) {
		return ServiceMedia
	}
	return p.Service
}

// Message builds the unsolicited mDNS response announcing this instance on one
// interface, whose usable addresses are addrs. It is the pure heart of the
// package: a config plus a set of addresses in, a marshalled DNS packet out, no
// socket touched.
//
// The answer set is the DNS-SD quartet plus address records, shown here for
// ServiceMedia (ServicePersonal substitutes `_mnemosyne._tcp` / `mnemosyne`):
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

	svc := p.service()
	service, err := dnsmessage.NewName(svc.Name())
	if err != nil {
		return nil, fmt.Errorf("discovery: service name: %w", err)
	}
	instance, err := dnsmessage.NewName(svc.InstanceName())
	if err != nil {
		return nil, fmt.Errorf("discovery: instance name: %w", err)
	}
	target, err := dnsmessage.NewName(svc.HostName())
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
