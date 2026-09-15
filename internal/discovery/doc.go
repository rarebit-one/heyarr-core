// Package discovery advertises this node over mDNS / DNS-SD so a client on a
// trusted network can FIND it without being handed an address (ADR-0094
// §Discovery, Phase 2).
//
// It is the client-facing sibling of the SSDP the renderer package speaks. There
// the node is the SEARCHER — it M-SEARCHes for televisions and listens for their
// replies (internal/renderer/ssdp.go). Here it is the one ANNOUNCING ITSELF: it
// multicasts an unsolicited `_heyarr._tcp` DNS-SD response so a client that is
// listening, or that queries later, learns where the API is.
//
// The split has two halves, and only the second touches the network:
//
//   - record.go is a pure record builder. Given the resolved advertisement
//     (a Params: the port, whether it is TLS, the API base path) and the
//     addresses of one interface, it produces the DNS message — PTR, SRV, TXT
//     and A/AAAA — with no host, site or person named in it (make hygiene). It
//     is unit-tested without a socket.
//   - advertise.go is the multicast responder. It selects the interfaces whose
//     addresses fall inside the guest trust boundary (config.Guest.TrustedNets),
//     and on each of those — and ONLY those — periodically sends the record
//     builder's message to the mDNS groups (224.0.0.251:5353, ff02::fb). An
//     empty trusted set, or a node with no trusted interface, advertises on
//     nothing: the feature is off by construction, mirroring "empty allow-list =
//     tier off" from the guest tier it borrows its boundary from.
//
// What it does NOT do: it does not answer the split-horizon DNS name
// heyarr.thesim.family (mechanism 1 of the ADR, owned by AdGuard, and the app
// stays unaware of it), and it does not read personal state. It announces a base
// URL and a port on trusted networks, nothing more.
package discovery
