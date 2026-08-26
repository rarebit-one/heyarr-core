package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/useridentity"
)

// identityDir is a config-directory-shaped temporary directory, never a data
// directory — the same distinction the device tests keep.
func identityDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "config", "heyarr", "identity")
}

// TestIdentityEnrolTakesTheLabelOffThroughTheCLI drives the whole client story
// through the real command tree: generate an identity, generate a device, enrol
// it, and see the labels come off — the ADR-0032 revisit, observed at the edge
// a person actually uses.
func TestIdentityEnrolTakesTheLabelOffThroughTheCLI(t *testing.T) {
	idDir := identityDir(t)
	devDir := deviceDir(t)
	ctx := context.Background()

	if _, _, err := run(t, ctx, "identity", "generate", "--identity-dir", idDir, "--name", "me"); err != nil {
		t.Fatalf("identity generate: %v", err)
	}
	if _, _, err := run(t, ctx, "device", "generate", "--device-dir", devDir, "--name", "box"); err != nil {
		t.Fatalf("device generate: %v", err)
	}

	// Before enrol: not_enrolled / unproven.
	before := showDeviceJSON(t, ctx, devDir)
	if before.EnrolmentStatus != device.EnrolmentNotEnrolled || !before.Unproven {
		t.Fatalf("before enrol: status=%q unproven=%v, want not_enrolled/true",
			before.EnrolmentStatus, before.Unproven)
	}

	// Enrol.
	out, _, err := run(t, ctx, "identity", "enrol",
		"--identity-dir", idDir, "--device-dir", devDir)
	if err != nil {
		t.Fatalf("identity enrol: %v", err)
	}
	if !strings.Contains(out, "device enrolled") {
		t.Fatalf("enrol output did not confirm enrolment:\n%s", out)
	}

	// After enrol: enrolled / proven, and the enrolled user is our identity.
	after := showDeviceJSON(t, ctx, devDir)
	if after.EnrolmentStatus != device.EnrolmentEnrolled || after.Unproven {
		t.Fatalf("after enrol: status=%q unproven=%v, want enrolled/false",
			after.EnrolmentStatus, after.Unproven)
	}
	idPub := showIdentityJSON(t, ctx, idDir).PublicKey
	if after.EnrolledUser != idPub {
		t.Fatalf("after enrol: enrolled_user=%q, want %q", after.EnrolledUser, idPub)
	}
}

// TestIdentityCredentialAuthenticates: the credential the CLI prints is a real
// device credential — a cert and a possession proof that verify against the
// identity and device keys the CLI stored.
func TestIdentityCredentialAuthenticates(t *testing.T) {
	idDir := identityDir(t)
	devDir := deviceDir(t)
	ctx := context.Background()

	if _, _, err := run(t, ctx, "identity", "generate", "--identity-dir", idDir); err != nil {
		t.Fatalf("identity generate: %v", err)
	}
	if _, _, err := run(t, ctx, "device", "generate", "--device-dir", devDir); err != nil {
		t.Fatalf("device generate: %v", err)
	}
	if _, _, err := run(t, ctx, "identity", "enrol", "--identity-dir", idDir, "--device-dir", devDir); err != nil {
		t.Fatalf("identity enrol: %v", err)
	}

	out, _, err := run(t, ctx, "identity", "credential", "--device-dir", devDir)
	if err != nil {
		t.Fatalf("identity credential: %v", err)
	}
	cred := strings.TrimSpace(out)
	certPart, proofPart, ok := strings.Cut(cred, enrolment.CredentialSeparator)
	if !ok {
		t.Fatalf("credential is not two halves: %q", cred)
	}

	now := time.Now().UTC()
	idPub := parseKey(t, showIdentityJSON(t, ctx, idDir).PublicKey)
	devPub := parseKey(t, showDeviceJSON(t, ctx, devDir).PublicKey)
	cert, err := enrolment.VerifyCert(certPart, idPub, now)
	if err != nil {
		t.Fatalf("credential cert half does not verify: %v", err)
	}
	if cert.Device != identity.FormatPublicKey(devPub) {
		t.Fatalf("cert binds %q, device is %q", cert.Device, identity.FormatPublicKey(devPub))
	}
	if err := enrolment.VerifyPossession(proofPart, devPub, certPart, now); err != nil {
		t.Fatalf("credential possession half does not verify: %v", err)
	}
}

// TestIdentityCredentialNeedsEnrolment: without a cert there is nothing to
// authenticate with, and the command says so rather than printing half a
// credential.
func TestIdentityCredentialNeedsEnrolment(t *testing.T) {
	devDir := deviceDir(t)
	ctx := context.Background()
	if _, _, err := run(t, ctx, "device", "generate", "--device-dir", devDir); err != nil {
		t.Fatalf("device generate: %v", err)
	}
	if _, _, err := run(t, ctx, "identity", "credential", "--device-dir", devDir); err == nil {
		t.Fatal("credential on an un-enrolled device: expected an error")
	}
}

func showDeviceJSON(t *testing.T, ctx context.Context, dir string) device.View {
	t.Helper()
	out, _, err := run(t, ctx, "device", "show", "--device-dir", dir, "--json")
	if err != nil {
		t.Fatalf("device show: %v", err)
	}
	var v device.View
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("device show --json invalid: %v\n%s", err, out)
	}
	return v
}

func showIdentityJSON(t *testing.T, ctx context.Context, dir string) useridentity.View {
	t.Helper()
	out, _, err := run(t, ctx, "identity", "show", "--identity-dir", dir, "--json")
	if err != nil {
		t.Fatalf("identity show: %v", err)
	}
	var v useridentity.View
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("identity show --json invalid: %v\n%s", err, out)
	}
	return v
}

func parseKey(t *testing.T, rendered string) []byte {
	t.Helper()
	k, err := identity.ParsePublicKey(rendered)
	if err != nil {
		t.Fatalf("parsing %q: %v", rendered, err)
	}
	return k
}
