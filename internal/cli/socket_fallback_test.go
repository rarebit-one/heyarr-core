package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/config"
)

// The client half of #170.
//
// The server declines a unix socket whose path is longer than this platform's
// sockaddr_un can hold, says so, and serves over TCP instead. The CLI used to
// dial the socket anyway and report "is heyarr running, and is this the right
// data directory?" — both of which were fine.
//
// The failing case only appears on a LONG path, which is nobody's development
// machine, so the long directory is constructed deliberately here rather than
// hoped for. And the short-path case is asserted just as hard, because a fix
// that simply always used TCP would pass the first test and quietly throw the
// local transport away.

// longDataDir builds a data directory whose implied socket path is over the
// limit. The length comes from the server package, because the number differs
// by platform — 104 on darwin and the BSDs, 108 on Linux, 256 on Windows — and
// a literal here would test one of them and lie about the rest.
func longDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for len(filepath.Join(dir, "heyarr.sock")) < httpapi.MaxUnixSocketPath() {
		dir = filepath.Join(dir, "a-directory-with-a-long-enough-name")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	return dir
}

// shortDataDir builds one comfortably under the limit. t.TempDir() is not used:
// on macOS it is already ~70 bytes deep, and this test is worth nothing if the
// socket it expects to be bound was declined for length.
func shortDataDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if len(filepath.Join(dir, "heyarr.sock")) >= httpapi.MaxUnixSocketPath() {
		t.Fatalf("%s is already too long for this platform, so this test would assert nothing", dir)
	}
	return dir
}

// TestTheCLIFallsBackToTCPWhenTheServerDeclinedTheSocket is the reported bug.
func TestTheCLIFallsBackToTCPWhenTheServerDeclinedTheSocket(t *testing.T) {
	h := newAPIHarness(t, withDataDir(longDataDir(t)), withRealListeners).seed()

	// The precondition is half the test: without it this passes on any machine
	// whose temp directory happens to be short.
	if got := h.httpServer.SocketPath(); got != "" {
		t.Fatalf("the server bound %q, so the failure this test exists for is not being reproduced", got)
	}
	if h.httpServer.Addr() == "" {
		t.Fatal("the server bound no TCP listener either, so there is nothing to fall back to")
	}

	out := h.mustRun("peers", "list")
	if !strings.Contains(out, "peer-a") {
		t.Errorf("`peers list` did not reach the API over TCP:\n%s", out)
	}
}

// TestTheCLIStillUsesTheSocketOnAShortPath is the other half: the fix must be a
// fallback, not a migration to TCP.
//
// It proves the transport rather than asserting it, by recording an http.addr
// in the configuration that nothing answers on — port 9 is discard, reserved
// and refusing connections everywhere. The command can only succeed over the
// socket.
func TestTheCLIStillUsesTheSocketOnAShortPath(t *testing.T) {
	h := newAPIHarness(t, withDataDir(shortDataDir(t)), withRealListeners,
		withConfiguredAddr("127.0.0.1:9")).seed()

	if h.httpServer.SocketPath() == "" {
		t.Fatal("the server declined the socket on a short path, so this test cannot assert it is used")
	}

	out := h.mustRun("peers", "list")
	if !strings.Contains(out, "peer-a") {
		t.Errorf("`peers list` did not reach the API over the unix socket:\n%s", out)
	}
}

// TestAPIAddrResolution covers the decision itself, one case per reason.
func TestAPIAddrResolution(t *testing.T) {
	short := shortDataDir(t)
	present := filepath.Join(short, "heyarr.sock")
	if err := os.WriteFile(present, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(short, "not-there.sock")
	overLong := filepath.Join(short, strings.Repeat("x", httpapi.MaxUnixSocketPath()), "heyarr.sock")

	cases := []struct {
		name    string
		flag    string
		socket  string
		tcp     string
		want    string
		wantErr []string
	}{
		{name: "--addr always wins", flag: "unix:///given.sock", socket: present, tcp: "127.0.0.1:7777", want: "unix:///given.sock"},
		{name: "a bound socket is the default transport", socket: present, tcp: "127.0.0.1:7777", want: ""},
		{name: "a socket the server cannot have bound falls back", socket: overLong, tcp: "127.0.0.1:7777", want: "127.0.0.1:7777"},
		{name: "a socket that is not there falls back", socket: absent, tcp: "127.0.0.1:7777", want: "127.0.0.1:7777"},
		{name: "a wildcard bind is reachable at loopback", socket: absent, tcp: "0.0.0.0:7777", want: "127.0.0.1:7777"},
		{
			name: "an over-long socket and nowhere else to go says so",
			// The message must name the real possibility rather than asking
			// whether heyarr is running.
			socket: overLong, tcp: "",
			wantErr: []string{overLong, "limit on this platform", "http.addr"},
		},
		{name: "no socket and no address is still the client package's error to give", socket: "", tcp: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.HTTP.UnixSocket = tc.socket
			cfg.HTTP.Addr = tc.tcp
			f := &clientFlags{addr: tc.flag}
			got, err := f.apiAddr(cfg)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("apiAddr = %q, want a refusal", got)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the refusal does not mention %q: %v", want, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("apiAddr: %v", err)
			}
			if got != tc.want {
				t.Errorf("apiAddr = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTheSocketLimitIsTheServersOwn: the client must not carry its own copy of
// a number that differs by platform.
func TestTheSocketLimitIsTheServersOwn(t *testing.T) {
	limit := httpapi.MaxUnixSocketPath()
	if limit <= 0 {
		t.Fatalf("the server reports a socket limit of %d", limit)
	}
	atLimit := "/" + strings.Repeat("x", limit-1)
	justUnder := "/" + strings.Repeat("x", limit-2)
	if !socketTooLong(atLimit) {
		t.Errorf("a path of exactly %d bytes is not treated as too long, and sun_path includes the NUL", limit)
	}
	if socketTooLong(justUnder) {
		t.Errorf("a path of %d bytes is treated as too long", limit-1)
	}
}
