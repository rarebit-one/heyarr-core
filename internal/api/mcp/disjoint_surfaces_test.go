package mcp_test

import (
	"path/filepath"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/device/personalmcp"
)

// stubReader is a device-side personal-state reader, enough to make the Personal
// MCP register its read tools for this enumeration.
type stubReader struct{}

func (stubReader) Playlist(string) ([]string, error) { return nil, nil }
func (stubReader) Starred(string) ([]string, error)  { return nil, nil }
func (stubReader) History(string) (personalmcp.PlayHistory, error) {
	return personalmcp.PlayHistory{}, nil
}

func (stubReader) ReadingPositions(string) ([]personalmcp.ReadingPosition, error) { return nil, nil }

// TestTheTwoMCPSurfacesAreDisjoint is §72/§73 made structural: the controller-side
// MCP (this package) and the device-side Personal MCP share NO tool. The Personal
// MCP exposes a personal-state read tool; the controller MCP exposes none of the
// Personal MCP's tools — so no path reaches decrypted personal state through the
// controller, which cannot decrypt it anyway (§72). This complements
// TestNoToolTouchesPersonalState (which forbids the vocabulary) by asserting the
// two enumerated surfaces do not intersect.
func TestTheTwoMCPSurfacesAreDisjoint(t *testing.T) {
	controller := newHarness(t, false).server.Names()

	ds, err := device.NewStore(device.StoreOptions{Dir: filepath.Join(t.TempDir(), "device")})
	if err != nil {
		t.Fatal(err)
	}
	personal, err := personalmcp.New(personalmcp.Options{Store: ds, PersonalState: stubReader{}})
	if err != nil {
		t.Fatal(err)
	}
	personalNames := personal.Names()

	// The Personal MCP genuinely exposes a personal-state read tool — otherwise
	// this test would pass vacuously against an empty personal surface.
	if !containsName(personalNames, "personal_playlist") {
		t.Fatalf("the Personal MCP does not expose personal_playlist: %v", personalNames)
	}

	// And that tool — and every other Personal MCP tool — is NOT on the controller.
	controllerSet := make(map[string]bool, len(controller))
	for _, n := range controller {
		controllerSet[n] = true
	}
	for _, n := range personalNames {
		if controllerSet[n] {
			t.Errorf("tool %q is on BOTH the controller and the Personal MCP surface — they must be disjoint (§72, §73)", n)
		}
	}
}

func containsName(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
