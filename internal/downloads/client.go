package downloads

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The Transmission download client, behind the provider registry's Downloader
// contract (§58, §59).

// DefaultLabel is what Heyarr tags its own transfers with when configuration
// does not say.
//
// # This is a safety property, not a convenience
//
// A download client is SHARED. The operator has their own transfers in it, and
// something that cannot tell them apart must never be allowed to remove,
// re-target or import anything. Every mutating operation filters on this first,
// and a transfer without it is invisible — even when its name matches exactly
// what Heyarr wanted.
//
// The failure this prevents is not subtle: an acquisition system that can
// delete an operator's data because a name matched is one nobody should run.
//
// It is CONFIGURABLE rather than a constant because two Heyarr instances can
// share one download client — a second machine being stood up, a migration in
// progress — and each must see only its own transfers. A hard-coded label makes
// the second instance adopt the first's work, which is the same failure as
// adopting the operator's, arriving by a different door.
const DefaultLabel = "heyarr"

// labelsRPCVersion is the RPC version that introduced torrent labels —
// Transmission 3.00.
//
// Below it, the *arr stack's fallback applies: a subdirectory of the download
// directory standing in for a category. Heyarr carries that for old instances
// and does not make it the default, because inheriting a workaround for a
// limitation fixed years ago is how software accumulates its predecessors'
// scar tissue.
const labelsRPCVersion = 16

// minimumRPCVersion is the oldest Transmission this client will drive.
//
// 14 is Transmission 2.40, which is where torrent-get's field selection
// stabilised. Below it the calls this client makes do not exist in the shape it
// expects, and pretending otherwise would produce parse failures that look like
// a Heyarr bug.
//
// A version below this is reported UNHEALTHY NAMING BOTH NUMBERS and is never
// a startup failure (ADR-0025): a download client that is too old must not stop
// Heyarr from serving the library.
const minimumRPCVersion = 14

// defaultTimeout bounds one RPC call.
//
// torrent-get over a large queue with trackerStats is the slow one, and it is
// still well under a second on anything healthy. Generous enough that a busy
// daemon is not mistaken for a dead one; short enough that the poll job does
// not hold a lease waiting on something that will never answer.
const defaultTimeout = 30 * time.Second

// Options configure a Transmission client.
type Options struct {
	// Name is the provider's name, from configuration.
	Name string
	// Endpoint is the RPC URL.
	Endpoint string
	// Username and Password are optional: an operator running Transmission on
	// a trusted network with authentication off is an ordinary, supported
	// deployment (see providers.needsCredential).
	Username string
	Password string
	// PathMap translates the client's namespace into Heyarr's.
	PathMap PathMap
	// Label tags this instance's transfers. Empty means DefaultLabel.
	Label string
	// Capabilities is what configuration declared.
	Capabilities []providers.Capability
	// HTTPClient is injectable so tests drive the real transport against the
	// replayed corpus rather than against a stub of it.
	HTTPClient *http.Client
	// Now is injected so health timestamps are fixed in a test (ADR-0017).
	Now func() time.Time
}

// Client drives one Transmission instance.
type Client struct {
	name    string
	caps    []providers.Capability
	pathMap PathMap
	label   string
	rpc     *transport
	now     func() time.Time

	// session is what the last successful session-get reported. It is a cache
	// of facts that change only when the daemon is reconfigured, and it is
	// refreshed by every health check.
	session sessionInfo
}

// sessionInfo is what session-get told us about the instance.
type sessionInfo struct {
	Version              string
	RPCVersion           int
	RPCVersionMinimum    int
	DownloadDir          string
	IncompleteDir        string
	IncompleteDirEnabled bool
	// Known is false until a session-get has succeeded. Distinct from a
	// zero-valued struct: "we have not asked" and "it reported zeros" lead to
	// different behaviour, and the labels decision turns on it.
	Known bool
}

// SupportsLabels reports whether this instance has torrent labels.
//
// When the version is UNKNOWN this answers false — the conservative direction.
// Assuming labels on an instance that lacks them would mean Heyarr queues
// transfers it can never afterwards recognise as its own, which is precisely
// the situation the label exists to prevent.
func (s sessionInfo) SupportsLabels() bool {
	return s.Known && s.RPCVersion >= labelsRPCVersion
}

// New builds a Transmission client.
func New(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return nil, errors.New("downloads: a transmission client needs a provider name")
	}
	if strings.TrimSpace(opts.Endpoint) == "" {
		return nil, errors.New("downloads: a transmission client needs an endpoint")
	}
	httpc := opts.HTTPClient
	if httpc == nil {
		httpc = &http.Client{Timeout: defaultTimeout}
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	caps := opts.Capabilities
	if len(caps) == 0 {
		caps = []providers.Capability{providers.CapabilityDownload}
	}

	label := strings.TrimSpace(opts.Label)
	if label == "" {
		label = DefaultLabel
	}

	return &Client{
		name:    strings.TrimSpace(opts.Name),
		caps:    caps,
		pathMap: opts.PathMap,
		label:   label,
		now:     now,
		rpc: &transport{
			endpoint: rpcURL(opts.Endpoint),
			user:     opts.Username,
			pass:     opts.Password,
			http:     httpc,
		},
	}, nil
}

// rpcURL appends Transmission's RPC path when the operator gave a base URL.
//
// Both forms are configured in the wild — the base and the full path — and
// refusing one would be Heyarr being precious about a detail with an obvious
// right answer.
func rpcURL(endpoint string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if strings.HasSuffix(trimmed, "/transmission/rpc") {
		return trimmed
	}
	return trimmed + "/transmission/rpc"
}

var _ providers.Downloader = (*Client)(nil)

// Name implements providers.Provider.
func (c *Client) Name() string { return c.name }

// Capabilities implements providers.Provider.
func (c *Client) Capabilities() []providers.Capability {
	return append([]providers.Capability(nil), c.caps...)
}

// Check exercises the instance and reports what it found (ADR-0025).
//
// It EXERCISES: a session-get is a real round trip that returns real facts. A
// client that reported itself healthy because it was configured would advertise
// something it might not deliver, and work would route to it and then fail.
//
// The version check is what replaces ADR-0023's pinning for a service Heyarr
// does not install. Not controlling the version does not mean ignoring it — an
// unsupported one is reported UNHEALTHY NAMING BOTH NUMBERS, so "acquisitions
// stopped after I upgraded the NAS" is one request away from an answer rather
// than an afternoon.
func (c *Client) Check(ctx context.Context) providers.Health {
	at := c.now()

	session, err := c.sessionGet(ctx)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnauthorised):
			// A configuration problem, said as one. "Unreachable" would send
			// an operator to look at the network for a wrong password.
			return providers.Unhealthy(
				"the credential was refused — check the username and password", at)
		default:
			return providers.Unhealthy(shortError(err), at)
		}
	}
	c.session = session

	if session.RPCVersion < minimumRPCVersion {
		return providers.Unhealthy(fmt.Sprintf(
			"RPC version %d is below the minimum this client supports (%d); "+
				"transfers will not be driven until Transmission is upgraded",
			session.RPCVersion, minimumRPCVersion), at)
	}

	health := providers.Healthy(session.Version, at)
	if !session.SupportsLabels() {
		// Degraded, not unhealthy: the fallback works. Reported so the
		// degradation is legible rather than mysterious — an operator
		// wondering why their transfers land in a subdirectory should find the
		// answer here rather than in the source.
		health.Detail = fmt.Sprintf(
			"reachable; RPC version %d has no torrent labels (needs %d), "+
				"so transfers are tagged by download subdirectory instead",
			session.RPCVersion, labelsRPCVersion)
	}
	return health
}

// Label is what this client tags and recognises its transfers by.
func (c *Client) Label() string { return c.label }

// Session exposes what the last check learnt, for callers that need the
// download directory or the labels decision.
func (c *Client) Session() sessionInfo { return c.session }

// shortError renders an error for a health detail.
//
// Health details are read by an operator on GET /api/v1/providers, and a Go
// error chain rendered in full is not a sentence. It also must never carry a
// credential — which is why the transport's errors name the endpoint and
// nothing else.
func shortError(err error) string {
	var rpcErr *rpcError
	if errors.As(err, &rpcErr) {
		return rpcErr.Detail
	}
	return "unreachable"
}
