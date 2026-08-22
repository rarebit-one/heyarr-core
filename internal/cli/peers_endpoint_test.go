package cli

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// `peers add` and the endpoint it accepts (#169).
//
// The reported failure was an endpoint accepted, stored and echoed back by
// `peers add`, and then refused by `peers ping` with a raw net/url error naming
// a path segment nobody typed. The value is now checked where it is written, so
// each case below asserts WHEN the refusal happens as much as that it happens:
// a peer that was not enrolled is the point, because registration is idempotent
// on the key and a typo would otherwise replace a working endpoint.

// TestPeersAddNormalisesABareHostPort: the reported input, end to end.
func TestPeersAddNormalisesABareHostPort(t *testing.T) {
	h := newAPIHarness(t).seed()
	pub, _ := fixedKeypair(t, 0x07)

	h.mustRun("peers", "add", "--name", "peer-b", "--site", "site-b",
		"--endpoint", "192.168.1.50:8443", "--public-key", identity.FormatPublicKey(pub))

	var p client.Peer
	out := h.mustRun("peers", "show", "peer-b", "--json")
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if p.Endpoint == nil || *p.Endpoint != "https://192.168.1.50:8443" {
		t.Fatalf("the stored endpoint is %v, want the normalised https:// form — "+
			"a bare host:port is what `peers ping` could not use", p.Endpoint)
	}
}

// TestPeersAddRefusesAnUnusableEndpoint enumerates the four malformed inputs
// one at a time. Each fails for a different reason, and one "invalid input is
// rejected" case would keep passing if three of them stopped being checked.
func TestPeersAddRefusesAnUnusableEndpoint(t *testing.T) {
	cases := []struct {
		name string
		in   string
		says string
	}{
		{"a scheme the inter-peer path does not speak", "http://", "http"},
		{"a port with no machine attached to it", ":8443", ":8443"},
		{"a port that is not a number", "host:notaport", "notaport"},
		{"an empty value", "", "empty"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAPIHarness(t).seed()
			pub, _ := fixedKeypair(t, byte(0x10+i))

			_, _, err := h.run("peers", "add", "--name", "peer-b", "--site", "site-b",
				"--endpoint", tc.in, "--public-key", identity.FormatPublicKey(pub))
			if err == nil {
				t.Fatalf("`peers add --endpoint %q` was accepted", tc.in)
			}
			// The flag, so the operator knows which value to retype.
			if !strings.Contains(err.Error(), "--endpoint") {
				t.Errorf("the refusal does not name the flag: %v", err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal does not mention %q: %v", tc.says, err)
			}
			// And an example, which is what the raw net/url error never had.
			if !strings.Contains(err.Error(), "https://") {
				t.Errorf("the refusal shows no example of a usable endpoint: %v", err)
			}

			// Refused at ADD time: nothing was enrolled. This is the assertion
			// that distinguishes the fix from validating on use — a peer that
			// exists here is one `peers list` would show as healthy.
			if _, _, err := h.run("peers", "show", "peer-b"); err == nil {
				t.Error("the peer was enrolled anyway, so the value was refused later rather than at add time")
			}
		})
	}
}

// TestPeersAddWithoutAnEndpointIsStillLegitimate: an omitted flag is not an
// empty value. A peer may be enrolled by its key before anyone knows where it
// will live, and `peers ping` already explains that case.
func TestPeersAddWithoutAnEndpointIsStillLegitimate(t *testing.T) {
	h := newAPIHarness(t).seed()
	pub, _ := fixedKeypair(t, 0x21)
	out := h.mustRun("peers", "add", "--name", "peer-b", "--public-key", identity.FormatPublicKey(pub))
	if !strings.Contains(out, "peer-b") {
		t.Errorf("`peers add` without --endpoint did not enrol the peer:\n%s", out)
	}
}

// TestPeersAddDoesNotMoveAWorkingEndpointToAnUnusableOne is the nastier shape
// of #169: registration is idempotent on the key, so a re-registration is an
// UPDATE, and a typo'd endpoint would silently replace a working one while the
// peer stayed enrolled and looked fine.
func TestPeersAddDoesNotMoveAWorkingEndpointToAnUnusableOne(t *testing.T) {
	h := newAPIHarness(t).seed()
	pub, _ := fixedKeypair(t, 0x22)
	key := identity.FormatPublicKey(pub)

	h.mustRun("peers", "add", "--name", "peer-b", "--endpoint", "https://b.example:8385", "--public-key", key)
	if _, _, err := h.run("peers", "add", "--name", "peer-b", "--endpoint", "b.example:notaport", "--public-key", key); err == nil {
		t.Fatal("the re-registration with a broken endpoint was accepted")
	}

	var p client.Peer
	out := h.mustRun("peers", "show", "peer-b", "--json")
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if p.Endpoint == nil || *p.Endpoint != "https://b.example:8385" {
		t.Errorf("the working endpoint was replaced: %v", p.Endpoint)
	}
}

// plantSelfIdentity puts this node's private key on disk and records its
// public half, which is what identity.Ensure does on a real first start. Ping
// refuses to present an identity without it, so a ping test that skipped this
// would never reach the endpoint at all.
func plantSelfIdentity(t *testing.T, h *apiHarness) {
	t.Helper()
	pub, priv := fixedKeypair(t, 0x11)
	seedHex := hex.EncodeToString(priv.Seed())
	if err := os.WriteFile(identity.KeyPath(h.dataDir), []byte("ed25519-seed:"+seedHex+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE peers SET public_key = ?, key_algo = 'ed25519' WHERE is_self = 1`, []byte(pub))
}

// seedLegacyPeer writes a peer row the way a build without the add-time check
// wrote one: straight into the table, with nothing between the operator and
// the column.
func seedLegacyPeer(t *testing.T, h *apiHarness, name, endpoint string, fill byte) {
	t.Helper()
	pub, _ := fixedKeypair(t, fill)
	h.exec(`INSERT INTO peers (id, name, site, mode, endpoint, public_key, key_algo, is_self, enrolled_at, created_at)
		VALUES (?, ?, 'site-b', 'full', ?, ?, 'ed25519', 0, ?, ?)`,
		"01990000-0000-7000-8000-0000000leg"+string(rune('0'+fill%10)),
		name, endpoint, []byte(pub), seedTime, seedTime)
}

// dialledBy names the commands that share the pinned dialler (#172). Both are
// exercised for every endpoint rule below, because the dialler is what was
// fixed: an endpoint one command could dial and the other could not would be
// the same bug wearing a different command's name.
var dialledBy = []struct {
	command string
	// route is what the shared connection appends to the endpoint first.
	route string
}{
	{"ping", "/peer/v1/identity"},
	{"attach", "/peer/v1/attachment"},
	// `report-inventory` reaches a controller through the same connection
	// (§19, ADR-0033), and it is here so that a command added to the dialler
	// is a command this rule covers. Its first request is the attachment.
	{"report-inventory", "/peer/v1/attachment"},
}

// TestTheSharedDiallerDialsALegacyBareHostPortAsHTTPS is the reported failure
// itself, kept as a regression.
//
// Rows written before #169 still hold bare host:port values, and what an
// operator got for one was `parse "192.168.x.x:8443/peer/v1/identity": first
// path segment in URL cannot contain colon` — a net/url message about a path
// nobody typed. The endpoint is normalised on the way out too, so the dial is
// attempted at https and the failure that remains is the honest one.
//
// Port 9 is discard: reserved, and refusing connections everywhere.
func TestTheSharedDiallerDialsALegacyBareHostPortAsHTTPS(t *testing.T) {
	for i, tc := range dialledBy {
		t.Run(tc.command, func(t *testing.T) {
			h := newAPIHarness(t).seed()
			plantSelfIdentity(t, h)
			seedLegacyPeer(t, h, "peer-legacy", "127.0.0.1:9", byte(0x30+i))

			_, _, err := h.run("peers", tc.command, "peer-legacy")
			if err == nil {
				t.Fatalf("`peers %s` reported success against a port that refuses connections", tc.command)
			}
			if strings.Contains(err.Error(), "first path segment in URL cannot contain colon") {
				t.Fatalf("the raw net/url error is still what an operator sees: %v", err)
			}
			if !strings.Contains(err.Error(), "https://127.0.0.1:9"+tc.route) {
				t.Errorf("the bare host:port was not dialled as an https origin: %v", err)
			}
		})
	}
}

// TestTheSharedDiallerRefusesAnEndpointItCannotPresentACertificateOver: a unix://
// endpoint is a legitimate peer endpoint and a legitimate probe target (§31),
// and it is not something this command can open a mutually authenticated TLS
// connection to. Saying so is better than building a nonsense URL out of it.
func TestTheSharedDiallerRefusesAnEndpointItCannotPresentACertificateOver(t *testing.T) {
	for i, tc := range dialledBy {
		t.Run(tc.command, func(t *testing.T) {
			h := newAPIHarness(t).seed()
			plantSelfIdentity(t, h)
			seedLegacyPeer(t, h, "peer-local", "unix:///tmp/heyarr-peer.sock", byte(0x40+i))

			_, _, err := h.run("peers", tc.command, "peer-local")
			if err == nil {
				t.Fatalf("`peers %s` reported success against a unix socket endpoint", tc.command)
			}
			for _, want := range []string{"peer-local", "unix:///tmp/heyarr-peer.sock", "https"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q: %v", want, err)
				}
			}
		})
	}
}

// TestTheSharedDiallerRefusesALegacyEndpointItCannotNormalise: a row holding something
// no normalisation can rescue names the field, the value and the command that
// fixes it, rather than surfacing a parse error.
func TestTheSharedDiallerRefusesALegacyEndpointItCannotNormalise(t *testing.T) {
	for i, tc := range dialledBy {
		t.Run(tc.command, func(t *testing.T) {
			h := newAPIHarness(t).seed()
			plantSelfIdentity(t, h)
			seedLegacyPeer(t, h, "peer-broken", "192.168.1.50:notaport", byte(0x50+i))

			_, _, err := h.run("peers", tc.command, "peer-broken")
			if err == nil {
				t.Fatalf("`peers %s` reported success against an endpoint it cannot dial", tc.command)
			}
			for _, want := range []string{"peer-broken", "192.168.1.50:notaport", "--endpoint"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q: %v", want, err)
				}
			}
		})
	}
}
