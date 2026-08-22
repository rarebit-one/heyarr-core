// Every response in this file is drained and closed by the helper that made
// the request, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by peerSend
package peerapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// This file is M4-06's acceptance: the peer → controller link (ADR-0033).
//
// M4-05 next door proved that the transport authenticates the right key. This
// proves what the surface does with the identity that authentication produced,
// and the property under test is one sentence: THE ACTING PEER COMES FROM THE
// CERTIFICATE AND NEVER FROM THE REQUEST BODY.
//
// A test that only sent well-formed requests would pass against a server that
// read the acting peer straight out of the body — every peer would be
// authenticated perfectly, and any of them could act as any other. So the
// central test sends a body naming a DIFFERENT peer and asserts a refusal, and
// it is paired with a control that proves the same request shape succeeds when
// the declaration matches.

// peerSend issues a request over a pinned peer client and returns the status
// and the body.
func peerSend(t *testing.T, c *http.Client, method, url, body string) (status int, out string, reused bool, err error) {
	t.Helper()
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
	})
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, reqErr := http.NewRequestWithContext(ctx, method, url, reader)
	if reqErr != nil {
		t.Fatal(reqErr)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", reused, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatal(readErr)
	}
	return resp.StatusCode, string(raw), reused, nil
}

func decodeAttachment(t *testing.T, body string) peerapi.Attachment {
	t.Helper()
	var out peerapi.Attachment
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("the peer surface answered something that is not an attachment: %v\n%s", err, body)
	}
	return out
}

func (l *listener) attachURL() string { return "https://" + l.addr + peerapi.Prefix + "/attach" }
func (l *listener) attachmentURL() string {
	return "https://" + l.addr + peerapi.Prefix + "/attachment"
}

// ---------------------------------------------------------------------------
// the peer authenticates to the controller with its ADR-0012 identity

func TestAPeerAttachesToTheControllerWithItsOwnCertificate(t *testing.T) {
	controller := newPeerNode(t, "controller-id", "controller")
	remote := newPeerNode(t, "remote-peer-id", "remote-peer")
	root := newTrustRoot(controller.member(), remote.member())
	l := serve(t, controller, root)
	c := dialler(t, remote, root)

	// No bearer token anywhere. The certificate is the whole credential, which
	// is the decision ADR-0033 records: one mechanism, one credential, one
	// revocation path.
	status, body, _, err := peerSend(t, c, http.MethodPost, l.attachURL(),
		`{"peer_id":"`+remote.peerID+`"}`)
	if err != nil {
		t.Fatalf("a remote peer could not attach to the controller: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("POST attach = %d\n%s", status, body)
	}

	got := decodeAttachment(t, body)
	if got.PeerID != remote.peerID {
		t.Errorf("the controller derived peer id %q, want %q", got.PeerID, remote.peerID)
	}
	if got.Name != remote.name {
		t.Errorf("the controller derived name %q, want %q", got.Name, remote.name)
	}
	if got.PublicKey != identity.FormatPublicKey(remote.pub) {
		t.Errorf("the controller pinned %q, want %q", got.PublicKey, identity.FormatPublicKey(remote.pub))
	}
	if got.Controller != controller.peerID {
		t.Errorf("controller = %q, want the answering node %q", got.Controller, controller.peerID)
	}
	// ADR-0029, stated in the response so a peer that ever gets something else
	// fails loudly rather than assuming.
	if got.ControlPlane != peerapi.ControlPlaneAttached {
		t.Errorf("control_plane = %q, want %q", got.ControlPlane, peerapi.ControlPlaneAttached)
	}
	// The escalation check, from the peer's side: what authenticated is a
	// peer, and a peer is not an admin.
	if got.Principal != "peer" {
		t.Errorf("principal = %q, want \"peer\" — anything else here is a privilege escalation "+
			"(ADR-0033)", got.Principal)
	}

	// And the derived-only route agrees. It carries no body and no path
	// parameter, so there is nothing a caller could have influenced.
	status, body, _, err = peerSend(t, c, http.MethodGet, l.attachmentURL(), "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET attachment = %d %v\n%s", status, err, body)
	}
	if derived := decodeAttachment(t, body); derived != got {
		t.Errorf("the declared path and the derived path describe different attachments:\n%+v\n%+v",
			got, derived)
	}
}

// ---------------------------------------------------------------------------
// peer A cannot act as peer B

// TestPeerACannotActAsPeerB is the sharpest test in this milestone.
//
// It sends a body naming ANOTHER peer and asserts a refusal — not that it
// succeeds under the wrong identity, and not merely that the response happens
// to carry the right id. Both weaker assertions pass against a server that
// reads the acting peer out of the body.
func TestPeerACannotActAsPeerB(t *testing.T) {
	controller := newPeerNode(t, "controller-id", "controller")
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(controller.member(), a.member(), b.member())
	l := serve(t, controller, root)

	// The control. Both peers are members and both can attach as themselves,
	// so the refusal below is about WHO IS ACTING and not about peer A being a
	// stranger — which is the distinction the whole test rests on.
	for _, node := range []*peerNode{a, b} {
		status, body, _, err := peerSend(t, dialler(t, node, root), http.MethodPost, l.attachURL(),
			`{"peer_id":"`+node.peerID+`"}`)
		if err != nil || status != http.StatusOK {
			t.Fatalf("%s could not attach as itself (%d %v), so the impersonation below proves nothing\n%s",
				node.peerID, status, err, body)
		}
		if got := decodeAttachment(t, body); got.PeerID != node.peerID {
			t.Fatalf("the control resolved %q, want %q", got.PeerID, node.peerID)
		}
	}

	// Peer A's certificate, peer B's id in the body.
	status, body, _, err := peerSend(t, dialler(t, a, root), http.MethodPost, l.attachURL(),
		`{"peer_id":"`+b.peerID+`"}`)
	if err != nil {
		t.Fatalf("the impersonation attempt failed at the transport (%v) rather than being refused "+
			"by the surface, so this run says nothing about authorisation", err)
	}
	if status != http.StatusForbidden {
		t.Fatalf("peer %s acted as peer %s and got %d, want 403. The acting peer must come from "+
			"the certificate and never from the request body (ADR-0033)\n%s",
			a.peerID, b.peerID, status, body)
	}
	// Not "it returned 403 for some reason": it refused because the caller
	// named somebody else.
	if !strings.Contains(body, "never from the request body") {
		t.Errorf("the refusal does not say why: %s", body)
	}
	// And it did not quietly succeed under either identity.
	if strings.Contains(body, `"control_plane"`) {
		t.Errorf("the refusal carries an attachment — the request was served after all: %s", body)
	}

	// The listener recorded both identities on its own side, where naming them
	// is safe and where an operator can see who tried what.
	logs := l.logs.String()
	if !strings.Contains(logs, "acting_peer_id="+a.peerID) || !strings.Contains(logs, "declared_peer_id="+b.peerID) {
		t.Errorf("the controller refused an impersonation without recording it:\n%s", logs)
	}
}

// TestADeclarationIsRequiredAndIsNeverDefaultedToTheCertificate.
//
// The declaration exists to be compared. A body that may be omitted is a body
// that stops being compared, and the reflex fix — "fall back to the
// certificate's id" — is the one that makes the comparison decorative.
func TestADeclarationIsRequiredAndIsNeverDefaultedToTheCertificate(t *testing.T) {
	controller := newPeerNode(t, "controller-id", "controller")
	remote := newPeerNode(t, "remote-peer-id", "remote-peer")
	root := newTrustRoot(controller.member(), remote.member())
	l := serve(t, controller, root)
	c := dialler(t, remote, root)

	for _, tc := range []struct{ name, body string }{
		{"an empty object", `{}`},
		{"an empty peer id", `{"peer_id":""}`},
		{"not an object", `"remote-peer-id"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body, _, err := peerSend(t, c, http.MethodPost, l.attachURL(), tc.body)
			if err != nil {
				t.Fatalf("the request failed at the transport: %v", err)
			}
			if status != http.StatusBadRequest {
				t.Fatalf("attach with %s = %d, want 400 — it must not be served as the peer the "+
					"certificate names\n%s", tc.name, status, body)
			}
		})
	}
}

// TestTheAttachmentIsDerivedRatherThanEchoed states the property the endpoint
// exists for, as an assertion about a value rather than about a status code.
//
// A surface that echoed the body would return the same 200 and the same shape
// on every honest request ever made against it.
func TestTheAttachmentIsDerivedRatherThanEchoed(t *testing.T) {
	controller := newPeerNode(t, "controller-id", "controller")
	// The name deliberately shares no substring with the id: the assertion
	// below is that the name was READ FROM MEMBERSHIP, and a name the request
	// body happens to contain would make it prove nothing.
	remote := newPeerNode(t, "remote-peer-id", "custodian-at-site-b")
	root := newTrustRoot(controller.member(), remote.member())
	l := serve(t, controller, root)

	status, body, _, err := peerSend(t, dialler(t, remote, root), http.MethodPost, l.attachURL(),
		`{"peer_id":"`+remote.peerID+`"}`)
	if err != nil || status != http.StatusOK {
		t.Fatalf("attach = %d %v\n%s", status, err, body)
	}
	got := decodeAttachment(t, body)
	if got.Name != remote.name {
		t.Fatalf("name = %q, want %q — it comes from the membership record, and nothing in the "+
			"request carried it", got.Name, remote.name)
	}
	if strings.Contains(`{"peer_id":"`+remote.peerID+`"}`, got.Name) {
		t.Fatal("the request body contains the name, so this assertion could be an echo")
	}
}

// ---------------------------------------------------------------------------
// a peer credential reaches the peer surface and nothing else

// TestTheAdminSurfaceIsNotServedOnThePeerListener asserts the structural half
// of "a peer is not an admin": the routes simply are not here.
//
// The route-by-route sweep across the whole admin surface lives in
// internal/controller, where both routers are in scope and the admin routes
// can be discovered from the real router rather than typed out. This is the
// same fact asserted over a real mTLS connection by an authenticated peer,
// because the discovery test cannot make one.
func TestTheAdminSurfaceIsNotServedOnThePeerListener(t *testing.T) {
	controller := newPeerNode(t, "controller-id", "controller")
	remote := newPeerNode(t, "remote-peer-id", "remote-peer")
	root := newTrustRoot(controller.member(), remote.member())
	l := serve(t, controller, root)
	c := dialler(t, remote, root)

	// The control: this connection is authenticated and can reach the peer
	// surface. Without it, every 404 below would also be produced by a
	// connection that was refused outright.
	if status, body, _, err := peerSend(t, c, http.MethodGet, l.attachmentURL(), ""); err != nil || status != http.StatusOK {
		t.Fatalf("the authenticated control request failed (%d %v), so the refusals below prove nothing\n%s",
			status, err, body)
	}

	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/tokens"},
		{http.MethodPost, "/api/v1/tokens"},
		{http.MethodDelete, "/api/v1/tokens/some-id"},
		{http.MethodPost, "/api/v1/peers"},
		{http.MethodDelete, "/api/v1/peers/some-id"},
		// And the peer prefix's own imagined admin route.
		{http.MethodPost, peerapi.Prefix + "/tokens"},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			status, body, _, err := peerSend(t, c, route.method, "https://"+l.addr+route.path, `{}`)
			if err != nil {
				t.Fatalf("the request failed at the transport: %v", err)
			}
			if status != http.StatusNotFound {
				t.Fatalf("an authenticated peer reached %s %s: %d. A peer certificate authorises "+
					"the peer surface only (ADR-0033)\n%s", route.method, route.path, status, body)
			}
		})
	}
}

// TestAuthorisesIsAnEquality pins the authorisation rule itself, away from a
// handshake, so a widening is a failing unit test rather than a behaviour
// nobody notices.
func TestAuthorisesIsAnEquality(t *testing.T) {
	controller := newPeerNode(t, "controller-id", "controller")
	remote := newPeerNode(t, "remote-peer-id", "remote-peer")
	root := newTrustRoot(controller.member(), remote.member())
	l := serve(t, controller, root)

	// The empty string is the case worth naming: a missing id must not be read
	// as "no particular peer, so allow it".
	status, body, _, err := peerSend(t, dialler(t, remote, root), http.MethodPost, l.attachURL(),
		`{"peer_id":" "}`)
	if err != nil {
		t.Fatalf("the request failed at the transport: %v", err)
	}
	if status != http.StatusForbidden {
		t.Fatalf("a whitespace peer id was answered %d, want 403\n%s", status, body)
	}
}
