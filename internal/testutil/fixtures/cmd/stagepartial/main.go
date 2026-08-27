// Command stagepartial writes a blob into a CAS as an in-flight transfer: a
// sparse staging file with only some pieces written, and the availability record
// that says which. It is how scripts/acceptance.sh demonstrates progressive
// playback (§33, §84, ADR-0044) deterministically — a genuinely incomplete blob
// on disk, with a known landed range, without racing a live swarm to catch one
// mid-transfer.
//
// It lives under internal/testutil for the same reason genlibrary does: it is
// not part of the product (ADR-0002 ships ./cmd/heyarr and nothing else), so it
// is a dev helper run with `go run`.
//
//	go run ./internal/testutil/fixtures/cmd/stagepartial \
//	    --cas /path/to/data/cas --size 786432 --landed 0,2 --content-out /tmp/full.bin
//
// It prints the blob's digest (blake3:...) to stdout, so the demo can address the
// content route, and writes the FULL content to --content-out so the demo can
// verify the served range against the true bytes. Landing more pieces is the
// same command run again with a longer --landed against the same --cas: it
// rewrites the record and fills the newly named pieces, which is exactly what a
// worker does as pieces arrive — so a blocked read then resolves.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/pieces"
)

func main() {
	casRoot := flag.String("cas", "", "CAS root to stage into (required)")
	size := flag.Int64("size", 0, "blob size in bytes (required)")
	landedCSV := flag.String("landed", "", "comma-separated piece indices to write and record")
	seed := flag.Int64("seed", 20260828, "seed for the deterministic content")
	contentOut := flag.String("content-out", "", "write the full content bytes here (optional)")
	flag.Parse()

	if err := run(*casRoot, *size, *landedCSV, *seed, *contentOut); err != nil {
		fmt.Fprintf(os.Stderr, "stagepartial: %v\n", err)
		os.Exit(1)
	}
}

func run(casRoot string, size int64, landedCSV string, seed int64, contentOut string) error {
	if casRoot == "" || size <= 0 {
		return fmt.Errorf("--cas and a positive --size are required")
	}
	landed, err := parseIndices(landedCSV)
	if err != nil {
		return err
	}

	// Deterministic, non-zero content: a hole reads back as zeroes, so content
	// that is never zero is content a hole cannot be mistaken for.
	data := make([]byte, size)
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic fixture, not a credential
	_, _ = rng.Read(data)
	for i, b := range data {
		if b == 0 {
			data[i] = 1
		}
	}
	if contentOut != "" {
		if werr := os.WriteFile(contentOut, data, 0o600); werr != nil {
			return fmt.Errorf("writing content-out: %w", werr)
		}
	}

	fs, err := cas.OpenFS(casRoot)
	if err != nil {
		return fmt.Errorf("opening CAS: %w", err)
	}
	blob, _, err := hashing.HashReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("hashing content: %w", err)
	}
	g, err := pieces.For(size)
	if err != nil {
		return fmt.Errorf("geometry: %w", err)
	}

	p, err := fs.OpenPartial(context.Background(), blob)
	if err != nil {
		return fmt.Errorf("opening partial: %w", err)
	}
	have := pieces.NewAvailability(g.Count())
	for _, idx := range landed {
		off, length, rerr := g.Range(idx)
		if rerr != nil {
			_ = p.Close()
			return fmt.Errorf("piece %d: %w", idx, rerr)
		}
		if _, werr := p.WriteAt(data[off:off+length], off); werr != nil {
			_ = p.Close()
			return fmt.Errorf("writing piece %d: %w", idx, werr)
		}
		have.Add(idx)
	}
	if err := p.Close(); err != nil {
		return fmt.Errorf("closing partial: %w", err)
	}
	if err := fs.SavePieceProgress(blob, pieces.Encode(g, have)); err != nil {
		return fmt.Errorf("recording progress: %w", err)
	}

	fmt.Println(blob.String())
	return nil
}

func parseIndices(csv string) ([]int, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil, nil
	}
	var out []int
	for _, f := range strings.Split(csv, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			return nil, fmt.Errorf("bad piece index %q: %w", f, err)
		}
		out = append(out, n)
	}
	return out, nil
}
