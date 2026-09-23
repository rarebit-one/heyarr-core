package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/rarebit-one/voidbind-go/device"

	"github.com/rarebit-one/heyarr-core/internal/api/weblogin"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client/cruciform"
)

// buildCruciformWake constructs the WakeFunc the cruciform-offload transport calls
// to wake the paired phone for an unwrap over the away-path (relay + push): it
// POSTs to the node's /v1/unwrap-wake, which fans an opaque voidbind:unwrap ping to
// this user's subscribed devices (ADR-0098).
//
// It authenticates with THIS device's enrolment cert and membership ops — Option A:
// the offload desktop is still an enrolled device with a signing identity, even
// though it offloads its encryption custody. A device that is not enrolled (no
// cert) yields a nil WakeFunc, so the offload still opens when the phone is already
// reachable (the LAN-direct path, or a phone already polling the relay) — waking is
// the away-path fallback, not a hard requirement. Reaching the config or device
// store is an error; being un-enrolled is not.
func buildCruciformWake(cfg config.Config, deviceDir string) (cruciform.WakeFunc, error) {
	base, hc, err := nodeWakeClient(cfg)
	if err != nil {
		return nil, err
	}
	ds, err := device.NewStore(device.StoreOptions{Dir: deviceDir})
	if err != nil {
		return nil, err
	}
	dev, err := ds.Get("")
	if err != nil {
		// No device key on this machine yet — nothing to authenticate a wake with.
		return nil, nil //nolint:nilnil // "no wake available" is a valid, non-error outcome
	}
	cert, ok := dev.EnrolmentCert()
	if !ok {
		// Enrolled-but-no-cert cannot happen; a bare device (never paired) has none,
		// and then the away-path wake is simply unavailable until enrolment.
		return nil, nil //nolint:nilnil // see above
	}
	ops, err := ds.Ops()
	if err != nil {
		return nil, err
	}
	return newCruciformWake(base, hc, cert, ops), nil
}

// newCruciformWake returns the WakeFunc that POSTs an unwrap-wake to the node. The
// cert and ops authenticate the request; relay_base and session (the transport's
// per-unwrap relay session) tell the phone which session to open.
func newCruciformWake(base string, hc *http.Client, cert string, ops []string) cruciform.WakeFunc {
	return func(ctx context.Context, relayBase, session string) error {
		body, err := json.Marshal(struct {
			Cert      string   `json:"cert"`
			Ops       []string `json:"ops,omitempty"`
			RelayBase string   `json:"relay_base"`
			Session   string   `json:"session"`
		}{Cert: cert, Ops: ops, RelayBase: relayBase, Session: session})
		if err != nil {
			return fmt.Errorf("cruciform wake: encoding request: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+weblogin.UnwrapWakePrefix, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return fmt.Errorf("cruciform wake: reaching the node: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode/100 != 2 {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			return fmt.Errorf("cruciform wake: node refused (%d): %s", resp.StatusCode, strings.TrimSpace(string(msg)))
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
}

// nodeWakeClient resolves the base URL and HTTP client to reach this node's public
// routes (where /v1/unwrap-wake lives), from the same config the API client reads:
// the unix socket when it is usable, else the configured TCP address. The wake
// route is public (cert-authenticated in the body), so this carries no bearer.
func nodeWakeClient(cfg config.Config) (string, *http.Client, error) {
	transport := &http.Transport{MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second}
	if usableSocket(cfg.HTTP.UnixSocket) {
		socket := cfg.HTTP.UnixSocket
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}
		return "http://node.heyarr.invalid", &http.Client{Transport: transport, Timeout: cruciform.DefaultTimeout}, nil
	}
	if addr := strings.TrimSpace(cfg.HTTP.Addr); addr != "" {
		base := addr
		if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			base = "http://" + base
		}
		return strings.TrimRight(base, "/"), &http.Client{Transport: transport, Timeout: cruciform.DefaultTimeout}, nil
	}
	return "", nil, fmt.Errorf("cruciform wake: no node address — set http.unix_socket or http.addr")
}
