package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "heyarr.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaultsAreSafe(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The defaults are the configuration a careless operator gets. They must be
	// safe, not convenient.
	if !cfg.HTTP.Auth.Enabled {
		t.Error("authentication is off by default")
	}
	if !strings.HasPrefix(cfg.HTTP.Addr, "127.0.0.1:") {
		t.Errorf("default http.addr = %q, want loopback", cfg.HTTP.Addr)
	}
}

func TestDerivedPathsFollowDataDir(t *testing.T) {
	cfg, err := Load(writeConfig(t, "data_dir: /srv/heyarr\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for name, got := range map[string]string{
		"cas.root":         cfg.CAS.Root,
		"database.path":    cfg.Database.Path,
		"http.unix_socket": cfg.HTTP.UnixSocket,
	} {
		if !strings.HasPrefix(got, "/srv/heyarr/") {
			t.Errorf("%s = %q, want it to follow data_dir", name, got)
		}
	}
}

func TestExplicitPathBeatsDerivedDefault(t *testing.T) {
	cfg, err := Load(writeConfig(t, "data_dir: /srv/heyarr\ncas:\n  root: /mnt/big/cas\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CAS.Root != "/mnt/big/cas" {
		t.Errorf("cas.root = %q, want the explicit value to win", cfg.CAS.Root)
	}
	if cfg.Database.Path != "/srv/heyarr/heyarr.db" {
		t.Errorf("database.path = %q, want it still derived", cfg.Database.Path)
	}
}

func TestEnvironmentOverridesFile(t *testing.T) {
	t.Setenv("HEYARR_LOG_LEVEL", "debug")
	t.Setenv("HEYARR_PEER_NAME", "cove")
	cfg, err := Load(writeConfig(t, "data_dir: /srv/heyarr\nlog:\n  level: info\npeer:\n  name: bartley\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level = %q, want the environment to win over the file", cfg.Log.Level)
	}
	if cfg.Peer.Name != "cove" {
		t.Errorf("peer.name = %q, want the environment to win over the file", cfg.Peer.Name)
	}
}

func TestMissingConfigFileNamesThePath(t *testing.T) {
	_, err := Load("/nonexistent/heyarr.yaml")
	if err == nil {
		t.Fatal("Load succeeded for a missing config file")
	}
	// The usual cause is a typo or a relative path resolved from an unexpected
	// working directory, so the path has to appear in the message.
	if !strings.Contains(err.Error(), "/nonexistent/heyarr.yaml") {
		t.Errorf("error = %q, want it to name the path", err)
	}
}

// The rule from ADR-0011: this server range-serves the whole library, and
// Milestone 1 has no identity model, so an unauthenticated non-loopback
// listener is refused rather than warned about.
func TestRefusesUnauthenticatedNonLoopbackBind(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:7777", ":7777", "192.168.16.224:7777", "[::]:7777", "heyarr.example.com:7777"} {
		t.Run(addr, func(t *testing.T) {
			cfg := Defaults()
			cfg.HTTP.Addr = addr
			cfg.HTTP.Auth.Enabled = false
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted unauthenticated bind on %q", addr)
			}
			if !strings.Contains(err.Error(), "refusing to start") {
				t.Errorf("error = %q, want it to say it is refusing", err)
			}
		})
	}
}

func TestAllowsUnauthenticatedLoopbackBind(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:7777", "localhost:7777", "[::1]:7777"} {
		t.Run(addr, func(t *testing.T) {
			cfg := Defaults()
			cfg.HTTP.Addr = addr
			cfg.HTTP.Auth.Enabled = false
			if err := cfg.Validate(); err != nil {
				t.Errorf("Validate rejected loopback %q: %v", addr, err)
			}
		})
	}
}

func TestAllowsAuthenticatedNonLoopbackBind(t *testing.T) {
	cfg := Defaults()
	cfg.HTTP.Addr = "0.0.0.0:7777"
	cfg.HTTP.Auth.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate rejected an authenticated public bind: %v", err)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"empty data_dir", func(c *Config) { c.DataDir = "" }, "data_dir must be set"},
		{"relative data_dir", func(c *Config) { c.DataDir = "heyarr" }, "absolute path"},
		{"bad log level", func(c *Config) { c.Log.Level = "verbose" }, "log.level"},
		{"bad log format", func(c *Config) { c.Log.Format = "xml" }, "log.format"},
		{"no peer name", func(c *Config) { c.Peer.Name = "" }, "peer.name"},
		{"library without name", func(c *Config) {
			c.Libraries = []Library{{Roots: []string{"/srv/media"}}}
		}, "has no name"},
		{"library without roots", func(c *Config) {
			c.Libraries = []Library{{Name: "films"}}
		}, "has no roots"},
		{"relative library root", func(c *Config) {
			c.Libraries = []Library{{Name: "films", Roots: []string{"media"}}}
		}, "must be an absolute path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			tt.mut(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate accepted it")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestEnsureDataDirCreatesAndProbes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "heyarr")
	cfg := Defaults()
	cfg.DataDir = dir
	if err := cfg.EnsureDataDir(); err != nil {
		t.Fatalf("EnsureDataDir: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("data_dir was not created: %v", err)
	}
	// The probe must not be left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("EnsureDataDir left %d entries behind, want a clean directory", len(entries))
	}
}

// The acceptance criterion for M1-02: a permission problem must surface at
// startup, naming the path, not on the first write halfway through a scan.
func TestEnsureDataDirFailsOnUnwritablePathNamingIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can write anywhere")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil { // r-x: cannot create children
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	target := filepath.Join(parent, "heyarr")
	cfg := Defaults()
	cfg.DataDir = target
	err := cfg.EnsureDataDir()
	if err == nil {
		t.Fatal("EnsureDataDir succeeded on an unwritable parent")
	}
	if !strings.Contains(err.Error(), target) {
		t.Errorf("error = %q, want it to name %q", err, target)
	}
}

// Regression: `data_dir` and `log.level` use different separators, so any
// single separator rule gets one of them wrong. HEYARR_DATA__DIR mapped to
// `data.dir`, matched nothing, and silently left the default in place — the
// process started against /var/lib/heyarr while the operator believed they had
// pointed it elsewhere. Silent is the problem; a wrong value that errors is
// recoverable, one that is ignored is not.
func TestEnvironmentKeysResolveRegardlessOfSeparator(t *testing.T) {
	tests := []struct {
		env  string
		want string
	}{
		{"HEYARR_DATA_DIR", "/srv/one"},
		{"HEYARR_DATA__DIR", "/srv/two"},
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			t.Setenv(tt.env, tt.want)
			cfg, err := Load("")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.DataDir != tt.want {
				t.Errorf("%s did not take effect: data_dir = %q, want %q", tt.env, cfg.DataDir, tt.want)
			}
		})
	}
}

func TestEnvironmentReachesNestedKeys(t *testing.T) {
	t.Setenv("HEYARR_HTTP_AUTH_ENABLED", "false")
	t.Setenv("HEYARR_HTTP_ADDR", "127.0.0.1:9999")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Auth.Enabled {
		t.Error("HEYARR_HTTP_AUTH_ENABLED did not reach http.auth.enabled")
	}
	if cfg.HTTP.Addr != "127.0.0.1:9999" {
		t.Errorf("http.addr = %q, want the environment value", cfg.HTTP.Addr)
	}
}

// An unknown environment key must be dropped, not turned into a config entry.
func TestUnknownEnvironmentKeysAreIgnored(t *testing.T) {
	t.Setenv("HEYARR_NOT_A_REAL_KEY", "x")
	if _, err := Load(""); err != nil {
		t.Fatalf("an unknown HEYARR_ variable broke Load: %v", err)
	}
}

// The separator-free index only works if no two keys collide once separators
// are removed. If a future key breaks that, this test says so before the
// ambiguity ships.
func TestCanonicalKeysDoNotCollide(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	_ = cfg
	seen := map[string]string{}
	for _, key := range []string{
		"data_dir", "cas.root", "database.path", "http.addr", "http.unix_socket",
		"http.auth.enabled", "peer.name", "peer.site", "log.level", "log.format",
	} {
		sq := squashSeparators(key)
		if prev, dup := seen[sq]; dup {
			t.Errorf("keys %q and %q collide as %q — environment mapping would be ambiguous", prev, key, sq)
		}
		seen[sq] = key
	}
}

// Regression: the write probe used a fixed filename, so the three roles
// starting as separate processes against one data_dir — the topology ADR-0002
// mandates — raced, and one process removed the probe another had just
// created. The loser failed to start with "cannot clean up in data_dir".
//
// It passed locally and failed on CI, because locally the starts were staggered
// enough to miss each other. That is the shape of bug this test exists to stop.
func TestEnsureDataDirIsSafeUnderConcurrentStart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	cfg := Defaults()
	cfg.DataDir = dir

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release them together, so they actually collide
			errs[i] = cfg.EnsureDataDir()
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent start %d failed: %v", i, err)
		}
	}
	// And no probe files may be left behind by any of them.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("concurrent starts left %d files behind: %v", len(entries), entries)
	}
}
