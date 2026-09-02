package deviceauth_test

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/voidbind-go/rp"

	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/enrolment"
)

// The voidbind-go membership vectors (testdata/vectors/membership, ADR-0007),
// replayed through the STORE rather than through enrolment.Evaluate directly:
// every op is recorded into membership_ops, read back, and evaluated, and the
// device_identities view the store materialises must agree with the vector's
// expected members and removals. A vector that passes in voidbind-go and
// fails here is a defect in the store's persistence or reconciliation, never a
// "flaky key". Copied verbatim from voidbind-go v0.9.0; regenerate there with
// `go test ./enrolment -run TestVectors -update` and re-copy.

type vector struct {
	Name string `json:"name"`
	User string `json:"usr"`
	Now  int64  `json:"now"`
	Keys map[string]struct {
		SignSeed string `json:"sign_seed"`
		EncSeed  string `json:"enc_seed"`
		ID       string `json:"id"`
	} `json:"keys"`
	Ops []struct {
		Label string `json:"label"`
		Token string `json:"token"`
		Hash  string `json:"hash"`
	} `json:"ops"`
	Expect struct {
		Members map[string]struct {
			AdmittedBy string `json:"admitted_by"`
			DeviceEnc  string `json:"denc"`
			AdmittedAt int64  `json:"admitted_at"`
			Expires    int64  `json:"expires"`
		} `json:"members"`
		Removed     []string          `json:"removed"`
		Heads       []string          `json:"heads"`
		Rejected    map[string]string `json:"rejected"`
		Ineffective map[string]string `json:"ineffective"`
	} `json:"expect"`
}

func loadVectors(t *testing.T) []vector {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "vectors", "membership", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no vectors found: %v", err)
	}
	var out []vector
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var v vector
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if v.Name != strings.TrimSuffix(filepath.Base(f), ".json") {
			t.Fatalf("%s: name %q does not match the file", f, v.Name)
		}
		out = append(out, v)
	}
	return out
}

func (v vector) token(label string) string {
	for _, o := range v.Ops {
		if o.Label == label {
			return o.Token
		}
	}
	panic("no op labelled " + label)
}

func (v vector) tokens() []string {
	out := make([]string, 0, len(v.Ops))
	for _, o := range v.Ops {
		out = append(out, o.Token)
	}
	return out
}

func (v vector) signer(label string) ed25519.PrivateKey {
	seed, err := hex.DecodeString(v.Keys[label].SignSeed)
	if err != nil {
		panic(err)
	}
	return ed25519.NewKeyFromSeed(seed)
}

// fixtureAt is a fixture whose clock is the vector's evaluation instant.
func fixtureAt(t *testing.T, v vector) (*fixture, time.Time) {
	t.Helper()
	f := newFixture(t)
	at := time.Unix(v.Now, 0).UTC()
	f.clock.t = at
	if _, err := f.store.EnrolUser(context.Background(), v.User, v.Name); err != nil {
		t.Fatal(err)
	}
	return f, at
}

// accepted mirrors what every writer into the log does first (rp.Verifier and
// POST /membership alike): evaluate, then hand the store only the structurally
// valid ops. The store refuses to record junk by contract.
func accepted(t *testing.T, usr string, tokens []string, at time.Time) []string {
	t.Helper()
	view, err := enrolment.Evaluate(usr, tokens, at)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, tok := range tokens {
		if _, ok := view.Accepted[enrolment.OpHash(tok)]; ok {
			out = append(out, tok)
		}
	}
	return out
}

func TestVectorsReplayThroughTheStore(t *testing.T) {
	t.Parallel()
	for _, v := range loadVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f, at := fixtureAt(t, v)

			// Record in the vector's (deliberately non-causal) order, then read back.
			ops := accepted(t, v.User, v.tokens(), at)
			if err := f.store.RecordOps(ctx, v.User, ops); err != nil {
				t.Fatalf("record: %v", err)
			}
			// Idempotent: recording the same set again changes nothing.
			if err := f.store.RecordOps(ctx, v.User, ops); err != nil {
				t.Fatalf("re-record: %v", err)
			}
			stored, err := f.store.Ops(ctx, v.User)
			if err != nil {
				t.Fatal(err)
			}
			if len(stored) != len(ops) {
				t.Fatalf("stored %d ops, recorded %d", len(stored), len(ops))
			}
			view, err := enrolment.Evaluate(v.User, stored, at)
			if err != nil {
				t.Fatal(err)
			}

			// The evaluation over what the store holds is the vector's.
			for dev, want := range v.Expect.Members {
				m, ok := view.Members[dev]
				if !ok {
					t.Errorf("member %s missing", dev)
					continue
				}
				if m.AdmittedBy != want.AdmittedBy || m.DeviceEnc != want.DeviceEnc ||
					m.AdmittedAt.Unix() != want.AdmittedAt || m.ExpiresAt.Unix() != want.Expires {
					t.Errorf("member %s = %+v, want %+v", dev, m, want)
				}
			}
			if len(view.Members) != len(v.Expect.Members) {
				t.Errorf("members = %d, want %d", len(view.Members), len(v.Expect.Members))
			}
			var removed []string
			for dev := range view.Removed {
				removed = append(removed, dev)
			}
			sort.Strings(removed)
			if strings.Join(removed, ",") != strings.Join(v.Expect.Removed, ",") {
				t.Errorf("removed = %v, want %v", removed, v.Expect.Removed)
			}
			if strings.Join(view.Heads, ",") != strings.Join(v.Expect.Heads, ",") {
				t.Errorf("heads = %v, want %v", view.Heads, v.Expect.Heads)
			}
			for hash, reason := range v.Expect.Ineffective {
				if got := view.Ineffective[hash]; string(got) != reason {
					t.Errorf("ineffective %s = %q, want %q", hash, got, reason)
				}
			}
			// Rejected ops were never recorded.
			for hash := range v.Expect.Rejected {
				for _, tok := range stored {
					if enrolment.OpHash(tok) == hash {
						t.Errorf("rejected op %s was recorded", hash)
					}
				}
			}

			// And the materialised view agrees: a row per member, holding the
			// admitting op and the encryption key it binds; a removed device that
			// has a row is tombstoned.
			for dev, want := range v.Expect.Members {
				d, err := f.store.LookupDevice(ctx, dev)
				if err != nil {
					t.Errorf("member %s has no row: %v", dev, err)
					continue
				}
				if enrolment.OpHash(d.Cert) != want.AdmittedBy || d.EncryptionKey != want.DeviceEnc || d.RevokedAt != nil ||
					d.ExpiresAt.Unix() != want.Expires {
					t.Errorf("row for %s = %+v, want admitted_by %s denc %s", dev, d, want.AdmittedBy, want.DeviceEnc)
				}
			}
			for _, dev := range v.Expect.Removed {
				d, err := f.store.LookupDevice(ctx, dev)
				if errors.Is(err, deviceauth.ErrUnknownDevice) {
					continue // never a member in the final view: no row was ever built
				}
				if err != nil {
					t.Fatal(err)
				}
				if d.RevokedAt == nil {
					t.Errorf("removed device %s is not tombstoned", dev)
				}
			}
		})
	}
}

// credentialFor builds "<admitting op>~<possession>" for a vector device.
func credentialFor(t *testing.T, v vector, label, opLabel string, at time.Time) string {
	t.Helper()
	tok := v.token(opLabel)
	proof, err := enrolment.SignPossession(v.signer(label), tok, at, 0)
	if err != nil {
		t.Fatal(err)
	}
	return tok + "~" + proof
}

func vectorNamed(t *testing.T, name string) vector {
	t.Helper()
	for _, v := range loadVectors(t) {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no vector %q", name)
	return vector{}
}

// A device admitted by ANOTHER device (not genesis) authenticates on first
// contact — provided it presents the ops the node has not seen (the
// Voidbind-Membership header) — and the view materialises it. Without them
// its admission cites a past the node cannot judge, and it is refused.
func TestDeviceAdmittedByAMemberAuthenticatesOnFirstContact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	v := vectorNamed(t, "genesis-a-b")
	f, at := fixtureAt(t, v)
	b := v.Keys["B"].ID
	credB := credentialFor(t, v, "B", "add-B", at)

	// Nothing but the pin: B's admission cites add-A, which this node has never seen.
	if _, err := f.store.Verify(ctx, credB, nil, at); !errors.Is(err, rp.ErrNotMember) {
		t.Fatalf("B without its evidence: want rp.ErrNotMember, got %v", err)
	}
	if _, err := f.store.LookupDevice(ctx, b); !errors.Is(err, deviceauth.ErrUnknownDevice) {
		t.Fatalf("a refused first contact materialised a row: %v", err)
	}

	// With the header: 200 on first contact, the row built, both ops recorded.
	got, err := f.store.Verify(ctx, credB, []string{v.token("add-A")}, at)
	if err != nil {
		t.Fatalf("B with its evidence: %v", err)
	}
	if got.DeviceKey != b || got.AdmittedBy != v.Expect.Members[b].AdmittedBy {
		t.Fatalf("authenticated = %+v", got)
	}
	d, err := f.store.LookupDevice(ctx, b)
	if err != nil || d.Cert != v.token("add-B") || d.EncryptionKey != v.Expect.Members[b].DeviceEnc {
		t.Fatalf("row = %+v, err = %v", d, err)
	}
	if ops, _ := f.store.Ops(ctx, v.User); len(ops) != 2 {
		t.Fatalf("recorded ops = %d, want 2", len(ops))
	}
	// Next time, no header needed: the node remembers.
	if _, err := f.store.Verify(ctx, credB, nil, at); err != nil {
		t.Fatalf("B on second contact: %v", err)
	}
	// And A — never itself in contact — is a member too.
	if _, err := f.store.Verify(ctx, credentialFor(t, v, "A", "add-A", at), nil, at); err != nil {
		t.Fatalf("A: %v", err)
	}
}

// A device a member removed is refused, and the node's view tombstones it —
// whether it learns of the remove from the removed device itself, from
// another device's header, or from a push.
func TestRemovedDeviceIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	v := vectorNamed(t, "a-removes-b")
	f, at := fixtureAt(t, v)
	b := v.Keys["B"].ID

	// B is admitted and reads.
	credB := credentialFor(t, v, "B", "add-B", at)
	if _, err := f.store.Verify(ctx, credB, []string{v.token("add-A")}, at); err != nil {
		t.Fatalf("B before the remove: %v", err)
	}
	// A removes B; the node learns it from a push (what POST /membership does).
	if err := f.store.RecordOps(ctx, v.User, []string{v.token("rm-B")}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Verify(ctx, credB, nil, at); !errors.Is(err, rp.ErrRemoved) {
		t.Fatalf("B after the remove: want rp.ErrRemoved, got %v", err)
	}
	d, err := f.store.LookupDevice(ctx, b)
	if err != nil || d.RevokedAt == nil {
		t.Fatalf("B's row after the remove = %+v, err = %v", d, err)
	}
	// A's re-add of B does not bring it back: remove wins unless genesis (ADR-0007).
	if err := f.store.RecordOps(ctx, v.User, []string{v.token("readd-B-by-A")}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Verify(ctx, credentialFor(t, v, "B", "readd-B-by-A", at), nil, at); !errors.Is(err, rp.ErrRemoved) {
		t.Fatalf("B on A's re-add: want rp.ErrRemoved, got %v", err)
	}
	// A is untouched.
	if _, err := f.store.Verify(ctx, credentialFor(t, v, "A", "add-A", at), nil, at); err != nil {
		t.Fatalf("A: %v", err)
	}
}

// A device enrolled before ADR-0068 holds a v1 or v2 cert. It is a genesis
// add: it authenticates unchanged, and BackfillLegacyCerts records it into the
// log so GET /membership speaks for it before it next calls.
func TestLegacyCertIsAGenesisAdd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	v := vectorNamed(t, "v2-cert-as-genesis-add")
	f, at := fixtureAt(t, v)

	for _, tc := range []struct{ label, op string }{{"A", "cert-A-v2"}, {"B", "cert-B-v1"}} {
		got, err := f.store.Verify(ctx, credentialFor(t, v, tc.label, tc.op, at), nil, at)
		if err != nil {
			t.Fatalf("%s: %v", tc.op, err)
		}
		if got.AdmittedBy != enrolment.OpHash(v.token(tc.op)) {
			t.Fatalf("%s admitted by %s", tc.op, got.AdmittedBy)
		}
	}
	// C, admitted by A under the v3 format, cites A's v2 cert as its prev.
	if _, err := f.store.Verify(ctx, credentialFor(t, v, "C", "add-C", at), nil, at); err != nil {
		t.Fatalf("add-C citing a v2 cert: %v", err)
	}

	// The backfill: a row enrolled by the pre-ADR-0068 admin path whose cert is
	// not yet in the log is recorded on startup, once.
	if _, err := f.db.Writer().ExecContext(ctx, `DELETE FROM membership_ops`); err != nil {
		t.Fatal(err)
	}
	n, err := f.store.BackfillLegacyCerts(ctx)
	if err != nil || n != 3 {
		t.Fatalf("backfill = %d, %v; want 3", n, err)
	}
	if n, err := f.store.BackfillLegacyCerts(ctx); err != nil || n != 0 {
		t.Fatalf("second backfill = %d, %v; want 0", n, err)
	}
	if ops, _ := f.store.Ops(ctx, v.User); len(ops) != 3 {
		t.Fatalf("ops after backfill = %d, want 3", len(ops))
	}
}

// The admin's tombstone outlives the log: a device RevokeDevice removed stays
// refused however many valid adds the log holds for it, and reconciliation
// never clears revoked_at.
func TestAdminTombstoneOutlivesTheLog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	v := vectorNamed(t, "genesis-a-b")
	f, at := fixtureAt(t, v)
	if err := f.store.RecordOps(ctx, v.User, v.tokens()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RevokeDevice(ctx, v.Keys["B"].ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Reconcile(ctx, v.User); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Verify(ctx, credentialFor(t, v, "B", "add-B", at), nil, at); !errors.Is(err, deviceauth.ErrDeviceRevoked) {
		t.Fatalf("want ErrDeviceRevoked, got %v", err)
	}
}
