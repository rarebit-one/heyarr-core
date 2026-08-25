// Package config loads layered configuration: file, then HEYARR_ environment,
// then flags.
package config

import (
	"errors"
	"fmt"
	"net"
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
}

// Auth configures API authentication.
type Auth struct {
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
}

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
		Media:    Media{},
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
