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
	size := flag.Int64("size", 0, "blob size in bytes (required unless --content-in or --playhead)")
	landedCSV := flag.String("landed", "", "comma-separated piece indices to write and record")
	seed := flag.Int64("seed", 20260828, "seed for the deterministic content")
	contentOut := flag.String("content-out", "", "write the full content bytes here (optional)")
	// Stage pieces of a REAL, already-existing blob rather than of synthetic
	// content. The bytes are read from this file and the digest is theirs, so the
	// staged partial addresses the SAME blob a running fabric is fetching — which
	// is what lets one node be pre-staged as a deterministic piece source for
	// another in a swarm, instead of racing the live transfer to have pieces
	// ready. --size is then derived from the file. Mutually exclusive with --seed
	// (which only shapes synthetic content).
	contentIn := flag.String("content-in", "", "stage pieces of the real bytes in this file, keeping their digest, instead of synthetic content")
	// Playhead-only mode: record where a consumer is reading in an EXTERNALLY
	// named blob, without staging any bytes. Used to demonstrate time-critical
	// priority (§33, §84) against a blob a running node is about to fetch.
	blobFlag := flag.String("blob", "", "with --playhead: the blob digest to record a playhead for")
	playhead := flag.Int64("playhead", -1, "with --blob: record this byte offset as the playhead and exit")
	flag.Parse()

	if *playhead >= 0 || *blobFlag != "" {
		if err := writePlayhead(*casRoot, *blobFlag, *playhead); err != nil {
			fmt.Fprintf(os.Stderr, "stagepartial: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(*casRoot, *size, *landedCSV, *seed, *contentOut, *contentIn); err != nil {
		fmt.Fprintf(os.Stderr, "stagepartial: %v\n", err)
		os.Exit(1)
	}
}

// writePlayhead records a playhead for an externally named blob and nothing else,
// so the demo can point a fresh transfer's window at a byte offset without
// staging any pieces itself.
func writePlayhead(casRoot, blobHex string, offset int64) error {
	if casRoot == "" || blobHex == "" || offset < 0 {
		return fmt.Errorf("playhead mode needs --cas, --blob and a non-negative --playhead")
	}
	blob, err := hashing.Parse(blobHex)
	if err != nil {
		return fmt.Errorf("parsing --blob: %w", err)
	}
	fs, err := cas.OpenFS(casRoot)
	if err != nil {
		return fmt.Errorf("opening CAS: %w", err)
	}
	if err := fs.SavePlayhead(blob, offset); err != nil {
		return fmt.Errorf("recording playhead: %w", err)
	}
	return nil
}

func run(casRoot string, size int64, landedCSV string, seed int64, contentOut, contentIn string) error {
	if casRoot == "" {
		return fmt.Errorf("--cas is required")
	}
	landed, err := parseIndices(landedCSV)
	if err != nil {
		return err
	}

	var data []byte
	if contentIn != "" {
		// Real bytes of an existing blob: read them, and let the digest be
		// theirs. --size, if given, must agree — a mismatch means staging pieces
		// of a different blob than the caller thinks under the same geometry.
		// #nosec G304,G703 -- contentIn is a fixture path from the demo, not
		// attacker input; this is a dev-only helper (ADR-0002 ships only
		// ./cmd/heyarr).
		data, err = os.ReadFile(contentIn)
		if err != nil {
			return fmt.Errorf("reading --content-in: %w", err)
		}
		if size > 0 && int64(len(data)) != size {
			return fmt.Errorf("--content-in is %d bytes but --size says %d", len(data), size)
		}
		size = int64(len(data))
	} else {
		if size <= 0 {
			return fmt.Errorf("a positive --size is required unless --content-in is given")
		}
		// Deterministic, non-zero content: a hole reads back as zeroes, so content
		// that is never zero is content a hole cannot be mistaken for.
		data = make([]byte, size)
		rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic fixture, not a credential
		_, _ = rng.Read(data)
		for i, b := range data {
			if b == 0 {
				data[i] = 1
			}
		}
	}
	if size <= 0 {
		return fmt.Errorf("--content-in is empty; there is nothing to stage")
	}
	if contentOut != "" {
		if werr := os.WriteFile(contentOut, data, 0o600); werr != nil { //nolint:gosec // a fixture output path from the demo, not attacker input
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
