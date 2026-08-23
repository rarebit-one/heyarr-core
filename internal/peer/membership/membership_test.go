package membership_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// Everything here runs against a real migrated SQLite database. The refusals
// this package is responsible for are partly the schema's — the unique index
// on public_key is the half that also holds when a row arrives through a
// restore — so a fake store would be testing this file's idea of the rules.

type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }

var fixedTime = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

type fixture struct {
	t     *testing.T
	db    *sqlite.DB
	store *membership.Store
	log   *events.Log
	clock *fixedClock
	// selfID is the peer this node is, created the way production creates it.
	selfID string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	clock := &fixedClock{t: fixedTime}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	// The self peer through the real path (ADR-0010), not an INSERT: the
	// "cannot remove self" and "cannot register a second self" refusals are
	// about the row production actually creates.
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: log, PeerName: "this-node", PeerSite: "site-a", Clock: clock,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	selfID, err := cat.SelfPeer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	store, err := membership.New(membership.Options{
		DB: db, Events: log, Clock: clock, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, db: db, store: store, log: log, clock: clock, selfID: selfID}
}

// key mints a real Ed25519 public key. Real, rather than 32 arbitrary bytes,
// because enrolment renders and re-parses it and a value that only happens to
// be the right length would not survive that round trip.
func key(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func (f *fixture) register(name string, pub []byte, endpoint string) membership.Result {
	f.t.Helper()
	res, err := f.store.Register(context.Background(), membership.Registration{
		Name: name, Site: "site-b", Endpoint: endpoint, PublicKey: pub,
	})
	if err != nil {
		f.t.Fatalf("registering %q: %v", name, err)
	}
	return res
}

// eventsOfType returns the payloads of every event of one type, in order.
func (f *fixture) eventsOfType(eventType string) []map[string]any {
	f.t.Helper()
	rows, err := f.db.Reader().QueryContext(context.Background(),
		`SELECT payload FROM events WHERE type = ? ORDER BY seq ASC`, eventType)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			f.t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, payload)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func TestRegisterPinsTheKeyAndLookupFindsIt(t *testing.T) {
	f := newFixture(t)
	pub := key(t)

	res := f.register("peer-b", pub, "https://b.example:8385")
	if res.Transition != membership.TransitionEnrolled {
		t.Errorf("transition = %q, want %q", res.Transition, membership.TransitionEnrolled)
	}
	if !res.Member.PublicKey.Equal(pub) {
		t.Error("the stored public key is not the one that was registered")
	}
	if res.Member.EnrolledAt != fixedTime {
		t.Errorf("enrolled_at = %v, want %v", res.Member.EnrolledAt, fixedTime)
	}

	// Byte-for-byte: what the other node reports as its own key is what this
	// node pinned. A rendering round trip that lost a byte would still look
	// right in a table.
	found, err := f.store.Lookup(context.Background(), pub)
	if err != nil {
		t.Fatalf("looking up a registered peer: %v", err)
	}
	if !found.PublicKey.Equal(pub) {
		t.Error("Lookup returned a different key to the one registered")
	}
	if found.PeerID != res.Member.PeerID {
		t.Errorf("Lookup found peer %s, want %s", found.PeerID, res.Member.PeerID)
	}

	// And it is a member by the boolean the request guard asks for.
	member, err := f.store.IsMember(context.Background(), pub)
	if err != nil {
		t.Fatal(err)
	}
	if !member {
		t.Error("a registered peer is not a member")
	}

	// The transition landed in the payload of the one type (§76, M4-15).
	registered := f.eventsOfType(events.TypePeerRegistered)
	// Two: the self peer at fixture setup, then this enrolment.
	if len(registered) != 2 {
		t.Fatalf("peer.registered emitted %d times, want 2 (self, then peer-b)", len(registered))
	}
	last := registered[1]
	if last["transition"] != events.PeerTransitionEnrolled {
		t.Errorf("transition in the payload = %v, want %q", last["transition"], events.PeerTransitionEnrolled)
	}
	if last["public_key"] != identity.FormatPublicKey(pub) {
		t.Errorf("public_key in the payload = %v, want %q", last["public_key"], identity.FormatPublicKey(pub))
	}
	if last["is_self"] != false {
		t.Errorf("is_self in the payload = %v, want false", last["is_self"])
	}
}

// TestEachRefusalIsItsOwn is one case per refusal, deliberately.
//
// A single "invalid input is rejected" case would pass with four of the five
// checks deleted, and each of these calls for a different action from the
// operator who hit it: a mistyped key, the wrong site's key, a name already in
// use, and two ways of trying to be this node.
func TestEachRefusalIsItsOwn(t *testing.T) {
	shared := key(t)

	cases := []struct {
		name string
		// setup runs before the refusal, and is where the state that makes it
		// a refusal is established.
		setup func(f *fixture)
		// act performs the operation that must be refused.
		act func(f *fixture) error
		// want is the sentinel the refusal must be.
		want error
		// mentions are substrings the message must carry, so the operator is
		// told what was wrong rather than that something was.
		mentions []string
	}{
		{
			name:  "a public key of the wrong length",
			setup: func(*fixture) {},
			act: func(f *fixture) error {
				_, err := f.store.Register(context.Background(), membership.Registration{
					Name: "peer-b", PublicKey: []byte("far too short"),
				})
				return err
			},
			want:     membership.ErrMalformedKey,
			mentions: []string{"13 bytes", "32"},
		},
		{
			name:  "no public key at all",
			setup: func(*fixture) {},
			act: func(f *fixture) error {
				// The trust-on-first-use shape: a name and an address, and a
				// hope that the machine answering is the right one.
				_, err := f.store.Register(context.Background(), membership.Registration{
					Name: "peer-b", Endpoint: "https://b.example:8385",
				})
				return err
			},
			want:     membership.ErrMalformedKey,
			mentions: []string{"registered by its public key"},
		},
		{
			name:  "a key already registered to a different peer",
			setup: func(f *fixture) { f.register("peer-b", shared, "https://b.example:8385") },
			act: func(f *fixture) error {
				_, err := f.store.Register(context.Background(), membership.Registration{
					Name: "peer-c", PublicKey: shared,
				})
				return err
			},
			want:     membership.ErrKeyRegistered,
			mentions: []string{"peer-b", "peer-c"},
		},
		{
			name:  "a name already taken by a peer with another key",
			setup: func(f *fixture) { f.register("peer-b", shared, "https://b.example:8385") },
			act: func(f *fixture) error {
				_, err := f.store.Register(context.Background(), membership.Registration{
					Name: "peer-b", PublicKey: key(t),
				})
				return err
			},
			want:     membership.ErrNameTaken,
			mentions: []string{"peer-b"},
		},
		{
			name:  "a second peer claiming to be this node",
			setup: func(*fixture) {},
			act: func(f *fixture) error {
				_, err := f.store.Register(context.Background(), membership.Registration{
					Name: "impostor", PublicKey: key(t), IsSelf: true,
				})
				return err
			},
			want:     membership.ErrSelfExists,
			mentions: []string{"this-node", "ADR-0010"},
		},
		{
			name:  "removing this node",
			setup: func(*fixture) {},
			act: func(f *fixture) error {
				_, err := f.store.Remove(context.Background(), "this-node")
				return err
			},
			want:     membership.ErrSelfRemoval,
			mentions: []string{"this-node"},
		},
		{
			name:  "removing a peer that was never registered",
			setup: func(*fixture) {},
			act: func(f *fixture) error {
				_, err := f.store.Remove(context.Background(), "never-heard-of-it")
				return err
			},
			want:     membership.ErrUnknownPeer,
			mentions: []string{"never-heard-of-it"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.setup(f)
			err := tc.act(f)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			for _, want := range tc.mentions {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q: %v", want, err)
				}
			}
		})
	}
}

// TestTheSchemaRefusesTwoPeersUnderOneKey is the same rule as
// ErrKeyRegistered, one layer down.
//
// The Go check reads first and refuses; this asserts the database would refuse
// anyway. That matters because a peers row can arrive without going through
// Register — a restore, a repair by hand, a future migration — and a trust
// root enforced only in the code path everybody remembers is enforced nowhere.
func TestTheSchemaRefusesTwoPeersUnderOneKey(t *testing.T) {
	f := newFixture(t)
	pub := key(t)
	f.register("peer-b", pub, "https://b.example:8385")

	_, err := f.db.Writer().ExecContext(context.Background(), `
		INSERT INTO peers (id, name, site, mode, public_key, is_self, enrolled_at, created_at)
		VALUES ('01990000-0000-7000-8000-0000000000cc', 'peer-c', '', 'full', ?, 0, ?, ?)`,
		[]byte(pub), fixedTime.Format(time.RFC3339Nano), fixedTime.Format(time.RFC3339Nano))
	if err == nil {
		t.Fatal("SQLite accepted a second peer holding an already-pinned public key")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Errorf("error = %v, want a unique constraint violation", err)
	}
}

// TestAnEndpointMovesWithoutTouchingIdentity is the other half of "a peer is
// registered by its public key, not by its address".
func TestAnEndpointMovesWithoutTouchingIdentity(t *testing.T) {
	f := newFixture(t)
	pub := key(t)
	first := f.register("peer-b", pub, "https://b.example:8385")

	moved, err := f.store.Register(context.Background(), membership.Registration{
		Name: "peer-b", Site: "site-b", Endpoint: "https://b2.example:8385", PublicKey: pub,
	})
	if err != nil {
		t.Fatal(err)
	}
	if moved.Transition != membership.TransitionEndpointChanged {
		t.Errorf("transition = %q, want %q", moved.Transition, membership.TransitionEndpointChanged)
	}
	if moved.Member.PeerID != first.Member.PeerID {
		t.Errorf("the peer id changed: %s -> %s", first.Member.PeerID, moved.Member.PeerID)
	}
	if moved.Member.EnrolledAt != first.Member.EnrolledAt {
		t.Errorf("enrolled_at changed: %v -> %v", first.Member.EnrolledAt, moved.Member.EnrolledAt)
	}
	if !moved.Member.PublicKey.Equal(pub) {
		t.Error("the pinned key changed when only the endpoint should have")
	}
	if moved.Member.Endpoint != "https://b2.example:8385" {
		t.Errorf("endpoint = %q, want the new one", moved.Member.Endpoint)
	}

	// The identity is what makes the peer findable, and it still does.
	found, err := f.store.Lookup(context.Background(), pub)
	if err != nil {
		t.Fatalf("the peer became unfindable by its key after moving: %v", err)
	}
	if found.PeerID != first.Member.PeerID {
		t.Errorf("Lookup now finds %s, want %s", found.PeerID, first.Member.PeerID)
	}

	// One type, transition in the payload.
	registered := f.eventsOfType(events.TypePeerRegistered)
	if len(registered) != 3 {
		t.Fatalf("peer.registered emitted %d times, want 3 (self, enrolled, endpoint_changed)", len(registered))
	}
	if registered[2]["transition"] != events.PeerTransitionEndpointChanged {
		t.Errorf("transition = %v, want %q", registered[2]["transition"], events.PeerTransitionEndpointChanged)
	}

	// Re-asserting what is already recorded is not a state transition and
	// emits nothing (invariant 7 is about transitions, not about calls).
	same, err := f.store.Register(context.Background(), membership.Registration{
		Name: "peer-b", Site: "site-b", Endpoint: "https://b2.example:8385", PublicKey: pub,
	})
	if err != nil {
		t.Fatal(err)
	}
	if same.Transition != membership.TransitionUnchanged {
		t.Errorf("transition = %q, want %q", same.Transition, membership.TransitionUnchanged)
	}
	if got := len(f.eventsOfType(events.TypePeerRegistered)); got != 3 {
		t.Errorf("a no-op re-registration emitted an event: %d events, want 3", got)
	}
}

// TestTheStoreStoresTheEndpointItIsGiven is the deliberate NON-rule (#169).
//
// What an operator may type is checked at the boundary that reads what they
// typed — `peers add` and POST /api/v1/peers — not in the single writer. Moving
// it in here would make every internal caller satisfy a rule about operator
// input: the health prober's tests (§31) construct membership around httptest
// servers, which are plain HTTP by construction, and would have to grow
// certificates to say anything about liveness.
func TestTheStoreStoresTheEndpointItIsGiven(t *testing.T) {
	f := newFixture(t)
	// The shape the health package builds: a plain-HTTP httptest address.
	got := f.register("peer-b", key(t), "http://127.0.0.1:44471")
	if got.Member.Endpoint != "http://127.0.0.1:44471" {
		t.Fatalf("endpoint = %q, want it stored verbatim", got.Member.Endpoint)
	}
}

// TestTheStoreTrimsButDoesNotRewrite: whitespace is not a value, and a
// re-registration with the same endpoint is not a move.
func TestTheStoreTrimsButDoesNotRewrite(t *testing.T) {
	f := newFixture(t)
	pub := key(t)
	got := f.register("peer-b", pub, "  https://b.example:8385  ")
	if got.Member.Endpoint != "https://b.example:8385" {
		t.Fatalf("endpoint = %q, want it trimmed", got.Member.Endpoint)
	}
	again, err := f.store.Register(context.Background(), membership.Registration{
		Name: "peer-b", Site: "site-b", Endpoint: "https://b.example:8385", PublicKey: pub,
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Transition != membership.TransitionUnchanged {
		t.Errorf("transition = %q, want %q", again.Transition, membership.TransitionUnchanged)
	}
}

// TestRemoveIsRevocation asserts the ADR-0012 sentence at the store level:
// membership is the record, so deleting the record withdraws the trust. The
// end-to-end version — a peer that was reading bytes and then cannot — is in
// revocation_test.go.
func TestRemoveIsRevocation(t *testing.T) {
	f := newFixture(t)
	pub := key(t)
	enrolled := f.register("peer-b", pub, "https://b.example:8385")

	// Reproduce the working case first. Asserting only the failure would pass
	// on a store where Lookup never found anything.
	if member, err := f.store.IsMember(context.Background(), pub); err != nil || !member {
		t.Fatalf("before removal: member = %v, err = %v; want true, nil", member, err)
	}

	removed, err := f.store.Remove(context.Background(), "peer-b")
	if err != nil {
		t.Fatal(err)
	}
	if removed.PeerID != enrolled.Member.PeerID {
		t.Errorf("removed peer %s, want %s", removed.PeerID, enrolled.Member.PeerID)
	}

	member, err := f.store.IsMember(context.Background(), pub)
	if err != nil {
		t.Fatal(err)
	}
	if member {
		t.Error("a removed peer is still a member")
	}
	if _, err := f.store.Lookup(context.Background(), pub); !errors.Is(err, membership.ErrNotAMember) {
		t.Errorf("Lookup error = %v, want ErrNotAMember", err)
	}

	// The row is gone, not flagged. There is no revocation list in this
	// design, so a row that still existed would still be trust.
	var count int
	if err := f.db.Reader().QueryRowContext(context.Background(),
		`SELECT count(*) FROM peers WHERE id = ?`, removed.PeerID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("the peers row survived removal: %d rows", count)
	}

	removedEvents := f.eventsOfType(events.TypePeerRemoved)
	if len(removedEvents) != 1 {
		t.Fatalf("peer.removed emitted %d times, want 1", len(removedEvents))
	}
	if removedEvents[0]["transition"] != events.PeerTransitionRemoved {
		t.Errorf("transition = %v, want %q", removedEvents[0]["transition"], events.PeerTransitionRemoved)
	}
	if removedEvents[0]["public_key"] != identity.FormatPublicKey(pub) {
		t.Errorf("the removal event does not name the key that was revoked: %v", removedEvents[0]["public_key"])
	}
}

// TestRemovingAPeerTakesItsReplicasWithIt: a peer this instance will not talk
// to is not a peer whose copy counts towards placement.
func TestRemovingAPeerTakesItsReplicasWithIt(t *testing.T) {
	f := newFixture(t)
	pub := key(t)
	enrolled := f.register("peer-b", pub, "https://b.example:8385")

	const hash = "blake3:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	ctx := context.Background()
	mustExec(t, f.db.Writer(), `INSERT INTO blobs (hash, size, first_seen_at) VALUES (?, 7, ?)`,
		hash, fixedTime.Format(time.RFC3339Nano))
	mustExec(t, f.db.Writer(), `INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, updated_at)
		VALUES (?, ?, 'present', 7, ?)`, hash, enrolled.Member.PeerID, fixedTime.Format(time.RFC3339Nano))

	var before int
	if err := f.db.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM replicas WHERE peer_id = ?`, enrolled.Member.PeerID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 1 {
		t.Fatalf("the replica row was not seeded: %d rows", before)
	}

	if _, err := f.store.Remove(ctx, "peer-b"); err != nil {
		t.Fatal(err)
	}

	var after int
	if err := f.db.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM replicas WHERE peer_id = ?`, enrolled.Member.PeerID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Errorf("a removed peer still holds %d replica rows", after)
	}
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// TestBothSidesSeeTwoPeers is the acceptance shape: node A registers node B's
// key and node B registers node A's, and each ends up with two peers whose
// public keys match byte-for-byte what the other reports as its own.
func TestBothSidesSeeTwoPeers(t *testing.T) {
	a := newFixture(t)
	b := newFixture(t)

	// Each node's own key, established the way M4-03 establishes it.
	aKey, bKey := key(t), key(t)
	recordSelfKey(t, a, aKey)
	recordSelfKey(t, b, bKey)

	a.register("peer-b", bKey, "https://b.example:8385")
	b.register("peer-a", aKey, "https://a.example:8385")

	for _, tc := range []struct {
		name  string
		side  *fixture
		other ed25519.PublicKey
	}{
		{"node A", a, bKey},
		{"node B", b, aKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			members, err := tc.side.store.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(members) != 2 {
				t.Fatalf("%d peers, want 2", len(members))
			}
			found, err := tc.side.store.Lookup(context.Background(), tc.other)
			if err != nil {
				t.Fatalf("the other node's key is not a member here: %v", err)
			}
			if !found.PublicKey.Equal(tc.other) {
				t.Error("the pinned key is not byte-identical to what the other node reports as its own")
			}
		})
	}
}

func recordSelfKey(t *testing.T, f *fixture, pub ed25519.PublicKey) {
	t.Helper()
	mustExec(t, f.db.Writer(),
		`UPDATE peers SET public_key = ?, key_algo = 'ed25519' WHERE is_self = 1`, []byte(pub))
}

// TestNothingHereCarriesPrivateKeyMaterial scans what this package hands out.
//
// It is a scan of the values rather than a review of the struct, because the
// field that leaks a key is the one somebody added after the review.
func TestNothingHereCarriesPrivateKeyMaterial(t *testing.T) {
	f := newFixture(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	res := f.register("peer-b", pub, "https://b.example:8385")

	rendered, err := json.Marshal(res.Member)
	if err != nil {
		t.Fatal(err)
	}
	blob := string(rendered) + strings.Join(payloadStrings(f.eventsOfType(events.TypePeerRegistered)), " ")

	// The positive control: the PUBLIC key really is in there. Without it this
	// test would pass on an empty string.
	if !strings.Contains(blob, identity.FormatPublicKey(pub)) &&
		!strings.Contains(blob, encodeBase64(pub)) {
		t.Fatalf("the public key is not in the output at all, so its absence proves nothing:\n%s", blob)
	}
	for _, secret := range []string{encodeBase64(priv.Seed()), encodeBase64(priv), encodeHex(priv.Seed())} {
		if strings.Contains(blob, secret) {
			t.Errorf("private key material appears in the output")
		}
	}
}

func payloadStrings(payloads []map[string]any) []string {
	out := make([]string, 0, len(payloads))
	for _, p := range payloads {
		buf, _ := json.Marshal(p)
		out = append(out, string(buf))
	}
	return out
}

func encodeBase64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
func encodeHex(b []byte) string    { return hex.EncodeToString(b) }
