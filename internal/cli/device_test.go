package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

// deviceDir is a config-directory-shaped temporary directory. It is NEVER a
// data directory: that distinction is the point of half the tests below.
func deviceDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "config", "heyarr", "device")
}

var (
	publicKeyPattern = regexp.MustCompile(`ed25519:[0-9a-f]{64}`)
	keyPathPattern   = regexp.MustCompile(`"key_path": "[^"]*"`)
)

// normaliseDevice hides what legitimately differs per run, so the golden file
// asserts the SHAPE — the field names, their nesting, and the presence of the
// caveat — which is what a script depends on and a refactor breaks silently.
func normaliseDevice(s string) string {
	s = uuidPattern.ReplaceAllString(s, "<uuid>")
	s = timestampPattern.ReplaceAllString(s, "<timestamp>")
	s = publicKeyPattern.ReplaceAllString(s, "ed25519:<hex>")
	s = keyPathPattern.ReplaceAllString(s, `"key_path": "<config-dir>/device_ed25519.key"`)
	return s
}

func generateDevice(t *testing.T, dir string) string {
	t.Helper()
	out, _, err := run(t, context.Background(), "device", "generate",
		"--device-dir", dir, "--name", "laptop", "--json")
	if err != nil {
		t.Fatalf("device generate: %v", err)
	}
	var view device.View
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("device generate --json is not valid JSON: %v\n%s", err, out)
	}
	return view.ID
}

func TestDeviceGenerateJSONShape(t *testing.T) {
	out, _, err := run(t, context.Background(), "device", "generate",
		"--device-dir", deviceDir(t), "--name", "laptop", "--json")
	if err != nil {
		t.Fatalf("device generate: %v", err)
	}
	testutil.Golden(t, "testdata/device_generate.json", []byte(normaliseDevice(out)))
}

func TestDeviceListJSONShape(t *testing.T) {
	dir := deviceDir(t)
	generateDevice(t, dir)
	out, _, err := run(t, context.Background(), "device", "list", "--device-dir", dir, "--json")
	if err != nil {
		t.Fatalf("device list: %v", err)
	}
	testutil.Golden(t, "testdata/device_list.json", []byte(normaliseDevice(out)))
}

func TestDeviceShowJSONShape(t *testing.T) {
	dir := deviceDir(t)
	id := generateDevice(t, dir)
	out, _, err := run(t, context.Background(), "device", "show", id, "--device-dir", dir, "--json")
	if err != nil {
		t.Fatalf("device show: %v", err)
	}
	testutil.Golden(t, "testdata/device_show.json", []byte(normaliseDevice(out)))
}

func TestDeviceRemoveJSONShape(t *testing.T) {
	dir := deviceDir(t)
	id := generateDevice(t, dir)
	out, _, err := run(t, context.Background(), "device", "remove", id, "--device-dir", dir, "--json")
	if err != nil {
		t.Fatalf("device remove: %v", err)
	}
	testutil.Golden(t, "testdata/device_remove.json", []byte(normaliseDevice(out)))
}

// TestDeviceGenerateRoundTripsThroughList asserts the public key, and that a
// second list is byte-identical to the first.
func TestDeviceGenerateRoundTripsThroughList(t *testing.T) {
	dir := deviceDir(t)
	ctx := context.Background()

	generated, _, err := run(t, ctx, "device", "generate", "--device-dir", dir, "--name", "laptop", "--json")
	if err != nil {
		t.Fatalf("device generate: %v", err)
	}
	var created device.View
	if err := json.Unmarshal([]byte(generated), &created); err != nil {
		t.Fatal(err)
	}
	if !publicKeyPattern.MatchString(created.PublicKey) {
		t.Fatalf("public key %q is not ed25519:<64 lowercase hex>", created.PublicKey)
	}

	first, _, err := run(t, ctx, "device", "list", "--device-dir", dir, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var listed []device.View
	if err := json.Unmarshal([]byte(first), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("device list reported %d devices, want 1", len(listed))
	}
	if listed[0].PublicKey != created.PublicKey {
		t.Errorf("device list reported %s, device generate reported %s", listed[0].PublicKey, created.PublicKey)
	}
	if listed[0].ID != created.ID {
		t.Errorf("device list reported id %s, device generate reported %s", listed[0].ID, created.ID)
	}

	second, _, err := run(t, ctx, "device", "list", "--device-dir", dir, "--json")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Errorf("two listings of one unchanged device differ:\n--- first ---\n%s\n--- second ---\n%s",
			first, second)
	}
}

func TestDeviceListIsEmptyRatherThanNull(t *testing.T) {
	out, _, err := run(t, context.Background(), "device", "list", "--device-dir", deviceDir(t), "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("an empty listing produced %q, want []", strings.TrimSpace(out))
	}
}

// TestTheKeyLivesInTheConfigDirectoryAndNeverTheDataDir.
//
// The order matters: the key file is asserted to EXIST where it should before
// the server directory is asserted untouched. The other order passes trivially
// on a command that wrote nothing at all.
func TestTheKeyLivesInTheConfigDirectoryAndNeverTheDataDir(t *testing.T) {
	dir := deviceDir(t)
	dataDir := t.TempDir()

	// A data directory with something in it, so "untouched" is a real claim.
	cfgPath := filepath.Join(dataDir, "heyarr.yaml")
	if err := os.WriteFile(cfgPath, []byte("data_dir: "+dataDir+"\npeer:\n  name: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, context.Background(), "--config", cfgPath, "token", "create", "svc"); err != nil {
		t.Fatalf("preparing a server data directory: %v", err)
	}
	before := snapshot(t, dataDir)
	if len(before) < 2 {
		t.Fatalf("the data directory holds %d files, so 'untouched' would prove little", len(before))
	}

	// And the environment pointed at it, so an implementation that consulted
	// the server's configuration would be caught rather than merely unlikely.
	t.Setenv("HEYARR_DATA_DIR", dataDir)

	if _, _, err := run(t, context.Background(), "device", "generate",
		"--device-dir", dir, "--name", "laptop"); err != nil {
		t.Fatalf("device generate: %v", err)
	}

	// First: the key is where it belongs, and owner-only.
	keyPath := filepath.Join(dir, device.KeyFileName)
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("the private key is not in the config directory: %v", err)
	}
	if got, want := info.Mode().Perm(), fs.FileMode(0o600); got != want {
		t.Errorf("%s is mode %#o, want %#o", keyPath, got, want)
	}

	// Only then: the server directory is exactly as it was.
	after := snapshot(t, dataDir)
	if !equalSnapshots(before, after) {
		t.Errorf("the server data directory changed:\n--- before ---\n%v\n--- after ---\n%v", before, after)
	}
	for _, entry := range after {
		if strings.Contains(entry, device.KeyFileName) {
			t.Errorf("a device key file appeared in the server's data directory: %s", entry)
		}
	}
}

// snapshot records name, size and mode for every file under root. Modification
// times are deliberately excluded: a file rewritten with identical content is
// still a file this command must not have touched, and a test that failed on a
// timestamp alone would be flaky rather than strict.
func snapshot(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, rel+" "+itoa(info.Size())+" "+info.Mode().Perm().String())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func equalSnapshots(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// keyMaterial returns every spelling of the private key currently on disk, or
// nothing if there is no key there right now.
//
// It is called after EVERY invocation rather than once at the start, because
// `device generate --force` replaces the key: a scan whose needles came only
// from the first key would look for a secret the command under test no longer
// has, and would pass while printing the new one in full. That is exactly how
// this test passed its own sabotage the first time it was written.
func keyMaterial(t *testing.T, dir string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, device.KeyFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	fileText := strings.TrimSpace(string(raw))
	seedHex := fileText[strings.IndexByte(fileText, ':')+1:]
	seed, err := hex.DecodeString(seedHex)
	if err != nil {
		t.Fatalf("the key file is not the shape this test assumes: %v", err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return map[string]string{
		"the seed in hex":        seedHex,
		"the key file verbatim":  fileText,
		"the private key in hex": hex.EncodeToString(priv),
		"the seed as raw bytes":  string(seed),
	}
}

// TestNoDeviceCommandEverPrintsThePrivateKey scans everything the commands
// wrote for the key material, rather than reading the code and concluding it
// looks fine.
func TestNoDeviceCommandEverPrintsThePrivateKey(t *testing.T) {
	dir := deviceDir(t)
	ctx := context.Background()
	id := generateDevice(t, dir)

	needles := map[string]string{}
	collect := func() {
		for what, needle := range keyMaterial(t, dir) {
			if needle == "" {
				t.Fatalf("the %s needle is empty, so this assertion would prove nothing", what)
			}
			needles[what+" "+needle[:8]] = needle
		}
	}
	collect()

	var transcript strings.Builder
	invocations := [][]string{
		{"device", "list", "--device-dir", dir},
		{"device", "list", "--device-dir", dir, "--json"},
		{"device", "show", "--device-dir", dir},
		{"device", "show", id, "--device-dir", dir, "--json"},
		{"device", "generate", "--device-dir", dir, "--name", "again"},
		{"device", "generate", "--device-dir", dir, "--name", "again", "--force", "--json"},
		{"device", "remove", id, "--device-dir", dir},
	}
	for _, args := range invocations {
		out, errOut, err := run(t, ctx, args...)
		transcript.WriteString(out)
		transcript.WriteString(errOut)
		if err != nil {
			transcript.WriteString(err.Error())
		}
		// After, not only before: a command that REPLACED the key must be
		// scanned for the key it left behind.
		collect()
	}
	// And the MCP door, over its real transport.
	transcript.WriteString(runMCP(t, dir,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"device_list","arguments":{}}}`,
	))
	collect()

	captured := transcript.String()
	if strings.TrimSpace(captured) == "" {
		t.Fatal("nothing was captured, so this scan proves nothing")
	}
	if len(needles) < 4 {
		t.Fatalf("only %d needles were collected, so this scan proves little", len(needles))
	}
	for what, needle := range needles {
		if strings.Contains(captured, needle) {
			t.Errorf("%s appears in command output:\n%s", what, captured)
		}
	}
	// The captured output must contain SOMETHING about the key, or the scan
	// above passed because the commands printed nothing interesting.
	if !publicKeyPattern.MatchString(captured) {
		t.Fatalf("no public key appears in the captured output, so the scan proves nothing:\n%s", captured)
	}
}

// TestDeviceRefusals — one case each. A single "invalid input is rejected"
// would pass with three of these broken.
func TestDeviceRefusals(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(t *testing.T, dir string) []string
		want    error
	}{
		{
			name: "a world-readable key file",
			arrange: func(t *testing.T, dir string) []string {
				t.Helper()
				generateDevice(t, dir)
				if err := os.Chmod(filepath.Join(dir, device.KeyFileName), 0o644); err != nil {
					t.Fatal(err)
				}
				return []string{"device", "list", "--device-dir", dir}
			},
			want: device.ErrKeyPermissions,
		},
		{
			name: "a key file that is not a key",
			arrange: func(t *testing.T, dir string) []string {
				t.Helper()
				generateDevice(t, dir)
				if err := os.WriteFile(filepath.Join(dir, device.KeyFileName),
					[]byte("ssh-ed25519 AAAA\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return []string{"device", "show", "--device-dir", dir}
			},
			want: device.ErrMalformedKey,
		},
		{
			name: "removing a device that does not exist",
			arrange: func(t *testing.T, dir string) []string {
				t.Helper()
				generateDevice(t, dir)
				return []string{
					"device", "remove", "01920000-0000-7000-8000-000000000000",
					"--device-dir", dir,
				}
			},
			want: device.ErrUnknownDevice,
		},
		{
			name: "regenerating without --force",
			arrange: func(t *testing.T, dir string) []string {
				t.Helper()
				generateDevice(t, dir)
				return []string{"device", "generate", "--device-dir", dir, "--name", "laptop"}
			},
			want: device.ErrDeviceExists,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := deviceDir(t)
			args := tt.arrange(t, dir)
			_, _, err := run(t, context.Background(), args...)
			if err == nil {
				t.Fatalf("the command succeeded, want %v", tt.want)
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestDeviceHumanOutputSaysTheKeyAuthorisesNothing.
//
// The caveat is a deliverable, not a comment: the CLI is where somebody
// actually reads it.
func TestDeviceHumanOutputSaysTheKeyAuthorisesNothing(t *testing.T) {
	dir := deviceDir(t)
	generateDevice(t, dir)
	for _, args := range [][]string{
		{"device", "list", "--device-dir", dir},
		{"device", "show", "--device-dir", dir},
	} {
		out, _, err := run(t, context.Background(), args...)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"unproven", "not_enrolled", device.NotYetAuthorising} {
			if !strings.Contains(out, want) {
				t.Errorf("`heyarr %s` does not say %q:\n%s", strings.Join(args, " "), want, out)
			}
		}
	}
}

// runMCP drives `heyarr device mcp` over its real stdio transport.
func runMCP(t *testing.T, dir string, requests ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := NewRootCommand(Options{Stdout: &out, Stderr: &errOut, ShutdownGrace: 2 * time.Second})
	cmd.SetArgs([]string{"device", "mcp", "--device-dir", dir})
	cmd.SetIn(strings.NewReader(strings.Join(requests, "\n") + "\n"))
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("device mcp: %v", err)
	}
	return out.String() + errOut.String()
}

// TestDeviceMCPIsLocalAndPublishesExactlyItsTools.
func TestDeviceMCPIsLocalAndPublishesExactlyItsTools(t *testing.T) {
	dir := deviceDir(t)
	generateDevice(t, dir)

	var out, errOut bytes.Buffer
	cmd := NewRootCommand(Options{Stdout: &out, Stderr: &errOut, ShutdownGrace: 2 * time.Second})
	cmd.SetArgs([]string{"device", "mcp", "--device-dir", dir})
	cmd.SetIn(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"))
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("device mcp: %v", err)
	}

	var reply struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &reply); err != nil {
		t.Fatalf("stdout is not one JSON-RPC message: %v\n%s", err, out.String())
	}
	var names []string
	for _, tool := range reply.Result.Tools {
		names = append(names, tool.Name)
	}
	want := []string{"device_generate", "device_list", "device_remove", "device_show"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("the personal MCP publishes %v, want exactly %v", names, want)
	}
	// Diagnostics go to stderr. On this transport a stray line of prose on
	// stdout is a protocol error.
	if !strings.Contains(errOut.String(), "personal mcp") {
		t.Errorf("nothing was reported on stderr:\n%s", errOut.String())
	}
}
