package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// The mDNS multicast groups (RFC 6762 §3). Port 5353 for both families.
var (
	mdnsGroupV4 = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	mdnsGroupV6 = &net.UDPAddr{IP: net.ParseIP("ff02::fb"), Port: 5353}
)

// defaultInterval is how often the node re-announces. A responder that does not
// answer live queries must refresh within the record TTL so a late-joining
// client still learns the node exists; half the TTL is the conventional margin.
const defaultInterval = (defaultTTL / 2) * time.Second

// Options configure an Advertiser.
type Options struct {
	// Params is the interface-independent advertisement content (port, TLS, path).
	Params Params
	// TrustedNets is the interface allow-list, and it IS the guest trust boundary
	// (config.Guest.ParsedNets). An empty set advertises on nothing.
	TrustedNets []*net.IPNet
	// Interval overrides the re-announce cadence. Zero uses defaultInterval.
	Interval time.Duration
	Logger   *slog.Logger
}

// Advertiser announces `_heyarr._tcp` over mDNS on the trusted interfaces.
//
// It owns a background goroutine that sends an unsolicited announcement at Start
// and then every Interval, until Stop. The network and interface enumeration are
// behind injectable seams (lister, newCast) so the lifecycle is tested without a
// socket; the default seams read the real host.
type Advertiser struct {
	params   Params
	nets     []*net.IPNet
	interval time.Duration
	log      *slog.Logger

	lister  interfaceLister
	newCast func() (multicaster, error)

	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	running bool
}

// New builds an Advertiser. It binds nothing — Start does, so a caller can
// construct one unconditionally and only pay for a socket when it decides to
// advertise.
func New(opts Options) *Advertiser {
	interval := opts.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Advertiser{
		params:   opts.Params,
		nets:     opts.TrustedNets,
		interval: interval,
		log:      log.With("component", "discovery"),
		lister:   systemInterfaces,
		newCast:  newMulticaster,
	}
}

// Start selects the trusted interfaces and, if any qualify, begins announcing in
// the background. It returns whether it actually started: with no trusted,
// multicast-capable interface — an empty boundary, or a node with only loopback
// and off-boundary NICs — it opens no socket and reports false, so a caller can
// log the difference between "advertising" and "inert" rather than guessing.
func (a *Advertiser) Start(ctx context.Context) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return true, nil
	}

	ifaces, err := a.lister()
	if err != nil {
		return false, err
	}
	selected := selectTrusted(ifaces, a.nets)
	if len(selected) == 0 {
		a.log.Info("mDNS advertisement inert: no trusted, multicast-capable interface")
		return false, nil
	}
	cast, err := a.newCast()
	if err != nil {
		return false, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	a.running = true
	a.wg.Add(1)
	go a.run(runCtx, selected, cast)

	a.log.Info("mDNS advertisement started",
		"service", ServiceName(), "interfaces", ifaceNames(selected),
		"port", a.params.Port, "tls", a.params.TLS)
	return true, nil
}

func (a *Advertiser) run(ctx context.Context, selected []iface, cast multicaster) {
	defer a.wg.Done()
	defer func() { _ = cast.Close() }()

	// Announce at once: a client already listening hears the node without waiting
	// a full interval.
	a.announceAll(selected, cast)
	t := time.NewTicker(a.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.announceAll(selected, cast)
		}
	}
}

func (a *Advertiser) announceAll(selected []iface, cast multicaster) {
	for _, i := range selected {
		msg, err := a.params.Message(i.Addrs, 0)
		if err != nil {
			a.log.Warn("skipping an interface: building the announcement failed",
				"interface", i.Name, "error", err)
			continue
		}
		if err := cast.announce(i, msg); err != nil {
			// A send failure on a flapping or busy interface is not fatal: mirror
			// ssdp.go's resilience and keep announcing on the others.
			a.log.Warn("mDNS announcement send failed", "interface", i.Name, "error", err)
		}
	}
}

// Stop ends advertisement and waits for the background sender to return. It is
// safe to call more than once and is a no-op when Start never ran or was inert.
func (a *Advertiser) Stop() {
	a.mu.Lock()
	cancel := a.cancel
	a.cancel = nil
	a.running = false
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	a.wg.Wait()
}

// multicaster sends an already-marshalled announcement out one interface to the
// mDNS groups. It is injectable so the advertiser's lifecycle is exercised
// without a real socket.
type multicaster interface {
	announce(i iface, msg []byte) error
	io.Closer
}

// udpMulticaster sends over real IPv4 and IPv6 multicast sockets. It opens
// whichever families the host allows and errors only if neither would open.
type udpMulticaster struct {
	v4 *ipv4.PacketConn
	v6 *ipv6.PacketConn
}

func newMulticaster() (multicaster, error) {
	m := &udpMulticaster{}
	if c, err := net.ListenPacket("udp4", "0.0.0.0:0"); err == nil {
		m.v4 = ipv4.NewPacketConn(c)
	}
	if c, err := net.ListenPacket("udp6", "[::]:0"); err == nil {
		m.v6 = ipv6.NewPacketConn(c)
	}
	if m.v4 == nil && m.v6 == nil {
		return nil, errors.New("discovery: could not open any multicast socket")
	}
	return m, nil
}

func (m *udpMulticaster) announce(i iface, msg []byte) error {
	var hasV4, hasV6 bool
	for _, ip := range i.Addrs {
		if ip.To4() != nil {
			hasV4 = true
		} else {
			hasV6 = true
		}
	}
	ni := &net.Interface{Index: i.Index, Name: i.Name}
	var errs error
	if m.v4 != nil && hasV4 {
		if err := m.v4.SetMulticastInterface(ni); err != nil {
			errs = errors.Join(errs, fmt.Errorf("v4 interface %s: %w", i.Name, err))
		} else if _, err := m.v4.WriteTo(msg, nil, mdnsGroupV4); err != nil {
			errs = errors.Join(errs, fmt.Errorf("v4 send on %s: %w", i.Name, err))
		}
	}
	if m.v6 != nil && hasV6 {
		if err := m.v6.SetMulticastInterface(ni); err != nil {
			errs = errors.Join(errs, fmt.Errorf("v6 interface %s: %w", i.Name, err))
		} else if _, err := m.v6.WriteTo(msg, nil, mdnsGroupV6); err != nil {
			errs = errors.Join(errs, fmt.Errorf("v6 send on %s: %w", i.Name, err))
		}
	}
	return errs
}

func (m *udpMulticaster) Close() error {
	var errs error
	if m.v4 != nil {
		errs = errors.Join(errs, m.v4.Close())
	}
	if m.v6 != nil {
		errs = errors.Join(errs, m.v6.Close())
	}
	return errs
}
