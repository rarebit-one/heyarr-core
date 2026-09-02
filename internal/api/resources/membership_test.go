// Every HTTP response in this file is closed by the t.Cleanup that the harness
// (or the local helper) registers, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by t.Cleanup
package resources_test

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/voidbind-go/enrolment"
	"github.com/rarebit-one/voidbind-go/identity"
	"github.com/rarebit-one/voidbind-go/rp"

	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// TestPhoneAdmitsPhoneAndTheNodeLearnsRemoves is the acceptance for ADR-0068,
// end to end through the real router. The Mac's genesis key admits phone A;
// phone A — not genesis — admits phone B; B's first contact with a node that
// has never met A succeeds because B presents A's admission beside its own;
// write scope is still keyed on the admitted device key (ADR-0065); A then
// removes B and pushes the remove to the node, after which B is refused and the
// node's view tombstones it. /enrol accepts the op-set shape and the cert-era
// shape alike.
func TestPhoneAdmitsPhoneAndTheNodeLearnsRemoves(t *testing.T) {
	h := newHarness(t, withAuth)
	admin := h.mint("admin", auth.ScopeAdmin)

	u, genesisPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	usr := u.UserID()
	newDevice := func() (string, ed25519.PrivateKey) {
		pub, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		return identity.FormatPublicKey(pub), priv
	}
	sign := func(signer ed25519.PrivateKey, kind enrolment.OpKind, dev string, prev ...string) string {
		t.Helper()
		tok, err := enrolment.SignOp(signer, usr, kind, dev, "", prev, fixedTime, 0)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	proof := func(priv ed25519.PrivateKey, over string) string {
		t.Helper()
		p, err := enrolment.SignPossession(priv, over, fixedTime, 0)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	// as sends a request under the Device scheme, optionally with the
	// Voidbind-Membership header.
	as := func(op string, priv ed25519.PrivateKey, presented []string, method, path string, body io.Reader) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, h.http.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Device "+op+"~"+proof(priv, op))
		if len(presented) > 0 {
			req.Header.Set(rp.MembershipHeader, rp.FormatMembershipHeader(presented))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	membership := func(resp *http.Response) (ops []string) {
		t.Helper()
		var out struct {
			User string   `json:"usr"`
			Ops  []string `json:"ops"`
		}
		if err := json.Unmarshal(h.body(resp), &out); err != nil {
			t.Fatal(err)
		}
		if out.User != usr {
			t.Fatalf("membership usr = %s, want %s", out.User, usr)
		}
		return out.Ops
	}

	// The admin's one act: pin the identity.
	if resp := h.do(http.MethodPost, "/api/v1/identities/users", admin.Secret,
		strings.NewReader(fmt.Sprintf(`{"public_key":%q,"name":"owner"}`, usr))); resp.StatusCode != http.StatusCreated {
		t.Fatalf("pin user: status = %d (body: %s)", resp.StatusCode, h.body(resp))
	}
	if resp := h.do(http.MethodGet, "/membership/"+usr, "", nil); resp.StatusCode != http.StatusOK || len(membership(resp)) != 0 {
		t.Fatalf("GET /membership before any op: status = %d", resp.StatusCode)
	}

	// Genesis admits A (on the Mac); A admits B (phone to phone, no secret).
	devA, privA := newDevice()
	devB, privB := newDevice()
	addA := sign(genesisPriv, enrolment.OpAdd, devA)
	addB := sign(privA, enrolment.OpAdd, devB, enrolment.OpHash(addA))

	// B alone: its admission cites add-A, which the node has never seen → 401.
	if resp := as(addB, privB, nil, http.MethodGet, "/api/v1/works", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("B without evidence: status = %d, want 401", resp.StatusCode)
	}
	// B with A's admission in the header: 200 on first contact.
	if resp := as(addB, privB, []string{addA}, http.MethodGet, "/api/v1/works", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("B with evidence: status = %d, want 200 (body: %s)", resp.StatusCode, h.body(resp))
	}
	// The node recorded both, and the view holds B (unnamed) under the identity.
	if resp := h.do(http.MethodGet, "/membership/"+usr, "", nil); resp.StatusCode != http.StatusOK || len(membership(resp)) != 2 {
		t.Fatalf("GET /membership after first contact: status = %d", resp.StatusCode)
	}
	if resp := as(addB, privB, nil, http.MethodGet, "/api/v1/works", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("B on second contact, no header: status = %d", resp.StatusCode)
	}
	if n := h.countRows(t, `SELECT count(*) FROM device_identities WHERE device_key = ? AND name = ''`, devB); n != 1 {
		t.Fatalf("B's materialised row = %d, want 1 unnamed", n)
	}

	// /enrol with the op-set shape names B; a second submit is idempotent (200).
	enrol := func(body string) *http.Response {
		return h.do(http.MethodPost, "/enrol", "", strings.NewReader(body))
	}
	opsJSON, _ := json.Marshal([]string{addA})
	resp := enrol(fmt.Sprintf(`{"op":%q,"proof":%q,"name":"phone-b","ops":%s}`, addB, proof(privB, addB), opsJSON))
	if resp.StatusCode != http.StatusOK { // the row already existed (first contact built it)
		t.Fatalf("POST /enrol B: status = %d, want 200 (body: %s)", resp.StatusCode, h.body(resp))
	}
	var enrolled struct {
		DeviceKey  string `json:"device_key"`
		Name       string `json:"name"`
		AdmittedBy string `json:"admitted_by"`
	}
	if err := json.Unmarshal(h.body(resp), &enrolled); err != nil {
		t.Fatal(err)
	}
	if enrolled.DeviceKey != devB || enrolled.Name != "phone-b" || enrolled.AdmittedBy != enrolment.OpHash(addB) {
		t.Fatalf("enrolled = %+v", enrolled)
	}
	// A device the node has never met at all, enrolling with `cert` (the
	// ADR-0067 field) carrying a v3 op and `ops` carrying its evidence: 201.
	devC, privC := newDevice()
	addC := sign(privB, enrolment.OpAdd, devC, enrolment.OpHash(addB))
	resp = enrol(fmt.Sprintf(`{"cert":%q,"proof":%q,"name":"tablet"}`, addC, proof(privC, addC)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /enrol C: status = %d, want 201 (body: %s)", resp.StatusCode, h.body(resp))
	}
	// A leaked op with a proof by the wrong key creates nothing.
	devD, _ := newDevice()
	_, privX := newDevice()
	addD := sign(privA, enrolment.OpAdd, devD, enrolment.OpHash(addC))
	if resp := enrol(fmt.Sprintf(`{"op":%q,"proof":%q,"name":"thief"}`, addD, proof(privX, addD))); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST /enrol with a wrong-key proof: status = %d, want 401", resp.StatusCode)
	}
	if n := h.countRows(t, `SELECT count(*) FROM device_identities WHERE device_key = ?`, devD); n != 0 {
		t.Fatalf("a refused enrolment materialised a row")
	}

	// ADR-0065: enrolment grants the read floor and nothing more — B, admitted
	// by a phone, writes only once an admin authorises B's OWN key (the http
	// package's TestMemberAdmittedDeviceEarnsWriteOnItsOwnKey wires the
	// authorizer and proves the lift; this harness has none).
	libBody := func() io.Reader { return strings.NewReader(`{"name":"phone-made","content_type":"movie"}`) }
	if resp := as(addB, privB, nil, http.MethodPost, "/api/v1/libraries", libBody()); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("B write: status = %d, want 403", resp.StatusCode)
	}

	// A removes B and pushes the remove. B is refused from then on, its row is
	// tombstoned, C (admitted by B BEFORE the remove) stays — no cascade through
	// history (ADR-0007).
	rmB := sign(privA, enrolment.OpRemove, devB, enrolment.OpHash(addC))
	resp = h.do(http.MethodPost, "/membership/"+usr, "", strings.NewReader(fmt.Sprintf(`{"ops":[%q]}`, rmB)))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /membership: status = %d (body: %s)", resp.StatusCode, h.body(resp))
	}
	var pushed struct {
		Ops      []string `json:"ops"`
		Recorded int      `json:"recorded"`
	}
	if err := json.Unmarshal(h.body(resp), &pushed); err != nil {
		t.Fatal(err)
	}
	if pushed.Recorded != 1 || len(pushed.Ops) != 4 {
		t.Fatalf("pushed = %+v", pushed)
	}
	if resp := as(addB, privB, nil, http.MethodGet, "/api/v1/works", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("B after the remove: status = %d, want 401", resp.StatusCode)
	}
	if n := h.countRows(t, `SELECT count(*) FROM device_identities WHERE device_key = ? AND revoked_at IS NOT NULL`, devB); n != 1 {
		t.Fatalf("B's row is not tombstoned")
	}
	if resp := as(addC, privC, nil, http.MethodGet, "/api/v1/works", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("C after B's removal: status = %d, want 200", resp.StatusCode)
	}
	if resp := as(addA, privA, nil, http.MethodGet, "/api/v1/works", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("A after the remove: status = %d, want 200", resp.StatusCode)
	}
	// Pushing it again is idempotent.
	resp = h.do(http.MethodPost, "/membership/"+usr, "", strings.NewReader(fmt.Sprintf(`{"ops":[%q]}`, rmB)))
	if err := json.Unmarshal(h.body(resp), &pushed); err != nil || pushed.Recorded != 0 {
		t.Fatalf("re-push = %+v, %v", pushed, err)
	}
	if h.eventsOfType(t, "identity.membership.recorded") < 3 || h.eventsOfType(t, "identity.device.removed") != 1 {
		t.Fatalf("events: recorded=%d removed=%d", h.eventsOfType(t, "identity.membership.recorded"), h.eventsOfType(t, "identity.device.removed"))
	}

	// Junk is refused opaquely; the log is untouched.
	for _, body := range []string{
		`{"ops":["not-an-op"]}`,
		fmt.Sprintf(`{"ops":[%q]}`, sign(privX, enrolment.OpAdd, devD, enrolment.OpHash(addC))+"x"),
	} {
		resp := h.do(http.MethodPost, "/membership/"+usr, "", strings.NewReader(body))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("POST /membership junk: status = %d, want 401 (body: %s)", resp.StatusCode, h.body(resp))
		}
	}
	// An op for ANOTHER identity is junk here too.
	other, otherPriv, _ := enrolment.GenerateUserIdentity()
	foreign, _ := enrolment.SignOp(otherPriv, other.UserID(), enrolment.OpAdd, devD, "", nil, fixedTime, 0)
	if resp := h.do(http.MethodPost, "/membership/"+usr, "", strings.NewReader(fmt.Sprintf(`{"ops":[%q]}`, foreign))); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST /membership foreign op: status = %d, want 401", resp.StatusCode)
	}
	if n := h.countRows(t, `SELECT count(*) FROM membership_ops`); n != 4 {
		t.Fatalf("membership_ops = %d, want 4", n)
	}
	// An identity the node has not pinned is 404; a non-key is 400.
	if resp := h.do(http.MethodGet, "/membership/"+other.UserID(), "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /membership unpinned: status = %d, want 404", resp.StatusCode)
	}
	if resp := h.do(http.MethodGet, "/membership/nobody", "", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET /membership garbage: status = %d, want 400", resp.StatusCode)
	}
	// Too many presented ops is a refusal, not a truncation.
	req, _ := http.NewRequest(http.MethodGet, h.http.URL+"/api/v1/works", nil)
	req.Header.Set("Authorization", "Device "+addA+"~"+proof(privA, addA))
	req.Header.Set(rp.MembershipHeader, strings.Repeat(addA+",", rp.MaxPresentedOps)+addA)
	tooMany, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tooMany.Body.Close() })
	if tooMany.StatusCode != http.StatusUnauthorized {
		t.Fatalf("over-cap header: status = %d, want 401", tooMany.StatusCode)
	}
}
