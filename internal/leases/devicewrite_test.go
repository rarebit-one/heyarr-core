package leases_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/grant"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/guest"
	"github.com/rarebit-one/heyarr-core/internal/leases"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// writeDevices is a WriteDevices whose grants a test edits between passes.
type writeDevices struct {
	keys []string
	err  error
}

func (w *writeDevices) WriteDeviceKeys(context.Context) ([]string, error) {
	return slices.Clone(w.keys), w.err
}

// siblingSites is a SiblingKeys whose membership a test edits between passes.
// The issuer reads only the rendered keys.
type siblingSites struct {
	keys []string
	err  error
}

func (s *siblingSites) PeerKeys(context.Context) (map[string]ed25519.PublicKey, error) {
	out := map[string]ed25519.PublicKey{}
	for _, k := range s.keys {
		out[k] = nil
	}
	return out, s.err
}

// issuerHarness is one peer's lease store on a real migrated database, with a
// clock the test moves (ADR-0017: no sleeps).
type issuerHarness struct {
	db       *sqlite.DB
	clock    *fixedClock
	store    *leases.Store
	self     string
	devices  *writeDevices
	siblings *siblingSites
	issuer   *leases.DeviceWriteIssuer
}

func newIssuerHarness(t *testing.T) *issuerHarness {
	t.Helper()
	db := testdb.Migrated(t)
	clock := &fixedClock{t: now}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := leases.New(leases.Options{
		Writer: db.Writer(), Reader: db.Reader(), Events: log, Signer: signer, Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &issuerHarness{
		db: db, clock: clock, store: store,
		self:     identity.FormatPublicKey(signer.Public().(ed25519.PublicKey)),
		devices:  &writeDevices{},
		siblings: &siblingSites{},
	}
	h.issuer, err = leases.NewDeviceWriteIssuer(store, h.devices, h.siblings)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// active renders this peer's live device write leases as "principal@resource",
// sorted, one entry per lease (so a refreshed pair shows twice).
func (h *issuerHarness) active(t *testing.T) []string {
	t.Helper()
	ls, err := h.store.ActiveLeases(context.Background(), h.clock.t)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range ls {
		if leases.IsDeviceWriteLease(l) {
			out = append(out, l.Principal+"@"+l.Resource)
		}
	}
	slices.Sort(out)
	return out
}

func (h *issuerHarness) events(t *testing.T, typ string) int {
	t.Helper()
	var n int
	if err := h.db.Reader().QueryRowContext(context.Background(),
		`SELECT count(*) FROM events WHERE type = ?`, typ).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func pair(device, site string) string {
	return leases.DevicePrincipal(device) + "@" + leases.SiteResource(site)
}

const (
	devD = "ed25519:d0d0"
	devE = "ed25519:e0e0"
	sibB = "ed25519:b0b0"
	sibC = "ed25519:c0c0"
)

// One pass of the issuer, then what must hold. Steps run in order on one peer;
// the event counts are cumulative over the scenario.
type issuerStep struct {
	name     string
	at       time.Duration // clock offset from `now`
	devices  []string
	siblings []string
	want     leases.DeviceWriteResult
	active   []string
	issued   int // lease.issued events so far
	revoked  int // lease.revoked events so far
}

// TestDeviceWriteIssuerReconcile walks the delivery plan's assertions (#285,
// item 2): a grant for D with sibling B yields exactly one {device:D, site:B,
// write}; a pass above the refresh threshold reuses it; a pass below issues a
// fresh one; deleting the grant revokes it and the next pass issues none.
func TestDeviceWriteIssuerReconcile(t *testing.T) {
	t.Parallel()
	ttl, threshold := leases.DeviceWriteTTL, leases.DeviceWriteRefreshThreshold
	// The instant the first lease's remaining life reaches the re-issue point,
	// the threshold plus the slack that covers one issuer beat.
	atThreshold := ttl - threshold - leases.DeviceWriteReissueSlack

	tests := []struct {
		name  string
		steps []issuerStep
	}{
		{
			name: "a grant for D and sibling B yields exactly one write lease",
			steps: []issuerStep{{
				name: "first pass", devices: []string{devD}, siblings: []string{sibB},
				want: leases.DeviceWriteResult{Issued: 1}, active: []string{pair(devD, sibB)}, issued: 1,
			}},
		},
		{
			name: "a pass above the threshold reuses the live lease",
			steps: []issuerStep{
				{
					name: "issue", devices: []string{devD}, siblings: []string{sibB},
					want: leases.DeviceWriteResult{Issued: 1}, active: []string{pair(devD, sibB)}, issued: 1,
				},
				{
					name: "same instant", devices: []string{devD}, siblings: []string{sibB},
					want: leases.DeviceWriteResult{Reused: 1}, active: []string{pair(devD, sibB)}, issued: 1,
				},
				{
					name: "a minute above", at: atThreshold - time.Minute, devices: []string{devD}, siblings: []string{sibB},
					want: leases.DeviceWriteResult{Reused: 1}, active: []string{pair(devD, sibB)}, issued: 1,
				},
				{
					name: "exactly the threshold left", at: atThreshold, devices: []string{devD}, siblings: []string{sibB},
					want: leases.DeviceWriteResult{Reused: 1}, active: []string{pair(devD, sibB)}, issued: 1,
				},
			},
		},
		{
			name: "a pass below the threshold issues a fresh lease and keeps the old one live",
			steps: []issuerStep{
				{
					name: "issue", devices: []string{devD}, siblings: []string{sibB},
					want: leases.DeviceWriteResult{Issued: 1}, active: []string{pair(devD, sibB)}, issued: 1,
				},
				{
					name: "a minute below", at: atThreshold + time.Minute, devices: []string{devD}, siblings: []string{sibB},
					want:   leases.DeviceWriteResult{Issued: 1},
					active: []string{pair(devD, sibB), pair(devD, sibB)}, issued: 2,
				},
				{
					name: "the fresh lease is reused", at: atThreshold + 2*time.Minute, devices: []string{devD}, siblings: []string{sibB},
					want:   leases.DeviceWriteResult{Reused: 1},
					active: []string{pair(devD, sibB), pair(devD, sibB)}, issued: 2,
				},
				{
					// The first lease expires on its own (never revoked). With the
					// re-issue slack a refresh falls due every TTL − threshold − slack,
					// a little under half the TTL, so by now the second lease is due too
					// and a third is issued: two live, the first gone.
					name: "the old lease expires on its own", at: ttl, devices: []string{devD}, siblings: []string{sibB},
					want:   leases.DeviceWriteResult{Issued: 1},
					active: []string{pair(devD, sibB), pair(devD, sibB)}, issued: 3,
				},
			},
		},
		{
			name: "deleting the grant revokes its lease and the next pass issues none",
			steps: []issuerStep{
				{
					name: "issue", devices: []string{devD}, siblings: []string{sibB},
					want: leases.DeviceWriteResult{Issued: 1}, active: []string{pair(devD, sibB)}, issued: 1,
				},
				{
					name: "grant deleted", at: time.Minute, siblings: []string{sibB},
					want: leases.DeviceWriteResult{Revoked: 1}, issued: 1, revoked: 1,
				},
				{
					name: "next pass", at: 2 * time.Minute, siblings: []string{sibB},
					want: leases.DeviceWriteResult{}, issued: 1, revoked: 1,
				},
			},
		},
		{
			name: "deleting the grant revokes every live lease it had, refreshed ones too",
			steps: []issuerStep{
				{
					name: "issue", devices: []string{devD}, siblings: []string{sibB},
					want: leases.DeviceWriteResult{Issued: 1}, active: []string{pair(devD, sibB)}, issued: 1,
				},
				{
					name: "refresh", at: atThreshold + time.Minute, devices: []string{devD}, siblings: []string{sibB},
					want:   leases.DeviceWriteResult{Issued: 1},
					active: []string{pair(devD, sibB), pair(devD, sibB)}, issued: 2,
				},
				{
					name: "grant deleted", at: atThreshold + 2*time.Minute, siblings: []string{sibB},
					want: leases.DeviceWriteResult{Revoked: 2}, issued: 2, revoked: 2,
				},
			},
		},
		{
			name: "each device gets a lease for each sibling",
			steps: []issuerStep{{
				name: "first pass", devices: []string{devD, devE}, siblings: []string{sibB, sibC},
				want:   leases.DeviceWriteResult{Issued: 4},
				active: []string{pair(devD, sibB), pair(devD, sibC), pair(devE, sibB), pair(devE, sibC)}, issued: 4,
			}},
		},
		{
			name: "a sibling leaving membership loses its leases and the others keep theirs",
			steps: []issuerStep{
				{
					name: "issue", devices: []string{devD}, siblings: []string{sibB, sibC},
					want: leases.DeviceWriteResult{Issued: 2}, active: []string{pair(devD, sibB), pair(devD, sibC)}, issued: 2,
				},
				{
					name: "C removed", at: time.Minute, devices: []string{devD}, siblings: []string{sibB},
					want:   leases.DeviceWriteResult{Reused: 1, Revoked: 1},
					active: []string{pair(devD, sibB)}, issued: 2, revoked: 1,
				},
			},
		},
		{
			name: "a single-site node issues nothing",
			steps: []issuerStep{{
				name: "no siblings", devices: []string{devD},
				want: leases.DeviceWriteResult{},
			}},
		},
		{
			name: "a blank device key is never a principal",
			steps: []issuerStep{{
				name: "blank key", devices: []string{""}, siblings: []string{sibB},
				want: leases.DeviceWriteResult{},
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newIssuerHarness(t)
			for _, st := range tc.steps {
				h.clock.t = now.Add(st.at)
				h.devices.keys = st.devices
				h.siblings.keys = st.siblings
				got, err := h.issuer.Reconcile(context.Background())
				if err != nil {
					t.Fatalf("%s: reconcile: %v", st.name, err)
				}
				if got != st.want {
					t.Errorf("%s: result = %+v, want %+v", st.name, got, st.want)
				}
				if active := h.active(t); !slices.Equal(active, st.active) {
					t.Errorf("%s: active device write leases = %v, want %v", st.name, active, st.active)
				}
				if n := h.events(t, events.TypeLeaseIssued); n != st.issued {
					t.Errorf("%s: %s events = %d, want %d", st.name, events.TypeLeaseIssued, n, st.issued)
				}
				if n := h.events(t, events.TypeLeaseRevoked); n != st.revoked {
					t.Errorf("%s: %s events = %d, want %d", st.name, events.TypeLeaseRevoked, n, st.revoked)
				}
			}
		})
	}
}

// The issued lease is exactly the binding the amendment names: principal
// device:<D>, resource site:<B>, the write capability only, the full TTL, and
// signed by this peer so B verifies it against the key it pinned.
func TestDeviceWriteLeaseShape(t *testing.T) {
	t.Parallel()
	h := newIssuerHarness(t)
	h.devices.keys, h.siblings.keys = []string{devD}, []string{sibB, h.self}
	if _, err := h.issuer.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	ls, err := h.store.ActiveLeases(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	// This peer's own key in the sibling set gets no lease: it would lift
	// nothing here, where the grant itself authorises write.
	if len(ls) != 1 {
		t.Fatalf("got %d leases, want exactly one (none for this peer's own site): %+v", len(ls), ls)
	}
	l := ls[0]
	if l.Principal != "device:"+devD || l.Resource != "site:"+sibB || l.Issuer != h.self {
		t.Errorf("lease binds %q / %q issued by %q", l.Principal, l.Resource, l.Issuer)
	}
	if !slices.Equal(l.Capabilities, []grant.Capability{grant.CapabilityWrite}) {
		t.Errorf("capabilities = %v, want only write", l.Capabilities)
	}
	if life := l.ExpiresAt.Sub(l.IssuedAt); life != grant.MaxTTL {
		t.Errorf("life = %v, want grant.MaxTTL", life)
	}
	req := grant.Request{Principal: l.Principal, Resource: l.Resource, Capability: grant.CapabilityWrite}
	if _, err := h.store.Honour(context.Background(), l.Token, req, now); err != nil {
		t.Errorf("the signed token does not verify: %v", err)
	}
}

// A pass that cannot read the grants or the siblings touches nothing. Reading
// "none" from an error would revoke every device's write at every sibling.
func TestDeviceWriteIssuerFailsWithoutRevoking(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for _, tc := range []struct {
		name    string
		breakIt func(h *issuerHarness)
	}{
		{"grants unreadable", func(h *issuerHarness) { h.devices.err = boom }},
		{"siblings unreadable", func(h *issuerHarness) { h.siblings.err = boom }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newIssuerHarness(t)
			h.devices.keys, h.siblings.keys = []string{devD}, []string{sibB}
			if _, err := h.issuer.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			tc.breakIt(h)
			h.clock.t = now.Add(time.Minute)
			if _, err := h.issuer.Reconcile(context.Background()); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the read failure", err)
			}
			if active := h.active(t); !slices.Equal(active, []string{pair(devD, sibB)}) {
				t.Errorf("active after a failed pass = %v, want the lease untouched", active)
			}
			if n := h.events(t, events.TypeLeaseRevoked); n != 0 {
				t.Errorf("a failed pass revoked %d leases", n)
			}
		})
	}
}

// Siblings are served device write leases only. A guest lease (ADR-0094) is
// node-local: it still admits a guest here, and it never reaches a sibling.
// Leases the issuer does not own (a guest's, another kind) are not revoked by
// a pass either.
func TestActiveLeaseTokensServesOnlyDeviceWriteLeases(t *testing.T) {
	t.Parallel()
	const source = "192.0.2.10"
	tests := []struct {
		name        string
		guest       bool
		grant       bool
		readLease   bool // a device:/site: lease without write
		wantDevice  int
		wantServed  int
		wantLocally int // guest leases still live in ActiveLeases
	}{
		{name: "guest only", guest: true, wantServed: 0, wantLocally: 1},
		{name: "guest and a device write lease", guest: true, grant: true, wantDevice: 1, wantServed: 1, wantLocally: 1},
		{name: "a device lease without write is not served", readLease: true, wantServed: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			h := newIssuerHarness(t)
			minter := guest.NewMinter(h.store, 0)
			if tc.guest {
				if _, err := minter.GuestLease(ctx, source, now); err != nil {
					t.Fatal(err)
				}
			}
			if tc.readLease {
				if _, err := h.store.Issue(ctx, leases.DevicePrincipal(devD), leases.SiteResource(sibB),
					[]grant.Capability{grant.CapabilityRead}, time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			if tc.grant {
				h.devices.keys, h.siblings.keys = []string{devD}, []string{sibB}
			}
			if _, err := h.issuer.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}

			tokens, err := h.store.ActiveLeaseTokens(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(tokens) != tc.wantServed {
				t.Fatalf("served %d tokens, want %d", len(tokens), tc.wantServed)
			}
			all, err := h.store.ActiveLeases(ctx, now)
			if err != nil {
				t.Fatal(err)
			}
			byToken := map[string]leases.Lease{}
			guests := 0
			for _, l := range all {
				byToken[l.Token] = l
				if l.Principal == guest.Principal {
					guests++
				}
			}
			for _, tok := range tokens {
				l := byToken[tok]
				if l.Principal == guest.Principal || !leases.IsDeviceWriteLease(l) {
					t.Errorf("served a lease that is not a device write lease: %q / %q %v", l.Principal, l.Resource, l.Capabilities)
				}
			}
			if guests != tc.wantLocally {
				t.Errorf("%d live guest leases, want %d (the issuer must not revoke them)", guests, tc.wantLocally)
			}
			if tc.guest {
				// The guest tier still works here: the next request reuses its lease.
				if _, err := minter.GuestLease(ctx, source, now.Add(time.Minute)); err != nil {
					t.Fatalf("guest admission after a pass: %v", err)
				}
				if n := h.events(t, events.TypeLeaseIssued); n != 1+tc.wantDevice {
					t.Errorf("%d leases issued, want the one guest lease reused and %d device leases", n, tc.wantDevice)
				}
			}
		})
	}
}
