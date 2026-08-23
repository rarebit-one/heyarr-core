package chunking

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"testing"
)

// Reassembly must be exact, and the offsets are half of what "exact" means. A
// chunker that returns the right bytes with the wrong offsets reassembles
// perfectly here and corrupts every reuse decision downstream, silently — so
// the offsets are asserted as contiguous from zero with no gap and no overlap,
// not merely as "the bytes rejoin".
func TestReassemblyIsExactIncludingOffsets(t *testing.T) {
	cfg := DefaultConfig()

	tests := []struct {
		name  string
		cfg   Config
		input []byte
	}{
		{"empty", cfg, nil},
		{"one byte", cfg, []byte{0x42}},
		{"exactly min", cfg, pseudoRandom(cfg.Min, 1)},
		{"min minus one", cfg, pseudoRandom(cfg.Min-1, 2)},
		{"min plus one", cfg, pseudoRandom(cfg.Min+1, 3)},
		{"exactly avg", cfg, pseudoRandom(cfg.Avg, 4)},
		{"exactly max", cfg, pseudoRandom(cfg.Max, 5)},
		{"max plus one", cfg, pseudoRandom(cfg.Max+1, 6)},
		{"several megabytes of random", cfg, pseudoRandom(9<<20, 7)},
		{"several megabytes of repetitive", cfg, repetitive(9 << 20)},
		{"random at fine parameters", fineConfig, pseudoRandom(4<<20, 8)},
		{"repetitive at fine parameters", fineConfig, repetitive(4 << 20)},
		{"all zeroes", fineConfig, make([]byte, 1<<20)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := chunkBytes(t, tt.input, tt.cfg)

			var want int64
			var rejoined []byte
			for i, c := range chunks {
				if c.Offset != want {
					t.Fatalf("chunk %d starts at %d, want %d — the offsets are not contiguous", i, c.Offset, want)
				}
				if c.Length <= 0 {
					t.Fatalf("chunk %d has length %d; a zero-length chunk is not a chunk", i, c.Length)
				}
				got := tt.input[c.Offset:c.End()]
				if d := digestOf(t, got); !c.Digest.Equal(d) {
					t.Errorf("chunk %d digest = %s, want %s — the digest does not describe the chunk's own bytes", i, c.Digest, d)
				}
				rejoined = append(rejoined, got...)
				want = c.End()
			}

			if want != int64(len(tt.input)) {
				t.Errorf("chunks cover %d bytes, input is %d — the tail was dropped or double-counted", want, len(tt.input))
			}
			if !bytes.Equal(rejoined, tt.input) {
				t.Errorf("reassembled %d bytes, they do not equal the input", len(rejoined))
			}
			if len(tt.input) == 0 && len(chunks) != 0 {
				t.Errorf("empty input produced %d chunks, want none", len(chunks))
			}
		})
	}
}

// Bounds are asserted over a real distribution, not over one case: a chunker
// can honour [min, max] on a single small input and violate it constantly on
// the repetitive data that actually stresses the cap.
func TestChunkSizeBoundsHoldOverADistribution(t *testing.T) {
	tests := []struct {
		name  string
		cfg   Config
		input []byte
	}{
		{"random, default parameters", DefaultConfig(), pseudoRandom(24<<20, 11)},
		{"repetitive, default parameters", DefaultConfig(), repetitive(64 << 20)},
		{"random, fine parameters", fineConfig, pseudoRandom(8<<20, 12)},
		{"repetitive, fine parameters", fineConfig, repetitive(8 << 20)},
		{"zeroes, fine parameters", fineConfig, make([]byte, 8<<20)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := chunkBytes(t, tt.input, tt.cfg)
			if len(chunks) < 8 {
				t.Fatalf("only %d chunks; this input is too small to be a distribution", len(chunks))
			}

			var total int64
			atMax := 0
			for i, c := range chunks {
				last := i == len(chunks)-1
				if c.Length > int64(tt.cfg.Max) {
					t.Fatalf("chunk %d is %d bytes, above Max %d", i, c.Length, tt.cfg.Max)
				}
				if !last && c.Length < int64(tt.cfg.Min) {
					t.Fatalf("chunk %d is %d bytes, below Min %d (only the last chunk may be shorter)", i, c.Length, tt.cfg.Min)
				}
				if c.Length == int64(tt.cfg.Max) {
					atMax++
				}
				total += c.Length
			}
			if total != int64(len(tt.input)) {
				t.Fatalf("chunk lengths sum to %d, input is %d", total, len(tt.input))
			}
			t.Logf("%d chunks, mean %.0f bytes (%.2f x Avg), %d at Max",
				len(chunks), float64(total)/float64(len(chunks)),
				float64(total)/float64(len(chunks))/float64(tt.cfg.Avg), atMax)
		})
	}
}

// shiftSurvivalFloor is the fraction of chunk digests that must survive a
// one-byte insertion at the front of the input.
//
// It is a measured number, not a hope. Over a 16 MiB pseudo-random input at
// fineConfig the measured survival is 0.9997: 3610 of 3611 chunks still appear
// after the shift, because the insertion perturbs the one boundary around it
// and nothing else. At the 1 MiB default average over 96 MiB it measures
// 0.9891 — the same one or two disturbed chunks, out of ninety-two rather than
// out of 3611. The floor below sits under both by a margin that absorbs
// ordinary variation between inputs, and far above the fixed-size control
// (measured 0.0000 in both) so that only a genuinely content-defined chunker
// clears it.
//
// This is the assertion the whole milestone rests on: M5-07 (chunk reuse across
// modified files) and M5-08 (repair of damaged replicas) are consequences of
// this number being close to one.
const shiftSurvivalFloor = 0.95

// fixedSurvivalCeiling is the control. Without it this test would also pass on
// a chunker that had degenerated into returning the entire file as one chunk
// with a stable digest, or on any other accident that happens to keep digests
// stable. A fixed-size chunker over the same shifted input must retain almost
// nothing.
const fixedSurvivalCeiling = 0.01

func TestOneByteInsertionPreservesMostChunks(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		size int
	}{
		{"fine parameters over 16 MiB", fineConfig, 16 << 20},
		{"default parameters over 96 MiB", DefaultConfig(), 96 << 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := pseudoRandom(tt.size, 21)
			shifted := append([]byte{0xff}, original...)

			before := chunkBytes(t, original, tt.cfg)
			after := chunkBytes(t, shifted, tt.cfg)
			if len(before) < 32 {
				t.Fatalf("only %d chunks; too few to measure a fraction against", len(before))
			}

			survived := survivingFraction(before, after)
			t.Logf("content-defined: %d chunks before, %d after, %.4f survived", len(before), len(after), survived)
			if survived < shiftSurvivalFloor {
				t.Errorf("only %.4f of chunk digests survived a one-byte insertion, want at least %.4f — "+
					"boundaries are following position rather than content", survived, shiftSurvivalFloor)
			}

			// The control, over the same bytes: cut points from position alone.
			fixedBefore := fixedSizeChunks(t, original, tt.cfg.Avg)
			fixedAfter := fixedSizeChunks(t, shifted, tt.cfg.Avg)
			fixedSurvived := survivingFraction(fixedBefore, fixedAfter)
			t.Logf("fixed-size control: %d chunks before, %d after, %.4f survived", len(fixedBefore), len(fixedAfter), fixedSurvived)
			if fixedSurvived > fixedSurvivalCeiling {
				t.Errorf("the fixed-size control retained %.4f of its chunks, want at most %.4f — "+
					"the control is not controlling for anything", fixedSurvived, fixedSurvivalCeiling)
			}
			if survived <= fixedSurvived {
				t.Errorf("content-defined chunking (%.4f) did no better than fixed-size (%.4f)", survived, fixedSurvived)
			}
		})
	}
}

// The chunk sequence must not depend on how the bytes arrive. A rolling hash
// that is reset, or a boundary search that restarts, at a buffer edge produces
// different manifests for the same blob depending on the network's packet
// boundaries — the most common bug in this kind of code, and invisible to a
// test that only ever feeds one big slice. Same shape as
// hashing.TestDigestIsIndependentOfChunking.
func TestChunksAreIndependentOfReadFraming(t *testing.T) {
	for _, cfg := range []Config{DefaultConfig(), fineConfig} {
		content := pseudoRandom(9<<20, 31)
		want := chunkBytes(t, content, cfg)
		if len(want) < 4 {
			t.Fatalf("only %d chunks; this proves very little", len(want))
		}

		frames := []int{1, 4 << 10, 1 << 20, readSize, cfg.Max, len(content)}
		for _, frame := range frames {
			if frame == 1 && cfg.Avg == DefaultAvg {
				// One byte per Read over 9 MiB at the default parameters is
				// nine million instrumented calls under -race. fineConfig
				// below runs the same assertion at a size where it is cheap,
				// and the buffer arithmetic being tested is identical.
				continue
			}
			r := &framedReader{data: content, frame: frame}
			got := collect(t, r, cfg)
			if len(got) != len(want) {
				t.Fatalf("reads of %d bytes produced %d chunks, single-shot produced %d", frame, len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("reads of %d bytes: chunk %d = {off %d len %d %s}, want {off %d len %d %s}",
						frame, i, got[i].Offset, got[i].Length, got[i].Digest,
						want[i].Offset, want[i].Length, want[i].Digest)
				}
			}
		}
	}
}

// One byte at a time, on an input small enough that nine million Read calls is
// not the test's runtime.
func TestSingleByteReadsAgreeWithSingleShot(t *testing.T) {
	content := pseudoRandom(1<<20, 32)
	want := chunkBytes(t, content, fineConfig)
	got := collect(t, &framedReader{data: content, frame: 1}, fineConfig)
	if len(got) != len(want) {
		t.Fatalf("one-byte reads produced %d chunks, single-shot produced %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("chunk %d differs between one-byte reads and a single shot: %+v vs %+v", i, got[i], want[i])
		}
	}
}

// Memory must not scale with the input. A 20 GB blob is the normal case here
// (ADR-0013), so a chunker that buffers what it is given is unusable on exactly
// the inputs chunking exists for.
func TestMemoryStaysFlatOverALargeInput(t *testing.T) {
	cfg := DefaultConfig()

	measure := func(size int64) (heap, allocated uint64, chunks int) {
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)

		c, err := New(&generatedReader{remaining: size, state: 99}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		var covered int64
		for {
			chunk, err := c.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			covered += chunk.Length
			chunks++
		}
		if covered != size {
			t.Fatalf("covered %d bytes of %d", covered, size)
		}

		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(c) // the chunker's buffer is what we are measuring
		return after.HeapAlloc, after.TotalAlloc - before.TotalAlloc, chunks
	}

	const small = 16 << 20
	const large = 128 << 20
	const budget = DefaultMax + readSize // one buffer's worth of slack

	smallHeap, smallAlloc, smallChunks := measure(small)
	largeHeap, largeAlloc, largeChunks := measure(large)
	t.Logf("%d MiB: %d chunks, live heap %d bytes, %d bytes allocated in total", small>>20, smallChunks, smallHeap, smallAlloc)
	t.Logf("%d MiB: %d chunks, live heap %d bytes, %d bytes allocated in total", large>>20, largeChunks, largeHeap, largeAlloc)

	if largeChunks < smallChunks*4 {
		t.Fatalf("the large input produced %d chunks against %d for an input eight times smaller; "+
			"the two runs are not actually different sizes", largeChunks, smallChunks)
	}
	// Eight times the input — 112 MiB more bytes — may cost at most one more
	// buffer, both in what stays live and in what is allocated at all.
	if growth := int64(largeHeap) - int64(smallHeap); growth > budget {
		t.Errorf("live heap grew by %d bytes when the input grew by %d — memory is scaling with the input",
			growth, large-small)
	}
	if growth := int64(largeAlloc) - int64(smallAlloc); growth > budget {
		t.Errorf("total allocation grew by %d bytes when the input grew by %d — the chunker is buffering what it reads",
			growth, large-small)
	}
	if int64(largeAlloc) > 4*budget {
		t.Errorf("chunking %d MiB allocated %d bytes in total; a streaming chunker should stay near its %d byte buffer",
			large>>20, largeAlloc, budget)
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"defaults", DefaultConfig(), false},
		{"fine", fineConfig, false},
		{"zero min", Config{Min: 0, Avg: 1 << 20, Max: 4 << 20}, true},
		{"negative min", Config{Min: -1, Avg: 1 << 20, Max: 4 << 20}, true},
		{"min equals avg", Config{Min: 1 << 20, Avg: 1 << 20, Max: 4 << 20}, true},
		{"min above avg", Config{Min: 2 << 20, Avg: 1 << 20, Max: 4 << 20}, true},
		{"avg equals max", Config{Min: 1 << 10, Avg: 4 << 20, Max: 4 << 20}, true},
		{"avg above max", Config{Min: 1 << 10, Avg: 8 << 20, Max: 4 << 20}, true},
		{"avg not a power of two", Config{Min: 1 << 10, Avg: 1_000_000, Max: 4 << 20}, true},
		{"avg too small for normalisation", Config{Min: 1, Avg: 4, Max: 64}, true},
		{"zero value", Config{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Validate accepted %+v", tt.cfg)
				}
				if !errors.Is(err, ErrInvalidConfig) {
					t.Errorf("error %v does not wrap ErrInvalidConfig", err)
				}
				if _, err := New(bytes.NewReader(nil), tt.cfg); err == nil {
					t.Error("New accepted a config Validate rejected")
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate rejected %+v: %v", tt.cfg, err)
			}
		})
	}
}

// A read failure is not an end of stream. Reporting it as one would hand the
// caller a short manifest that describes a truncated blob and looks complete.
func TestReadErrorsAreReportedAndSticky(t *testing.T) {
	sentinel := errors.New("disk fell over")
	c, err := New(&failingReader{prefix: pseudoRandom(1<<10, 41), err: sentinel}, fineConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Next(); !errors.Is(err, sentinel) {
		t.Fatalf("Next returned %v, want the reader's error", err)
	}
	if _, err := c.Next(); !errors.Is(err, sentinel) {
		t.Fatalf("a second Next returned %v; the error must be sticky, not retried into an EOF", err)
	}
}

func TestExhaustedChunkerKeepsReturningEOF(t *testing.T) {
	c, err := New(bytes.NewReader(pseudoRandom(1<<10, 42)), fineConfig)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := c.Next(); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("Next: %v", err)
		}
	}
	if _, err := c.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next after exhaustion returned %v, want io.EOF", err)
	}
}

func TestChunkEnd(t *testing.T) {
	c := Chunk{Offset: 100, Length: 25}
	if c.End() != 125 {
		t.Errorf("End() = %d, want 125", c.End())
	}
}
