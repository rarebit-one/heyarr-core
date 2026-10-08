package deviceauth_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/enrolment"

	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
)

// A key registered as an executor's service recipient is never also a member's
// (ADR-0104): enrolling a user with it as the recovery key, re-keying to it, or
// enrolling a device whose encryption key it is, is refused in the same
// transaction as the write. The personal-state store refuses the reverse.
func TestAServiceRecipientKeyIsNeverAMembersKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	svc := x25519Pub(t)
	ts := now.Format(time.RFC3339Nano)
	for _, q := range []string{
		`INSERT INTO principals (id, kind, name, created_at) VALUES ('p-exe', 'executor', 'runner', '` + ts + `')`,
		`INSERT INTO service_recipients (id, principal_id, recipient, fingerprint, registered_by_principal,
		 registered_by_device, created_at) VALUES ('r-1', 'p-exe', '` + svc + `', 'FP', 'u', 'd', '` + ts + `')`,
	} {
		if _, err := f.db.Writer().ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	a := newActor(t)
	if _, err := f.store.EnrolUser(ctx, a.userKey, "alice", svc); !errors.Is(err, deviceauth.ErrKeyIsServiceRecipient) {
		t.Errorf("enrolling a user with a service key as its recovery key: %v", err)
	}
	user, err := f.store.EnrolUser(ctx, a.userKey, "alice", oldRecovery)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RekeyUser(ctx, user.PrincipalID, newUserKey(t), svc); !errors.Is(err, deviceauth.ErrKeyIsServiceRecipient) {
		t.Errorf("re-keying to a service key: %v", err)
	}

	devPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := enrolment.SignCert(a.userPriv, devPub, svc, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EnrolDevice(ctx, cert, "laptop"); !errors.Is(err, deviceauth.ErrKeyIsServiceRecipient) {
		t.Errorf("enrolling a device whose encryption key is a service key: %v", err)
	}
	// Even by reconciliation, the row is never materialised onto the key.
	if err := f.store.RecordOps(ctx, a.userKey, []string{cert}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM device_identities WHERE encryption_key = ?`, svc); n != 0 {
		t.Errorf("%d device row(s) carry a service recipient's key", n)
	}
	allowed, err := f.store.AllowedWrapRecipients(ctx)
	if err != nil || allowed[svc] {
		t.Errorf("a service key became a member wrap target: %v %v", allowed[svc], err)
	}
}
