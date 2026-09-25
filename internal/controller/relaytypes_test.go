package controller

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rarebit-one/voidbind-go/enrolment"
	"github.com/rarebit-one/voidbind-go/pairflow"
	"github.com/rarebit-one/voidbind-go/pairing"
	vbrelay "github.com/rarebit-one/voidbind-go/relay"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/relay"
)

// TestNodeRelayCarriesThePairingDefaults: the node's relay is built on
// voidbind-go's DefaultTypes, so every pairing slot voidbind-go defines —
// including ADR-0012's `refuse` — is served at /pair/v1 without a node change.
func TestNodeRelayCarriesThePairingDefaults(t *testing.T) {
	types := nodeRelayTypes()
	for _, want := range append(slices.Clone(vbrelay.DefaultTypes), "refuse") {
		if !slices.Contains(types, want) {
			t.Fatalf("the node relay's slots %v lack %q", types, want)
		}
	}
}

// TestNodeRelayRoundTripsARefusal is ADR-0012 through the node's relay as the
// controller mounts it: after the handshake, the initiator refuses, and the
// responder waiting in Receive gets pairflow.ErrRefused at once instead of
// waiting out its deadline. A relay without the `refuse` slot would answer the
// refusal 400 and leave the responder to time out.
func TestNodeRelayRoundTripsARefusal(t *testing.T) {
	r := chi.NewRouter()
	relay.New(relay.Options{Types: nodeRelayTypes()}).Mount(r)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	base := ts.URL + httpapi.RelayPrefix

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := vbrelay.CreateSession(ctx, ts.Client(), base)
	if err != nil {
		t.Fatalf("create session through the node: %v", err)
	}
	salt, err := pairing.NewSalt()
	if err != nil {
		t.Fatal(err)
	}
	u, userPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	in, err := pairflow.NewInitiator(userPriv, salt, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := pairflow.NewResponder(u.UserID(), salt, now)
	if err != nil {
		t.Fatal(err)
	}
	poll := 10 * time.Millisecond
	initT := &vbrelay.Client{Base: base, Session: session, Role: string(pairflow.RoleInitiator), HTTP: ts.Client(), PollInterval: poll}
	respT := &vbrelay.Client{Base: base, Session: session, Role: string(pairflow.RoleResponder), HTTP: ts.Client(), PollInterval: poll}

	respErr := make(chan error, 1)
	go func() {
		if _, err := resp.Handshake(ctx, respT); err != nil {
			respErr <- err
			return
		}
		_, err := resp.Receive(ctx, respT)
		respErr <- err
	}()
	if _, err := in.Handshake(ctx, initT); err != nil {
		t.Fatalf("initiator handshake: %v", err)
	}
	// The human said the codes differ.
	start := time.Now()
	if err := in.Refuse(ctx, initT); err != nil {
		t.Fatalf("the node relay rejected the refusal: %v", err)
	}
	err = <-respErr
	if !errors.Is(err, pairflow.ErrRefused) {
		t.Fatalf("responder: %v, want ErrRefused", err)
	}
	// Fast, not the 10s deadline: a handful of 10ms polls.
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the refusal took %s to reach the responder", took)
	}
	if err := in.Authorise(ctx, initT); err == nil {
		t.Fatal("Authorise after Refuse succeeded")
	}
}
