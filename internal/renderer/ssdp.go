package renderer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

// SSDP discovery (§68).
//
// M-SEARCH is a UDP multicast question and every answer is a unicast reply
// carrying a LOCATION to fetch. There is no completion signal — a device that
// is asleep, or whose reply was dropped, is indistinguishable from one that
// does not exist — so discovery is bounded by time and returns what answered,
// never "everything there is".
//
// That distinction is not pedantic here. A Samsung television closes every
// listener in standby and, on at least one model, stops answering ARP
// altogether: it is absent from the network rather than idle on it. So an
// empty result means "nothing answered in this window", and a caller that
// reports it as "you have no renderers" will be wrong in a way the household
// notices.

const (
	ssdpMulticast = "239.255.255.250:1900"
	// ssdpMediaRenderer is the search target. Searching for the renderer
	// device type rather than ssdp:all keeps the answer set small on a busy
	// network — an M-SEARCH for everything on a home LAN returns routers,
	// printers and every NAS share.
	ssdpMediaRenderer = "urn:schemas-upnp-org:device:MediaRenderer:1"
	// ssdpMX is the largest random delay, in seconds, a device may wait before
	// replying. Devices spread their answers across this window to avoid
	// stampeding the searcher, so it is also the floor on how long discovery
	// must listen to hear a well-behaved device.
	ssdpMX = 3
	// ssdpRetries is how many M-SEARCHes go out. UDP multicast is lossy and a
	// dropped question is silent; three is the conventional answer and costs
	// three datagrams.
	ssdpRetries = 3
)

// DiscoverOptions bounds a discovery sweep.
type DiscoverOptions struct {
	// Timeout is how long to listen. It must comfortably exceed ssdpMX or
	// slower devices are cut off mid-answer.
	Timeout time.Duration
	// Interface, when set, is the local interface to search from. Empty lets
	// the host choose, which is right on a single-homed machine and wrong on
	// a peer with several NICs where only one faces the televisions.
	Interface *net.UDPAddr
}

// Discover finds MediaRenderers by SSDP and returns their advertised locations.
//
// It returns locations rather than Renderers because fetching and parsing each
// description is a separate HTTP round trip per device, and a caller that only
// wants to know whether anything is out there should not pay for all of them.
// DiscoverRenderers is the version that follows through.
func Discover(ctx context.Context, opts DiscoverOptions) ([]string, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	target, err := net.ResolveUDPAddr("udp4", ssdpMulticast)
	if err != nil {
		return nil, fmt.Errorf("renderer: resolving the SSDP address: %w", err)
	}
	conn, err := net.ListenUDP("udp4", opts.Interface)
	if err != nil {
		return nil, fmt.Errorf("renderer: opening an SSDP socket: %w", err)
	}
	defer func() { _ = conn.Close() }()

	deadline := time.Now().Add(opts.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("renderer: setting the SSDP deadline: %w", err)
	}

	search := "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: " + ssdpMulticast + "\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		fmt.Sprintf("MX: %d\r\n", ssdpMX) +
		"ST: " + ssdpMediaRenderer + "\r\n\r\n"
	for range ssdpRetries {
		if _, err := conn.WriteToUDP([]byte(search), target); err != nil {
			return nil, fmt.Errorf("renderer: sending M-SEARCH: %w", err)
		}
	}

	var locations []string
	seen := make(map[string]bool)
	buf := make([]byte, 8192)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			// A timeout is how this loop ends, not a failure: the window
			// closed and whatever answered is the answer.
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				break
			}
			return locations, fmt.Errorf("renderer: reading an SSDP reply: %w", err)
		}
		loc := locationOf(buf[:n])
		if loc == "" || seen[loc] {
			// A device answers once per advertised service, so the same
			// location arrives several times for one renderer.
			continue
		}
		seen[loc] = true
		locations = append(locations, loc)
	}
	return locations, ctx.Err()
}

// locationOf reads the LOCATION header out of an SSDP reply.
//
// The reply is HTTP-shaped without being HTTP, so it is parsed as a status
// line plus MIME headers rather than with http.ReadResponse, which insists on
// a valid HTTP version token that some devices do not send.
func locationOf(datagram []byte) string {
	r := textproto.NewReader(bufio.NewReader(bytes.NewReader(datagram)))
	if _, err := r.ReadLine(); err != nil { // the HTTP/1.1 200 OK status line
		return ""
	}
	headers, err := r.ReadMIMEHeader()
	if err != nil {
		// Truncated replies are common enough not to be worth a log line, and
		// whatever headers were read before the error are still usable.
		if headers == nil {
			return ""
		}
	}
	return strings.TrimSpace(http.Header(headers).Get("Location"))
}

// DiscoverRenderers finds renderers and describes each one.
//
// A device that answers M-SEARCH and then fails to describe itself is skipped
// rather than failing the sweep: one unreachable television must not hide the
// speaker next to it. The errors are returned alongside the renderers so a
// caller can say what it could not read.
func DiscoverRenderers(ctx context.Context, client *http.Client, opts DiscoverOptions) ([]Renderer, []error) {
	locations, err := Discover(ctx, opts)
	var problems []error
	if err != nil {
		problems = append(problems, err)
	}
	var found []Renderer
	for _, loc := range locations {
		r, err := Describe(ctx, client, loc)
		if err != nil {
			// ErrNotRenderer is expected and uninteresting: it is how a device
			// that answered a MediaRenderer search without being one gets
			// dropped. Anything else is worth telling the caller about.
			if !errors.Is(err, ErrNotRenderer) {
				problems = append(problems, err)
			}
			continue
		}
		found = append(found, r)
	}
	return found, problems
}
