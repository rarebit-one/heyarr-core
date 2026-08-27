package voidbindcompat_test

import (
	"crypto/ed25519"
	"testing"
	"time"

	henrol "github.com/rarebit-one/heyarr-core/internal/enrolment"
	hgrant "github.com/rarebit-one/heyarr-core/internal/grant"

	venrol "github.com/rarebit-one/voidbind-go/enrolment"
	vgrant "github.com/rarebit-one/voidbind-go/grant"
	vrp "github.com/rarebit-one/voidbind-go/rp"
)

// Golden vectors CAPTURED FROM PRE-MIGRATION heyarr-core — generated on
// origin/main BEFORE the identity packages became shims, with the fixed seeds
// below (user = 0x01×32, device = 0x02×32) and a fixed issue time. They are the
// load-bearing proof that this migration is a byte-identical dedup and not a
// wire change: the post-migration code (and voidbind-go directly) must reproduce
// and verify these EXACT bytes, and a cert/grant minted before the migration
// must still verify after it. If a golden constant ever has to change to make
// this file pass, the "dedup" broke a wire format — stop and investigate.
const (
	goldenUserID    = "ed25519:8a88e3dd7409f195fd52db2d3cba5d72ca6709bf1d94121bf3748801b40f6f5c"
	goldenDeviceKey = "ed25519:8139770ea87d175f56a35466c34c7ecccb8d8a91b4ee37a25df60f5b8fc9b394"
	goldenDeviceEnc = "x25519:0303030303030303030303030303030303030303030303030303030303030303"
	goldenResource  = "space/demo"

	goldenCert  = "eyJ2IjoyLCJ1c3IiOiJlZDI1NTE5OjhhODhlM2RkNzQwOWYxOTVmZDUyZGIyZDNjYmE1ZDcyY2E2NzA5YmYxZDk0MTIxYmYzNzQ4ODAxYjQwZjZmNWMiLCJkZXYiOiJlZDI1NTE5OjgxMzk3NzBlYTg3ZDE3NWY1NmEzNTQ2NmMzNGM3ZWNjY2I4ZDhhOTFiNGVlMzdhMjVkZjYwZjViOGZjOWIzOTQiLCJkZW5jIjoieDI1NTE5OjAzMDMwMzAzMDMwMzAzMDMwMzAzMDMwMzAzMDMwMzAzMDMwMzAzMDMwMzAzMDMwMzAzMDMwMzAzMDMwMzAzMDMiLCJpYXQiOjE3MDAwMDAwMDAsImV4cCI6MTcwNzc3NjAwMH0.yUnLKnvDZ8YtkgV9zf5eRrYHes5osqzzGlVHXcFSPiuIuVM0jmcdGH4qOQA-UCla_9qwSK7VPpXSfsTbSY_JBA"
	goldenGrant = "eyJ2IjoxLCJpc3MiOiJlZDI1NTE5OjhhODhlM2RkNzQwOWYxOTVmZDUyZGIyZDNjYmE1ZDcyY2E2NzA5YmYxZDk0MTIxYmYzNzQ4ODAxYjQwZjZmNWMiLCJwcm4iOiJlZDI1NTE5OjgxMzk3NzBlYTg3ZDE3NWY1NmEzNTQ2NmMzNGM3ZWNjY2I4ZDhhOTFiNGVlMzdhMjVkZjYwZjViOGZjOWIzOTQiLCJyZXMiOiJzcGFjZS9kZW1vIiwiY2FwIjpbInJlYWQiXSwiaWF0IjoxNzAwMDAwMDAwLCJleHAiOjE3MDAwMDM2MDB9.brzHGshlYawaLSbzOzdbiEmlfxhf9vhgcmhR4CEa8TrNx6nKM_q24usgv5KedbHH6XVaSVzXI4Y8WzPmDoBBDg"
)

// fixedIssuedAt is the timestamp the golden vectors were signed at; verifyNow is
// a moment inside both the cert (90 day) and grant (1 hour) validity windows.
var (
	fixedIssuedAt = time.Unix(1700000000, 0).UTC()
	verifyNow     = fixedIssuedAt.Add(time.Minute)
)

func fixedSeed(b byte) []byte {
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = b
	}
	return s
}

func userPriv() ed25519.PrivateKey { return ed25519.NewKeyFromSeed(fixedSeed(0x01)) }
func userPub() ed25519.PublicKey   { return userPriv().Public().(ed25519.PublicKey) }
func devicePub() ed25519.PublicKey {
	return ed25519.NewKeyFromSeed(fixedSeed(0x02)).Public().(ed25519.PublicKey)
}

// --- Certs -----------------------------------------------------------------

// The post-migration heyarr shim re-signs the pre-migration cert bytes exactly.
func TestHeyarrShimReproducesGoldenCert(t *testing.T) {
	got, err := henrol.SignCert(userPriv(), devicePub(), goldenDeviceEnc, fixedIssuedAt, henrol.CertLifetime)
	if err != nil {
		t.Fatal(err)
	}
	if got != goldenCert {
		t.Fatalf("heyarr shim cert bytes drifted from the pre-migration golden\n got:    %s\n golden: %s", got, goldenCert)
	}
}

// voidbind-go signs the identical cert bytes — the library and heyarr agree.
func TestVoidbindReproducesGoldenCert(t *testing.T) {
	got, err := venrol.SignCert(userPriv(), devicePub(), goldenDeviceEnc, fixedIssuedAt, venrol.CertLifetime)
	if err != nil {
		t.Fatal(err)
	}
	if got != goldenCert {
		t.Fatalf("voidbind-go cert bytes differ from the heyarr golden\n got:    %s\n golden: %s", got, goldenCert)
	}
}

// A cert minted by PRE-migration heyarr verifies via voidbind-go's relying-party
// surface, against the pinned user key, and resolves the right device.
func TestPreMigrationCertVerifiesViaVoidbindRP(t *testing.T) {
	auth, err := vrp.Verifier{Trust: vrp.MemTrust{goldenUserID: userPub()}}.Verify(goldenCert, verifyNow)
	if err != nil {
		t.Fatalf("voidbind-go/rp refused a pre-migration heyarr cert: %v", err)
	}
	if auth.DeviceKey != goldenDeviceKey {
		t.Fatalf("device key = %s, want %s", auth.DeviceKey, goldenDeviceKey)
	}
	if auth.DeviceEnc != goldenDeviceEnc {
		t.Fatalf("device enc key = %s, want %s", auth.DeviceEnc, goldenDeviceEnc)
	}
}

// The same pre-migration cert verifies through heyarr's (now shimmed) enrolment.
func TestPreMigrationCertVerifiesViaHeyarr(t *testing.T) {
	cert, err := henrol.VerifyCert(goldenCert, userPub(), verifyNow)
	if err != nil {
		t.Fatalf("heyarr enrolment refused a pre-migration cert: %v", err)
	}
	if cert.Device != goldenDeviceKey {
		t.Fatalf("cert.Device = %s, want %s", cert.Device, goldenDeviceKey)
	}
}

// --- Grants ----------------------------------------------------------------

func heyarrGrant() hgrant.Grant {
	return hgrant.Grant{
		Issuer:       goldenUserID,
		Principal:    goldenDeviceKey,
		Resource:     goldenResource,
		Capabilities: []hgrant.Capability{hgrant.CapabilityRead},
		IssuedAt:     fixedIssuedAt,
		ExpiresAt:    fixedIssuedAt.Add(time.Hour),
	}
}

func voidbindGrant() vgrant.Grant {
	return vgrant.Grant{
		Issuer:       goldenUserID,
		Principal:    goldenDeviceKey,
		Resource:     goldenResource,
		Capabilities: []vgrant.Capability{vgrant.CapabilityRead},
		IssuedAt:     fixedIssuedAt,
		ExpiresAt:    fixedIssuedAt.Add(time.Hour),
	}
}

// The post-migration heyarr shim re-signs the pre-migration grant bytes exactly.
func TestHeyarrShimReproducesGoldenGrant(t *testing.T) {
	got, err := heyarrGrant().Sign(userPriv())
	if err != nil {
		t.Fatal(err)
	}
	if got != goldenGrant {
		t.Fatalf("heyarr shim grant bytes drifted from the pre-migration golden\n got:    %s\n golden: %s", got, goldenGrant)
	}
}

// voidbind-go signs the identical grant bytes.
func TestVoidbindReproducesGoldenGrant(t *testing.T) {
	got, err := voidbindGrant().Sign(userPriv())
	if err != nil {
		t.Fatal(err)
	}
	if got != goldenGrant {
		t.Fatalf("voidbind-go grant bytes differ from the heyarr golden\n got:    %s\n golden: %s", got, goldenGrant)
	}
}

// A grant minted by PRE-migration heyarr verifies via voidbind-go, and via the
// heyarr shim — both against the pinned issuer key (an RP's pinned user IS its
// grant issuer, so one MemTrust answers both).
func TestPreMigrationGrantVerifiesBothWays(t *testing.T) {
	trust := vrp.MemTrust{goldenUserID: userPub()}
	req := vgrant.Request{Principal: goldenDeviceKey, Resource: goldenResource, Capability: vgrant.CapabilityRead}

	if _, err := vgrant.Verify(goldenGrant, trust, req, verifyNow); err != nil {
		t.Fatalf("voidbind-go/grant refused a pre-migration grant: %v", err)
	}

	hreq := hgrant.Request{Principal: goldenDeviceKey, Resource: goldenResource, Capability: hgrant.CapabilityRead}
	if _, err := hgrant.Verify(goldenGrant, trust, hreq, verifyNow); err != nil {
		t.Fatalf("heyarr grant refused a pre-migration grant: %v", err)
	}
}
