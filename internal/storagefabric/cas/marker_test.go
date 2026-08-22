package cas

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// The CAS half of the two-places peer identity (ADR-0010, M4-03). The store
// records which peer owns these bytes and reports it back; it deliberately does
// not compare it with anything, because it cannot see the database.

const testPeer = "01990000-0000-7000-8000-00000000000a"

func TestAFreshRootIsUnbound(t *testing.T) {
	s, err := OpenFS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.MarkerPeerID()
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Errorf("a fresh root reports peer %q, want unbound", id)
	}
}

func TestBindPeerSurvivesReopeningAndKeepsTheLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cas")
	s, err := OpenFS(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindPeer(testPeer); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenFS(root)
	if err != nil {
		t.Fatalf("a bound root would not reopen: %v", err)
	}
	id, err := reopened.MarkerPeerID()
	if err != nil {
		t.Fatal(err)
	}
	if id != testPeer {
		t.Errorf("marker peer = %q, want %q", id, testPeer)
	}

	// Binding must not lose what the marker already carried — a rewritten
	// marker that dropped the layout version would make the next binary
	// misread the root rather than refuse it.
	data, err := os.ReadFile(reopened.MarkerPath())
	if err != nil {
		t.Fatal(err)
	}
	var marker Marker
	if err := json.Unmarshal(data, &marker); err != nil {
		t.Fatal(err)
	}
	if marker.Version != LayoutVersion || marker.Algo != hashing.Algorithm || len(marker.Fanout) != 2 {
		t.Errorf("binding a peer damaged the marker: %+v", marker)
	}

	// The marker holds an identity; it must not be world-readable for the same
	// reason the rest of the data directory is not.
	info, err := os.Stat(reopened.MarkerPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o007 != 0 {
		t.Errorf("the marker is %#o", perm)
	}
}

func TestBindPeerRefusesAnEmptyPeer(t *testing.T) {
	s, err := OpenFS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindPeer(""); err == nil {
		t.Fatal("an empty peer id was accepted, which would silently unbind the root")
	}
}

// Rebinding replaces the value rather than appending or refusing: the store
// writes what it is told, and deciding whether that is right needs the
// database, which is the caller's job.
func TestBindPeerReplaces(t *testing.T) {
	s, err := OpenFS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindPeer(testPeer); err != nil {
		t.Fatal(err)
	}
	const other = "01990000-0000-7000-8000-00000000000b"
	if err := s.BindPeer(other); err != nil {
		t.Fatal(err)
	}
	id, err := s.MarkerPeerID()
	if err != nil {
		t.Fatal(err)
	}
	if id != other {
		t.Errorf("marker peer = %q, want %q", id, other)
	}
}
