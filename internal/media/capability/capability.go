// Package capability decides what a worker may advertise, by EXERCISING each
// candidate rather than by reading a list (§6, §75, ADR-0023, ADR-0037).
//
// # The measurement this package exists because of
//
// `ffmpeg -encoders` will happily list a hardware AV1 encoder on silicon that
// cannot encode AV1. Asking it to encode eight frames then fails with
//
//	No capable devices found
//
// while the same device decodes AV1 without trouble. Encode and decode support
// are asymmetric per hardware generation, and the asymmetry is not derivable
// from anything FFmpeg prints — not from `-encoders`, not from `-hwaccels`, not
// from the codec flags. There is no string to parse that answers the question.
//
// A node advertising a capability it cannot deliver is worse than a node
// advertising nothing, because the job ROUTES to it and then fails — after the
// queue has already decided this was the right home for the work, and after it
// has passed over the node that could have done it.
//
// So: encode a handful of frames from a synthetic source to a null sink with
// each candidate encoder, and advertise only what exits successfully. Fast,
// unambiguous, and the only thing that separates "listed" from "works".
//
// # The list is an optimisation and never a proof
//
// [Probe] does ask the binary what it has, and skips candidates it does not
// mention — there is no point spawning a process to be told an encoder does not
// exist. That is a way of doing LESS work, not a way of deciding the answer.
// Every capability this package returns has a successful process exit behind it.
// If that ever inverts, the routing table starts lying and the lie is only
// discoverable by watching jobs fail.
//
// # False negatives are safe; false positives are the bug
//
// A probe that fails for an environmental reason — a device claimed by another
// process, a VAAPI render node that is not present — costs this node the work.
// A probe that passes wrongly costs the FLEET the work, because the queue hands
// it over and nobody else looks at it again. Everything below is biased
// accordingly.
package capability

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// Source says how a held capability was established.
//
// It travels with the capability because the RE-VERIFICATION RULE differs by
// source, and an operator reading a row needs to know which rule applies to it.
type Source string

const (
	// SourceBinary is a tool resolved at startup. ADR-0023's stance, unchanged:
	// the binary is not re-resolved, because installing ffmpeg under a running
	// Heyarr and expecting it to be noticed is not a supported flow.
	SourceBinary Source = "binary"
	// SourceProbe is a capability that was EXERCISED. It is re-verified on a
	// beat, because a device can be claimed by another process or broken by a
	// kernel update without the binary changing.
	SourceProbe Source = "probe"
	// SourceService is an external network service that reported itself healthy
	// (ADR-0025).
	SourceService Source = "service"
)

// Vocabulary. Dotted and hierarchical, so a job can require exactly as much as
// it needs: the binary, a codec, or a codec on a specific accelerator.
//
// The job queue's existing `required_capability IN (...)` matches these with no
// query change and no migration — but only because the match is EXACT. `ffmpeg`
// is a prefix of `ffmpeg.encoder.hevc`, so a substring or LIKE match here would
// route AV1 work to a node that merely has the binary installed. Every
// comparison of a capability string, in this repo and in its tests, is equality.
const (
	// FFmpeg is the binary itself, and the coarsest thing anyone can require.
	FFmpeg = "ffmpeg"
	// FFprobe is the other half of the toolchain (ADR-0023).
	FFprobe = "ffprobe"

	encoderPrefix = FFmpeg + ".encoder."
)

// EncoderCapability is the dotted name for a codec on an accelerator.
//
// A software encoder has no accelerator segment: `ffmpeg.encoder.hevc` means
// "this node can encode HEVC somehow", which is exactly what a job that does
// not care where the cycles come from should require.
func EncoderCapability(codec, accel string) string {
	if accel == "" {
		return encoderPrefix + codec
	}
	return encoderPrefix + codec + "." + accel
}

// CodecCapability is the rollup: "can this node encode this codec at all".
func CodecCapability(codec string) string { return encoderPrefix + codec }

// Candidate is one encoder worth trying.
type Candidate struct {
	// Codec is the vocabulary segment — "h264", "hevc", "av1".
	Codec string
	// Accel is the accelerator segment, empty for a software encoder.
	Accel string
	// Encoder is what FFmpeg calls it on the command line.
	Encoder string
}

// Capability is the dotted name this candidate would contribute if it passed.
func (c Candidate) Capability() string { return EncoderCapability(c.Codec, c.Accel) }

// Held is one capability this node has established, and on what basis.
type Held struct {
	Name   string
	Source Source
	// Detail is a few words for an operator: which encoder proved it, or which
	// binary. Never a command line and never stderr — this is read over an API.
	Detail   string
	ProvedAt time.Time
}

// Runner is the seam between deciding what to try and actually trying it.
//
// It exists because the case that matters most cannot otherwise be tested where
// it matters: "this machine lists an encoder it cannot run" is not reproducible
// on a machine that does not have that hardware, and "this machine has no
// ffmpeg at all" is not reproducible on one that does. Both are ordinary
// conditions in the fleet this feature is for.
type Runner interface {
	// ListEncoders returns the encoder names the binary says it has. Used only
	// to SKIP candidates, never to accept one.
	ListEncoders(ctx context.Context) ([]string, error)
	// Exercise runs a real encode and returns nil only if the process exited
	// successfully.
	Exercise(ctx context.Context, c Candidate) error
}

// Probe returns what this node has PROVEN it can do, given the candidates.
//
// # Order of operations, and why each step is where it is
//
//  1. Ask the binary what it lists. A failure here is not fatal: it means every
//     candidate is tried, which is slower and no less correct. Treating an
//     unreadable list as "nothing is available" would silently disarm the whole
//     feature on any FFmpeg build whose `-encoders` output we cannot parse.
//  2. Skip candidates the binary does not mention. Doing less work.
//  3. EXERCISE what is left. This step, and only this step, produces a
//     capability. A candidate that errors contributes nothing and is logged at
//     debug — a machine with five accelerators it does not have would otherwise
//     emit five warnings on every beat, forever.
//  4. Roll a codec up once at least one accelerator under it has passed.
//
// Deterministic output order, because this feeds a durable advertisement and a
// map walk would rewrite the same set in a different order on every beat.
func Probe(ctx context.Context, r Runner, candidates []Candidate, now func() time.Time, log *slog.Logger) []Held {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	listed := map[string]bool{}
	haveList := false
	if names, err := r.ListEncoders(ctx); err == nil {
		haveList = true
		for _, n := range names {
			listed[n] = true
		}
	} else {
		// Not fatal, and worth saying once: an unparseable list means every
		// candidate is exercised, which costs time and changes no answer.
		log.Debug("could not read the encoder list; every candidate will be exercised",
			"error", err)
	}

	byName := map[string]Held{}
	codecs := map[string]bool{}
	for _, c := range candidates {
		if haveList && !listed[c.Encoder] {
			continue
		}
		if err := ctx.Err(); err != nil {
			break
		}
		// THE line this package exists for. What follows must depend on the
		// process having exited successfully, not on c.Encoder having appeared
		// in `listed` above. Deleting this call and advertising the listed set
		// is the sabotage ADR-0037 records, and it is caught by a fixture that
		// lists an encoder it refuses to run.
		if err := r.Exercise(ctx, c); err != nil {
			log.Debug("an encoder is listed but did not encode",
				"encoder", c.Encoder, "codec", c.Codec, "accel", c.Accel, "error", err)
			continue
		}
		byName[c.Capability()] = Held{
			Name:     c.Capability(),
			Source:   SourceProbe,
			Detail:   "encoded a test pattern with " + c.Encoder,
			ProvedAt: now().UTC(),
		}
		codecs[c.Codec] = true
	}

	// The rollup. Derived only from leaves that passed, so "can this node
	// encode AV1 at all" cannot become true because AV1 was listed somewhere.
	for codec := range codecs {
		name := CodecCapability(codec)
		if _, exists := byName[name]; exists {
			// A software candidate already occupies the rollup name; its
			// detail names the encoder that proved it, which is better.
			continue
		}
		byName[name] = Held{
			Name:     name,
			Source:   SourceProbe,
			Detail:   "at least one " + codec + " encoder is usable",
			ProvedAt: now().UTC(),
		}
	}

	return sortHeld(byName)
}

// Names lists the capability strings, sorted, for logging and comparison.
func Names(held []Held) []string {
	out := make([]string, 0, len(held))
	for _, h := range held {
		out = append(out, h.Name)
	}
	sort.Strings(out)
	return out
}

func sortHeld(byName map[string]Held) []Held {
	out := make([]Held, 0, len(byName))
	for _, h := range byName {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Merge combines held sets, first writer wins on a duplicate name.
//
// It is how the startup-resolved binary capabilities and the freshly probed
// hardware ones become one advertisement without either half having to know
// about the other.
func Merge(sets ...[]Held) []Held {
	byName := map[string]Held{}
	for _, set := range sets {
		for _, h := range set {
			if _, exists := byName[h.Name]; !exists {
				byName[h.Name] = h
			}
		}
	}
	return sortHeld(byName)
}

// ParseEncoderList reads the names out of `ffmpeg -encoders` output.
//
// The format is a header, a separator line of dashes, then one encoder per line:
//
//	V....D av1_qsv              AV1 (Intel Quick Sync Video acceleration)
//	V..... libx264              libx264 H.264 / AVC / MPEG-4 AVC
//
// The flags field is fixed-width and its first character is the media type, so
// a line is an encoder line when its first field is exactly six characters of
// flags. Anything else is banner or heading, and is skipped rather than being
// guessed at — a heading mistaken for an encoder would produce a candidate that
// can never be exercised, which costs a process launch per beat and nothing else.
func ParseEncoderList(out string) []string {
	var names []string
	started := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "------") {
			started = true
			continue
		}
		if !started {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || len(fields[0]) != 6 {
			continue
		}
		names = append(names, fields[1])
	}
	return names
}

// Advertisement is one worker saying what it currently holds, and for how long
// that claim should be believed.
//
// It is a whole set, never a delta, and that is the design decision that makes
// NARROWING structural rather than something a caller has to remember. A
// capability that stops passing simply is not in the next advertisement, and
// the writer's job is to make the stored set equal this one — including the
// removals. An interface that took "these are my new capabilities" could only
// ever grow, and an advertisement that can only grow is one that lies after the
// first driver update.
type Advertisement struct {
	// WorkerID is the advertising worker — the job queue's lease owner, unique
	// per process.
	WorkerID string
	// PeerID and PeerName say which NODE this worker runs on, which is the unit
	// the fleet view answers about.
	PeerID   string
	PeerName string
	// Held is the whole set. Empty is meaningful: it says this worker can do
	// none of the things anybody routes on, which is a supported state
	// (ADR-0023) and must be recordable rather than indistinguishable from
	// never having advertised.
	Held []Held
	// TTL is how long these claims survive without being renewed. It travels
	// with the advertisement rather than being a constant in the reader,
	// because a worker that re-verifies every five minutes and one that
	// re-verifies every hour must not share a deadline chosen elsewhere.
	TTL time.Duration
}

// Change is what one advertisement did to what was stored.
//
// Lost is the interesting half and the reason this is returned at all: gaining
// a capability is unremarkable, and losing one without the binary changing and
// without a restart is the event an operator needs to see.
type Change struct {
	Gained []string
	Lost   []string
}

// Empty says nothing changed, which is the normal outcome of a beat.
func (c Change) Empty() bool { return len(c.Gained) == 0 && len(c.Lost) == 0 }

// Advertised is one worker's live advertisement, as read back.
type Advertised struct {
	WorkerID  string
	PeerID    string
	PeerName  string
	Held      []Held
	ExpiresAt time.Time
}
