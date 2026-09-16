package tpm

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport/tcp"
)

// TestSealUnsealAgainstSwtpm drives the seal/unseal core against a real TPM 2.0
// provided by swtpm (an external binary, pure-Go transport — no cgo). It is the
// CI proof of the round-trip; it SKIPS when swtpm is not installed, so the
// default matrix stays hardware-free. CI installs swtpm on the Linux leg.
func TestSealUnsealAgainstSwtpm(t *testing.T) {
	if _, err := exec.LookPath("swtpm"); err != nil {
		t.Skip("swtpm not installed; skipping the live TPM round-trip")
	}
	cmdPort, platPort := freePort(t), freePort(t)
	dir := t.TempDir()

	sw := exec.Command("swtpm", "socket", "--tpm2",
		"--server", fmt.Sprintf("type=tcp,port=%d,bindaddr=127.0.0.1", cmdPort),
		"--ctrl", fmt.Sprintf("type=tcp,port=%d,bindaddr=127.0.0.1", platPort),
		"--tpmstate", "dir="+dir,
		"--flags", "not-need-init",
	)
	sw.Stderr = os.Stderr
	if err := sw.Start(); err != nil {
		t.Fatalf("starting swtpm: %v", err)
	}
	t.Cleanup(func() {
		_ = sw.Process.Kill()
		_ = sw.Wait()
	})
	waitPort(t, cmdPort)
	waitPort(t, platPort)

	tpm, err := tcp.Open(tcp.Config{
		CommandAddress:  fmt.Sprintf("127.0.0.1:%d", cmdPort),
		PlatformAddress: fmt.Sprintf("127.0.0.1:%d", platPort),
	})
	if err != nil {
		t.Fatalf("tcp.Open(swtpm): %v", err)
	}
	defer func() { _ = tpm.Close() }()
	if err := tpm.PowerOff(); err != nil {
		t.Fatalf("PowerOff: %v", err)
	}
	if err := tpm.PowerOn(); err != nil {
		t.Fatalf("PowerOn: %v", err)
	}
	if _, err := (tpm2.Startup{StartupType: tpm2.TPMSUClear}).Execute(tpm); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	sealUnsealCanary(t, tpm)
}

// freePort reserves an ephemeral TCP port and returns it (the listener is closed,
// so swtpm can bind it — a small race, acceptable in a test).
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// waitPort blocks until something accepts on the port, or fails the test.
func waitPort(t *testing.T, port int) {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for i := 0; i < 100; i++ {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("swtpm never accepted on %s", addr)
}
