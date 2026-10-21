package controller

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/leases"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// The device write lease beat is wired to the real stores the controller
// gives it — the catalog's management grants and peer membership — and runs on
// the ticker it is handed: a startup pass issues {device:D, site:B, write} for
// the one grant and the one sibling, and the pass on the next tick after the
// grant is deleted revokes it. The mechanism has a caller (#362).
func TestDeviceLeaseBeatIssuesAndRevokesFromTheControllersStores(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	db := testdb.Migrated(t)
	clock := &movableClock{t: time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{DB: db, Events: eventLog, PeerName: "self", PeerSite: "site-a", Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	members, err := membership.New(membership.Options{DB: db, Events: eventLog, Logger: discard()})
	if err != nil {
		t.Fatal(err)
	}
	selfPub, signer, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	siblingPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []membership.Registration{
		{Name: "peer-a", Site: "site-a", Mode: "full", PublicKey: selfPub, IsSelf: true, Endpoint: "https://peer-a.invalid:7780"},
		{Name: "peer-b", Site: "site-b", Mode: "full", PublicKey: siblingPub, Endpoint: "https://peer-b.invalid:7780"},
	} {
		if _, err := members.Register(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	const device = "ed25519:0d0d"
	if _, err := cat.GrantManagement(ctx, device, "test"); err != nil {
		t.Fatal(err)
	}
	wantPrincipal := leases.DevicePrincipal(device)
	wantResource := leases.SiteResource(identity.FormatPublicKey(siblingPub))

	ticker := newFakeTicker()
	if err := startDeviceLeaseIssuer(ctx, db, eventLog, signer, cat, members, clock, discard(), ticker.newTicker); err != nil {
		t.Fatal(err)
	}
	if ticker.made != 1 || ticker.interval != deviceLeaseInterval {
		t.Fatalf("made %d tickers at %v, want one at %v", ticker.made, ticker.interval, deviceLeaseInterval)
	}

	// A store over the same table, to read what the beat wrote.
	reader, err := leases.New(leases.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	served := func() []leases.Lease {
		t.Helper()
		ls, err := reader.ActiveLeases(ctx, clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		return ls
	}

	// The startup pass ran before startDeviceLeaseIssuer returned.
	ls := served()
	if len(ls) != 1 || ls[0].Principal != wantPrincipal || ls[0].Resource != wantResource || !leases.IsDeviceWriteLease(ls[0]) {
		t.Fatalf("after the startup pass: %+v, want one write lease %s @ %s", ls, wantPrincipal, wantResource)
	}
	if tokens, err := reader.ActiveLeaseTokens(ctx); err != nil || len(tokens) != 1 || tokens[0] != ls[0].Token {
		t.Fatalf("served tokens = %v (%v), want the one device lease", tokens, err)
	}

	if _, err := cat.RevokeManagement(ctx, device); err != nil {
		t.Fatal(err)
	}
	clock.advance(deviceLeaseInterval)
	ticker.fire(t)
	ticker.fire(t) // returns once the first tick's pass has finished
	if ls := served(); len(ls) != 0 {
		t.Fatalf("after the grant was deleted: %+v, want none", ls)
	}

	cancel()
	ticker.awaitStop(t)
}
