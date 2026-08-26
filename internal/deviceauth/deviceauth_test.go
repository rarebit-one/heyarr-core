package deviceauth_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }

var now = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

type fixture struct {
	store *deviceauth.Store
	clock *fixedClock
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
	clock := &fixedClock{t: now}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	store, err := deviceauth.New(deviceauth.Options{
		Writer: db.Writer(), Reader: db.Reader(), Events: log, Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{store: store, clock: clock}
}

// a user identity and a device, with helpers to mint a cert and a full device
// credential ("<cert>~<possession>").
type actor struct {
	userPriv   ed25519.PrivateKey
	userKey    string
	devicePriv ed25519.PrivateKey
	deviceKey  string
	cert       string
}

func newActor(t *testing.T) *actor {
	t.Helper()
	u, upriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	dpub, dpriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := enrolment.SignCert(upriv, dpub, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	return &actor{
		userPriv: upriv, userKey: u.UserID(),
		devicePriv: dpriv, deviceKey: identity.FormatPublicKey(dpub), cert: cert,
	}
}

// credential builds "<cert>~<possession>" for a moment, the value the middleware
// hands to Verify.
func (a *actor) credential(t *testing.T, at time.Time) string {
	t.Helper()
	proof, err := enrolment.SignPossession(a.devicePriv, a.cert, at, 0)
	if err != nil {
		t.Fatal(err)
	}
	return a.cert + "~" + proof
}

// enrol pins the user and enrols the device, the ordinary setup.
func (f *fixture) enrol(t *testing.T, a *actor) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.store.EnrolUser(ctx, a.userKey, "alice"); err != nil {
		t.Fatalf("enrol user: %v", err)
	}
	if _, err := f.store.EnrolDevice(ctx, a.cert, "phone"); err != nil {
		t.Fatalf("enrol device: %v", err)
	}
}

func TestDeviceAuthenticatesAsItsUser(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := newActor(t)
	f.enrol(t, a)

	got, err := f.store.Verify(context.Background(), a.credential(t, now), now)
	if err != nil {
		t.Fatalf("a valid device was refused: %v", err)
	}
	if got.UserKey != a.userKey || got.DeviceKey != a.deviceKey {
		t.Fatalf("resolved keys wrong: %+v", got)
	}
	if got.PrincipalID == "" || got.PrincipalName != "alice" {
		t.Fatalf("resolved principal wrong: %+v", got)
	}
}

// The same cert and a fresh proof authenticate against a SECOND, independent
// store that has also pinned the user — a device is its user's on either peer
// (§40), with no token issued by either.
func TestSameDeviceAuthenticatesAtEitherPeer(t *testing.T) {
	t.Parallel()
	a := newActor(t)
	peerA, peerB := newFixture(t), newFixture(t)
	peerA.enrol(t, a)
	peerB.enrol(t, a)

	for name, f := range map[string]*fixture{"peer-a": peerA, "peer-b": peerB} {
		got, err := f.store.Verify(context.Background(), a.credential(t, now), now)
		if err != nil {
			t.Fatalf("%s refused a valid device: %v", name, err)
		}
		if got.UserKey != a.userKey || got.DeviceKey != a.deviceKey {
			t.Fatalf("%s resolved the wrong identity: %+v", name, got)
		}
	}
}

// Every refusal in the chain, each by its own error. The whole point of ADR-0048
// is that a grant/cert is not a bearer token with extra fields, so each check is
// proven to actually refuse.
func TestVerifyRefusalsAreDistinct(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("unknown user", func(t *testing.T) {
		f := newFixture(t)
		a := newActor(t) // never enrolled
		_, err := f.store.Verify(ctx, a.credential(t, now), now)
		if !errors.Is(err, deviceauth.ErrUnknownUser) {
			t.Fatalf("want ErrUnknownUser, got %v", err)
		}
	})

	t.Run("expired cert", func(t *testing.T) {
		f := newFixture(t)
		a := newActor(t)
		f.enrol(t, a)
		// enrolment.CertLifetime past issue.
		later := now.Add(enrolment.CertLifetime + time.Hour)
		_, err := f.store.Verify(ctx, a.credential(t, later), later)
		if !errors.Is(err, enrolment.ErrExpired) {
			t.Fatalf("want enrolment.ErrExpired, got %v", err)
		}
	})

	t.Run("cert signed by the wrong key", func(t *testing.T) {
		f := newFixture(t)
		a := newActor(t)
		if _, err := f.store.EnrolUser(ctx, a.userKey, "alice"); err != nil {
			t.Fatal(err)
		}
		// A cert that CLAIMS a's user but is signed by an impostor.
		_, impostor, _ := ed25519.GenerateKey(nil)
		devicePub := a.devicePriv.Public().(ed25519.PublicKey)
		forged, err := enrolment.SignCert(impostor, devicePub, now, 0)
		if err != nil {
			t.Fatal(err)
		}
		// Repoint its user field to a's user id so lookup finds a's pinned key.
		forged = repointCertUser(t, forged, a.userKey)
		proof, _ := enrolment.SignPossession(a.devicePriv, forged, now, 0)
		_, err = f.store.Verify(ctx, forged+"~"+proof, now)
		if !errors.Is(err, enrolment.ErrBadSignature) {
			t.Fatalf("want enrolment.ErrBadSignature, got %v", err)
		}
	})

	t.Run("device not enrolled", func(t *testing.T) {
		f := newFixture(t)
		a := newActor(t)
		if _, err := f.store.EnrolUser(ctx, a.userKey, "alice"); err != nil {
			t.Fatal(err)
		}
		// User pinned, device never enrolled.
		_, err := f.store.Verify(ctx, a.credential(t, now), now)
		if !errors.Is(err, deviceauth.ErrUnknownDevice) {
			t.Fatalf("want ErrUnknownDevice, got %v", err)
		}
	})

	t.Run("device revoked", func(t *testing.T) {
		f := newFixture(t)
		a := newActor(t)
		f.enrol(t, a)
		if _, err := f.store.RevokeDevice(ctx, a.deviceKey); err != nil {
			t.Fatal(err)
		}
		_, err := f.store.Verify(ctx, a.credential(t, now), now)
		if !errors.Is(err, deviceauth.ErrDeviceRevoked) {
			t.Fatalf("want ErrDeviceRevoked, got %v", err)
		}
	})

	t.Run("no possession proof", func(t *testing.T) {
		f := newFixture(t)
		a := newActor(t)
		f.enrol(t, a)
		_, err := f.store.Verify(ctx, a.cert, now) // no "~<proof>"
		if !errors.Is(err, deviceauth.ErrMalformedCredential) {
			t.Fatalf("want ErrMalformedCredential, got %v", err)
		}
	})

	t.Run("possession by the wrong key", func(t *testing.T) {
		f := newFixture(t)
		a := newActor(t)
		f.enrol(t, a)
		_, impostor, _ := ed25519.GenerateKey(nil)
		badProof, _ := enrolment.SignPossession(impostor, a.cert, now, 0)
		_, err := f.store.Verify(ctx, a.cert+"~"+badProof, now)
		if !errors.Is(err, enrolment.ErrPossessionSignature) {
			t.Fatalf("want ErrPossessionSignature, got %v", err)
		}
	})

	t.Run("possession expired", func(t *testing.T) {
		f := newFixture(t)
		a := newActor(t)
		f.enrol(t, a)
		// A proof minted at `now` but verified after PossessionTTL.
		cred := a.credential(t, now)
		later := now.Add(enrolment.PossessionTTL + time.Minute)
		_, err := f.store.Verify(ctx, cred, later)
		if !errors.Is(err, enrolment.ErrPossessionExpired) {
			t.Fatalf("want ErrPossessionExpired, got %v", err)
		}
	})
}

// Revoking a user cascades to its devices and stops authentication.
func TestRevokeUserCascades(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	a := newActor(t)
	f.enrol(t, a)

	if _, err := f.store.RevokeUser(ctx, a.userKey); err != nil {
		t.Fatalf("revoke user: %v", err)
	}
	if _, err := f.store.Verify(ctx, a.credential(t, now), now); !errors.Is(err, deviceauth.ErrUnknownUser) {
		t.Fatalf("after user revocation want ErrUnknownUser, got %v", err)
	}
	// The device row went with it (cascade), so the device is unknown too.
	if _, err := f.store.LookupDevice(ctx, a.deviceKey); !errors.Is(err, deviceauth.ErrUnknownDevice) {
		t.Fatalf("cascade should have removed the device, got %v", err)
	}
}

// A device key already enrolled cannot be enrolled again, and a user key already
// pinned cannot be pinned again.
func TestEnrolIsIdempotentlyRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	a := newActor(t)
	f.enrol(t, a)

	if _, err := f.store.EnrolUser(ctx, a.userKey, "alice-again"); !errors.Is(err, deviceauth.ErrUserExists) {
		t.Fatalf("re-pinning a user want ErrUserExists, got %v", err)
	}
	if _, err := f.store.EnrolDevice(ctx, a.cert, "phone-again"); !errors.Is(err, deviceauth.ErrDeviceExists) {
		t.Fatalf("re-enrolling a device want ErrDeviceExists, got %v", err)
	}
}

func TestEnrolDeviceRefusesUnpinnedUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	a := newActor(t) // user never pinned
	if _, err := f.store.EnrolDevice(ctx, a.cert, "phone"); !errors.Is(err, deviceauth.ErrUnknownUser) {
		t.Fatalf("want ErrUnknownUser, got %v", err)
	}
}

func TestEnrolUserRefusesMalformedKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.store.EnrolUser(ctx, "not-a-key", "x"); !errors.Is(err, deviceauth.ErrMalformedKey) {
		t.Fatalf("want ErrMalformedKey, got %v", err)
	}
}

// repointCertUser rewrites a cert's user field without re-signing — a forgery
// helper, to prove the signature (not the claimed user) is the authority.
func repointCertUser(t *testing.T, cert, newUser string) string {
	t.Helper()
	// The cert is base64url(json).base64url(sig); decode, edit "usr", re-encode
	// the body, keep the (now invalid) signature. We reuse the enrolment package
	// only through its public API, so do the surgery by string replacement on the
	// decoded JSON.
	out, err := rewriteCertUser(cert, newUser)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func rewriteCertUser(cert, newUser string) (string, error) {
	bodyEnc, sigEnc, ok := strings.Cut(cert, ".")
	if !ok {
		return "", fmt.Errorf("cert has no signature")
	}
	body, err := base64.RawURLEncoding.DecodeString(bodyEnc)
	if err != nil {
		return "", err
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return "", err
	}
	m["usr"] = newUser
	reencoded, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(reencoded) + "." + sigEnc, nil
}
