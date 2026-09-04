// Package config loads layered configuration: file, then HEYARR_ environment,
// then flags.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	env "github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// EnvPrefix is the prefix for environment overrides. HEYARR_HTTP_ADDR maps to
// http.addr, HEYARR_LOG_LEVEL to log.level, and so on.
const EnvPrefix = "HEYARR_"

// Config is the whole of Heyarr's configuration. It is passed to every role;
// no package reads configuration from anywhere else.
type Config struct {
	// DataDir is the root beneath which Heyarr keeps everything it owns.
	// CASRoot and Database default to paths inside it.
	DataDir string `koanf:"data_dir"`

	CAS       CAS       `koanf:"cas"`
	Database  Database  `koanf:"database"`
	HTTP      HTTP      `koanf:"http"`
	Peer      Peer      `koanf:"peer"`
	Log       Log       `koanf:"log"`
	Media     Media     `koanf:"media"`
	Libraries []Library `koanf:"libraries"`

	// Providers configures the external services Heyarr talks to — indexers,
	// download clients — through the centralised registry (§59, M3-07).
	//
	// It lives here, alongside `libraries:` and `peer:`, rather than in the
	// database: a node's whole identity is then one reviewable document, and
	// standing up a second machine is copying a file rather than replaying a
	// sequence of API calls. It is also where the operator's other secrets
	// already are.
	//
	// An empty list is a supported, tested configuration — a Heyarr with no
	// providers still scans, ingests, catalogues, verifies, serves ranges and
	// plays. Search and acquire jobs simply stay pending and visible
	// (ADR-0025).
	Providers []providers.Entry `koanf:"providers"`

	// Backup configures the continuous backup of this peer's control plane
	// (§49, ADR-0044). The database is where it lives; the backups are copies of
	// it, so they live under the data directory too unless pointed elsewhere.
	Backup Backup `koanf:"backup"`

	// Notify configures the Voidbind push/wake plane (ADR-0055): the self-hosted
	// ntfy server that carries a login push to a paired device. Push is additive
	// to the QR web-login (ADR-0053) — the QR stays the primary channel — so an
	// empty configuration is fully supported: the login broker still mounts and
	// still shows a QR, it simply wakes no device.
	Notify Notify `koanf:"notify"`
}

// Notify configures the push/wake login channel (ADR-0055). The subscription
// address book and fan-out live in voidbind-go/notify; this records only the
// operator-facing deployment detail.
type Notify struct {
	// NtfyBaseURL records this deployment's self-hosted ntfy origin. It is
	// informational: a device registers its FULL ntfy topic URL as its
	// subscription endpoint (the wake channel POSTs there directly), so the plane
	// works without this being set. It is logged at startup so an operator can see
	// which ntfy server a login push is meant for, and defaults empty (no default
	// public server is assumed — push is opt-in on the device registering a topic).
	NtfyBaseURL string `koanf:"ntfy_base_url"`
}

// Backup configures the control-plane backup cadence (§49, ADR-0044).
type Backup struct {
	// Interval is the backup cadence as a duration string ("5m"). It is also the
	// RPO: at most this much control-plane work is lost if the disk dies between
	// backups (ADR-0044 question 1). Empty or "0" disables the background
	// cadence; `heyarr backup` still takes one on demand.
	Interval string `koanf:"interval"`
	// Dir is where backups are written. Empty derives <data_dir>/backups, so a
	// single data_dir move takes the backups with it.
	Dir string `koanf:"dir"`
	// PeerRetain is how many generations of EACH peer's control-plane backup
	// this node keeps when peers push to it (§50, M7-03). Zero uses a sensible
	// default. Keeping only the newest leaves nothing when the newest is the
	// copy written during the incident, so the default is more than one.
	PeerRetain int `koanf:"peer_retain"`
}

// CAS configures the content-addressed store. Its on-disk layout is private to
// the storage fabric; only the root is configurable (ADR-0006).
type CAS struct {
	Root string `koanf:"root"`
}

// Database configures the controller's SQLite database (ADR-0003).
type Database struct {
	Path string `koanf:"path"`
}

// HTTP configures the API listeners.
type HTTP struct {
	// Addr is the TCP listen address. It defaults to loopback, and binding a
	// non-loopback address with Auth.Enabled false is refused (ADR-0011).
	Addr string `koanf:"addr"`
	// UnixSocket is the preferred local transport; empty disables it.
	UnixSocket string `koanf:"unix_socket"`
	Auth       Auth   `koanf:"auth"`
	// Guest optionally admits a credential-less caller as an anonymous,
	// read-only Guest over the shared library (ADR-0074). It defaults off: with
	// it disabled a request that presents no credential is refused exactly as
	// before. Enabling it on a non-loopback listener is a deliberate decision to
	// let anyone who can reach the listener read the shared library.
	Guest Guest `koanf:"guest"`
	// TLS optionally serves the TCP client API over HTTPS (ADR-0072). Both files
	// set → the TCP listener is served with ServeTLS; neither → plain HTTP as
	// before (the default); exactly one → a startup error. The unix socket is
	// never wrapped: it is the local IPC transport the CLI and workers dial.
	TLS TLS `koanf:"tls"`
	// PublicOrigin is the external origin clients reach this node at — the
	// scheme, host and any port a browser or television types, e.g.
	// "https://heyarr.example.com". It is what the login/session rp origin and
	// the rendered base URL use when set, because a listener derives an IP:port
	// (renderBaseURL) and a Voidbind login needs the https HOSTNAME behind the
	// reverse proxy or TLS listener, not the address the socket bound. Empty
	// keeps today's derived behaviour (ADR-0072).
	PublicOrigin string `koanf:"public_origin"`
}

// TLS points at the certificate and key that serve the client API over HTTPS.
//
// The two are all-or-nothing: both set turns TLS on, neither leaves the plain
// HTTP behaviour untouched, and exactly one is refused at startup rather than
// falling silently back to plaintext on a listener the operator believed was
// encrypted (ADR-0072). The process only READS these files; issuing and
// renewing the certificate is an out-of-process concern (a systemd + lego
// DNS-01 timer), and a renewal is picked up on the next restart or SIGHUP.
type TLS struct {
	CertFile string `koanf:"cert_file"`
	KeyFile  string `koanf:"key_file"`
}

// Enabled reports whether both halves are set, which is the one state that
// turns TLS on. A method rather than a resolved field so the zero HTTP means
// "plain", the same as an unmentioned key.
func (t TLS) Enabled() bool { return t.CertFile != "" && t.KeyFile != "" }

// Auth configures API authentication.
type Auth struct {
	Enabled bool `koanf:"enabled"`
}

// Guest configures anonymous read-only browse (ADR-0074). Disabled by default:
// the zero value is off, so an unmentioned key leaves the safe stance — a
// credential-less request is refused — untouched. Enabled admits such a request
// as a first-class Guest identity holding only the read scope.
type Guest struct {
	Enabled bool `koanf:"enabled"`
}

// Peer identifies this node within the Heyarr instance. A peer row exists from
// Milestone 1 even though there is only one (ADR-0010).
type Peer struct {
	// Endpoint is how OTHER roles reach this node's HTTP API — the worker
	// probing a blob over ranges (§29), and in Milestone 4 another peer.
	//
	// Empty means "derive it", which covers every single-host deployment:
	// a configured unix socket becomes unix://<path>, and a concrete TCP bind
	// becomes http://<addr>. It has to be settable because derivation cannot
	// work across hosts — 127.0.0.1 is not an address another machine can use,
	// and a wildcard bind names no host at all.
	Endpoint string `koanf:"endpoint"`

	// Listen is the address the mTLS peer surface binds (§26, ADR-0012).
	//
	// Empty — the default — means this node serves no peer surface at all, and
	// that is the correct state for a deployment with one peer: there is
	// nothing to be a member of, and `heyarr all` on a laptop must not need a
	// certificate to talk to itself. Nothing about this listener is
	// configurable beyond the address, because there is nothing else to
	// configure: the certificate is derived from this node's Ed25519 identity
	// in memory, is regenerated before it expires, and is never written down.
	//
	// It is safe on a routable address, which http.addr is not (ADR-0011).
	// That is the point of the split: this listener refuses every connection
	// that does not present a client certificate whose public key a membership
	// record pins, and it refuses it during the handshake.
	Listen string `koanf:"listen"`

	// Name is this peer's stable name within the instance.
	Name string `koanf:"name"`
	// Site is the physical failure domain (spec §35). Two peers sharing a site
	// provide much weaker resilience than two at different sites, so this is
	// recorded rather than inferred.
	Site string `koanf:"site"`

	// ServePieces is whether this node answers piece exchange on its peer
	// surface (§26, §27, ADR-0042). Default true; a pointer so that "not
	// mentioned" and "set to false" are different things.
	//
	// # What turning it off makes this node
	//
	// A WEB SEED, in §27's sense: a member that serves byte ranges of blobs it
	// holds WHOLE, over the ordinary content route, and takes no part in
	// swarms. Its content route is unchanged, its inventory and manifests are
	// unchanged, and a peer fetching a whole blob from it sees no difference
	// at all.
	//
	// # Why anyone would
	//
	// Serving pieces means answering an availability question per blob and a
	// request per piece, which is many small reads and many small responses
	// where a whole-blob pull is one large sequential one. On a low-power NAS
	// holding an archive tier that is read rarely and never acquired to, that
	// is CPU and IOPS spent on a role the node is not there to play. §19's
	// peer modes already say a fabric is not uniform; this says the same thing
	// about the transport rather than about storage.
	//
	// # Why it is not inferred
	//
	// Before this existed, `Blobs` and `Pieces` were built from the same store
	// in the same call, so a peer served both or neither and §27's web seed
	// had NO COUNTERPARTY — nothing in the tree could produce a node one was
	// correct for (#266). A transport that chose a source kind was therefore
	// choosing between one real case and one that could not occur. This is the
	// counterparty, stated by an operator rather than discovered by probing:
	// ADR-0042 is right that a peer whose piece route is BROKEN must not look
	// like one that never served pieces, and only a declaration tells them
	// apart.
	ServePieces *bool `koanf:"serve_pieces"`
}

// ServesPieces reports whether this node answers piece exchange, defaulting to
// true when configuration does not say.
//
// A method rather than a resolved field, so that the zero Peer — which several
// tests and `heyarr all` on a laptop construct — means the same thing as an
// unmentioned key rather than meaning "off".
func (p Peer) ServesPieces() bool { return p.ServePieces == nil || *p.ServePieces }

// Log configures diagnostic output.
type Log struct {
	// Level is one of debug, info, warn, error.
	Level string `koanf:"level"`
	// Format is json, text, or auto (text when stderr is a terminal).
	Format string `koanf:"format"`
}

// Media locates the external audiovisual toolchain (§10, ADR-0023).
//
// Both paths are empty by default, which means "look on PATH, and degrade if
// it is not there" — a node with no FFmpeg is a supported configuration, not a
// broken one. Setting one is a statement that THIS binary is to be used, and a
// value that does not work is a startup failure rather than a silent fall back
// to PATH. See internal/media for why those two cases are not symmetrical.
type Media struct {
	FFprobePath string `koanf:"ffprobe_path"`
	FFmpegPath  string `koanf:"ffmpeg_path"`
	// StreamConcurrency caps the on-the-fly repackages a node runs at once
	// (ADR-0069). Each is one ffmpeg, and one that re-encodes video is a
	// core. Zero means the default of two; a client past the cap is told to
	// retry rather than queued.
	StreamConcurrency int `koanf:"stream_concurrency"`
}

// PeerEndpoint is how another role reaches this node's API.
//
// The unix socket is preferred over TCP when both exist, for the same reason
// the CLI prefers it: it is always present, always local, and needs no port to
// be guessed. A wildcard or ephemeral TCP bind ("", ":0", "0.0.0.0:8080")
// yields nothing, because none of those name an address anything can dial —
// and returning a plausible-looking one would produce a probe that fails at
// runtime instead of a configuration that is visibly incomplete.
func (c Config) PeerEndpoint() string {
	if c.Peer.Endpoint != "" {
		return c.Peer.Endpoint
	}
	if c.HTTP.UnixSocket != "" {
		return "unix://" + c.HTTP.UnixSocket
	}
	addr := c.HTTP.Addr
	if addr == "" {
		return ""
	}
	host, port, found := strings.Cut(addr, ":")
	if !found || port == "" || port == "0" {
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "[::]" {
		// A wildcard bind is reachable at many addresses and names none of
		// them. Guessing 127.0.0.1 would be right on one host and wrong the
		// moment the worker moves, which is a supported deployment.
		return ""
	}
	return "http://" + addr
}

// Library is a managed collection of content rooted at one or more paths.
type Library struct {
	Name        string   `koanf:"name"`
	ContentType string   `koanf:"content_type"`
	Roots       []string `koanf:"roots"`
}

// Defaults returns the configuration Heyarr uses when nothing is specified.
// The defaults are deliberately safe rather than convenient: loopback only,
// authentication on.
func Defaults() Config {
	return Config{
		DataDir:  "/var/lib/heyarr",
		HTTP:     HTTP{Addr: "127.0.0.1:7777", Auth: Auth{Enabled: true}},
		Peer:     Peer{Name: "local"},
		Log:      Log{Level: "info", Format: "auto"},
		CAS:      CAS{},
		Database: Database{},
		Media:    Media{StreamConcurrency: 2},
		Backup:   Backup{Interval: "5m"},
	}
}

// Load resolves configuration from defaults, then the file at path if it is
// non-empty, then HEYARR_ environment variables. The result is validated.
func Load(path string) (Config, error) {
	k := koanf.New(".")

	if err := k.Load(structs.Provider(Defaults(), "koanf"), nil); err != nil {
		return Config{}, fmt.Errorf("loading defaults: %w", err)
	}

	if path != "" {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			// A config path given explicitly and then not readable is always a
			// mistake worth stopping for; say which path, since the usual cause
			// is a typo or a relative path resolved from an unexpected cwd.
			return Config{}, fmt.Errorf("reading config %s: %w", path, err)
		}
	}

	// Environment keys are resolved against the canonical key set rather than
	// by a separator rule. `data_dir` and `log.level` use different separators,
	// so any single rule gets one of them wrong — HEYARR_DATA__DIR would become
	// `data.dir`, which silently matches nothing and leaves the default in
	// place. Matching on separators-removed makes HEYARR_DATA_DIR and
	// HEYARR_LOG_LEVEL both work, and an unrecognised key is dropped rather
	// than invented.
	canonical := canonicalKeys(k.Keys())
	if err := k.Load(env.Provider(".", env.Opt{
		Prefix: EnvPrefix,
		TransformFunc: func(key, value string) (string, any) {
			bare := strings.ToLower(strings.TrimPrefix(key, EnvPrefix))
			if resolved, ok := canonical[squashSeparators(bare)]; ok {
				return resolved, value
			}
			// Not a key we know. Returning it unchanged would create a
			// meaningless entry; returning empty drops it.
			return "", nil
		},
	}), nil); err != nil {
		return Config{}, fmt.Errorf("loading environment: %w", err)
	}

	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing configuration: %w", err)
	}

	cfg.applyDerivedDefaults()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// applyDerivedDefaults fills in the paths that hang off DataDir. It runs after
// loading so that setting data_dir alone moves everything, while setting a path
// explicitly still wins.
func (c *Config) applyDerivedDefaults() {
	if c.CAS.Root == "" && c.DataDir != "" {
		c.CAS.Root = filepath.Join(c.DataDir, "cas")
	}
	if c.Database.Path == "" && c.DataDir != "" {
		c.Database.Path = filepath.Join(c.DataDir, "heyarr.db")
	}
	if c.HTTP.UnixSocket == "" && c.DataDir != "" {
		c.HTTP.UnixSocket = filepath.Join(c.DataDir, "heyarr.sock")
	}
	if c.Backup.Dir == "" && c.DataDir != "" {
		c.Backup.Dir = filepath.Join(c.DataDir, "backups")
	}
}

// BackupInterval parses the configured cadence, or 0 when the background
// cadence is disabled. A malformed value is a startup error rather than a
// silently-ignored one — a backup nobody takes is the failure this exists to
// prevent.
func (c Config) BackupInterval() (time.Duration, error) {
	s := strings.TrimSpace(c.Backup.Interval)
	if s == "" || s == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("config: backup.interval %q is not a duration (e.g. \"5m\"): %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("config: backup.interval must not be negative, got %q", s)
	}
	return d, nil
}

var validLogLevels = []string{"debug", "info", "warn", "error"}

// Validate reports the first configuration problem, phrased so the operator can
// act on it without reading the source. Configuration is checked before any
// role starts: failing at startup is far cheaper than failing on first write.
func (c Config) Validate() error {
	if c.DataDir == "" {
		return errors.New("config: data_dir must be set")
	}
	if !filepath.IsAbs(c.DataDir) {
		return fmt.Errorf("config: data_dir must be an absolute path, got %q", c.DataDir)
	}
	if !slicesContains(validLogLevels, c.Log.Level) {
		return fmt.Errorf("config: log.level %q is not one of %s", c.Log.Level, strings.Join(validLogLevels, ", "))
	}
	if f := c.Log.Format; f != "json" && f != "text" && f != "auto" {
		return fmt.Errorf("config: log.format %q is not one of json, text, auto", f)
	}
	if _, err := c.BackupInterval(); err != nil {
		return err
	}
	if c.Peer.Name == "" {
		return errors.New("config: peer.name must be set")
	}
	if c.Media.StreamConcurrency < 0 {
		return fmt.Errorf("config: media.stream_concurrency must be zero or more, got %d", c.Media.StreamConcurrency)
	}
	// The peer surface's address is checked for shape only. There is
	// deliberately no loopback rule here and no equivalent of ADR-0011's
	// refusal: this listener is mutually authenticated and pinned to
	// membership (ADR-0012), so binding it publicly is what it is FOR, and a
	// rule that pushed operators towards a tunnel would contradict the ADR's
	// "must not treat an existing site-to-site VPN as its security boundary".
	if c.Peer.Listen != "" {
		if _, _, err := net.SplitHostPort(c.Peer.Listen); err != nil {
			return fmt.Errorf("config: peer.listen %q is not a valid listen address: %w", c.Peer.Listen, err)
		}
	}

	// TLS is all-or-nothing (ADR-0072). Exactly one half set is the dangerous
	// state — a listener the operator believed was encrypted, quietly serving
	// plaintext, or a key with no certificate — so it is refused here rather
	// than falling back. Both-set and neither-set are the two supported shapes.
	if (c.HTTP.TLS.CertFile == "") != (c.HTTP.TLS.KeyFile == "") {
		return fmt.Errorf(
			"config: http.tls needs both cert_file and key_file or neither — "+
				"got cert_file=%q key_file=%q; set both to serve HTTPS or clear both to serve plain HTTP",
			c.HTTP.TLS.CertFile, c.HTTP.TLS.KeyFile)
	}

	// A public origin, when set, must be an absolute http(s) URL with a host —
	// it becomes the login/session rp origin, and an origin that is merely
	// plausible produces a browser that cannot complete a login, which reads as
	// Heyarr being broken rather than as a misconfiguration.
	if o := strings.TrimSpace(c.HTTP.PublicOrigin); o != "" {
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf(
				"config: http.public_origin %q is not an absolute http(s) origin (e.g. https://heyarr.example.com)", o)
		}
	}

	// ADR-0011. Milestone 1 has no identity model, and this server range-serves
	// every byte of the library. A warning in a log is not a control, so an
	// unauthenticated non-loopback listener is refused outright rather than
	// noted. Milestone 8 replaces the token scheme, not this rule.
	if !c.HTTP.Auth.Enabled && c.HTTP.Addr != "" {
		nonLoopback, err := bindsNonLoopback(c.HTTP.Addr)
		if err != nil {
			return fmt.Errorf("config: http.addr %q is not a valid listen address: %w", c.HTTP.Addr, err)
		}
		if nonLoopback {
			return fmt.Errorf(
				"config: refusing to start — http.addr %q is not loopback and http.auth.enabled is false, "+
					"which would serve the entire library unauthenticated; "+
					"either set http.auth.enabled true or bind 127.0.0.1", c.HTTP.Addr)
		}
	}

	// Providers are validated by the registry, which owns the rules — an
	// endpoint that is not a URL, a required credential that is missing, a
	// capability that does not exist. They are startup errors for ADR-0023's
	// reason applied to configuration: somebody named this service, and
	// silently using none is worse than not starting.
	//
	// Reachability is deliberately NOT checked here. That inverts the
	// asymmetry and it is the whole of ADR-0025: a download client down at
	// 03:00 must not stop Heyarr serving the library at 03:01.
	if _, err := providers.Validate(c.Providers); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	for i, lib := range c.Libraries {
		if lib.Name == "" {
			return fmt.Errorf("config: libraries[%d] has no name", i)
		}
		// A library's content type steers identification, so a library without
		// one would silently identify nothing (M1-11). It is checked here
		// rather than defaulted, because guessing wrong is worse than asking.
		if lib.ContentType == "" {
			return fmt.Errorf("config: library %q has no content_type", lib.Name)
		}
		if len(lib.Roots) == 0 {
			return fmt.Errorf("config: library %q has no roots", lib.Name)
		}
		for _, r := range lib.Roots {
			if !filepath.IsAbs(r) {
				return fmt.Errorf("config: library %q root %q must be an absolute path", lib.Name, r)
			}
		}
	}
	return nil
}

// bindsNonLoopback reports whether addr would listen on anything other than a
// loopback address. The wildcard forms — an empty host, 0.0.0.0, :: — all count
// as non-loopback, which is the case this exists to catch.
func bindsNonLoopback(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, err
	}
	if host == "" {
		return true, nil // ":7777" listens on every interface
	}
	if host == "localhost" {
		return false, nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A hostname we cannot resolve at config time. Refuse to guess: treat
		// it as non-loopback so the safe branch is the default.
		return true, nil
	}
	return !ip.IsLoopback(), nil
}

// EnsureDataDir creates the data directory and verifies it is writable. It is
// called at startup so that a permission problem surfaces immediately, naming
// the path, rather than on the first write halfway through a scan.
func (c Config) EnsureDataDir() error {
	if err := os.MkdirAll(c.DataDir, 0o750); err != nil {
		return fmt.Errorf("config: cannot create data_dir %s: %w", c.DataDir, err)
	}
	// The probe must be unique per caller. ADR-0002 has the controller, worker
	// and peer running as separate processes against one data_dir, so they call
	// this concurrently at startup — and with a fixed name one process deletes
	// the probe another just created, failing a start for no reason. CreateTemp
	// gives each caller its own file.
	f, err := os.CreateTemp(c.DataDir, ".heyarr-write-probe-*")
	if err != nil {
		return fmt.Errorf("config: data_dir %s is not writable: %w", c.DataDir, err)
	}
	probe := f.Name()
	if err := f.Close(); err != nil {
		return fmt.Errorf("config: data_dir %s is not writable: %w", c.DataDir, err)
	}
	if err := os.Remove(probe); err != nil {
		return fmt.Errorf("config: cannot clean up in data_dir %s: %w", c.DataDir, err)
	}
	return nil
}

// canonicalKeys indexes every configuration key by its separator-free form, so
// an environment variable can be matched without the caller having to know
// whether a given boundary is a dot or an underscore.
func canonicalKeys(keys []string) map[string]string {
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		out[squashSeparators(key)] = key
	}
	return out
}

func squashSeparators(s string) string {
	return strings.NewReplacer(".", "", "_", "").Replace(s)
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
