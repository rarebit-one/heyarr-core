package cas

import (
	"os"
	"path/filepath"
	"testing"
)

// A deduplicating Link must never claim a rung of ADR-0014's ladder, whatever
// it was asked for.
//
// This is the shape of #223: on ext4, where block cloning is impossible, every
// deduplicating ingest reported `materialised":"reflink"` while the ingests
// beside it that actually moved bytes reported `"copy"`. `materialised` is the
// field an operator greps to confirm the ladder is reaching the rung they paid
// for, so a dedupe asserting the best possible outcome is wrong in the
// direction that reads as success.
func TestADeduplicatedLinkReportsNoRung(t *testing.T) {
	t.Parallel()
	for _, mode := range []Materialisation{Reflink, Hardlink, Copy} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s, err := OpenFS(filepath.Join(dir, "store"))
			if err != nil {
				t.Fatal(err)
			}
			content := []byte("the same bytes under two names")
			for _, name := range []string{"first.bin", "second.bin"} {
				if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			first, err := s.Link(t.Context(), filepath.Join(dir, "first.bin"), mode)
			if err != nil {
				t.Fatal(err)
			}
			if first.Deduplicated {
				t.Fatal("the first Link reported a duplicate")
			}
			// The first one DID materialise, so it must name a real rung —
			// the control that makes the assertion below mean something.
			if first.Materialised == None {
				t.Error("the first Link reported no rung, but it materialised the bytes")
			}

			second, err := s.Link(t.Context(), filepath.Join(dir, "second.bin"), mode)
			if err != nil {
				t.Fatal(err)
			}
			if !second.Deduplicated {
				t.Fatal("the second Link did not report a duplicate")
			}
			if second.Materialised != None {
				t.Errorf("a deduplicated Link asked for %s reported materialised=%s, want %s",
					mode, second.Materialised, None)
			}
		})
	}
}
