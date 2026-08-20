// Package ffmpeg wraps FFmpeg for the one audiovisual operation Milestone 2
// needs: a container remux (spec §10, §75).
//
// # What this deliberately is not
//
// It re-wraps encoded streams. It does not re-encode, and there is no quality
// ladder, no HLS or DASH segmenting, no subtitle burn-in, no hardware
// acceleration and no adaptive bitrate. Those are later milestones, and §84's
// ordering is load-bearing precisely because a transcode ladder built before
// the peer model and placement policy exist has to be rebuilt around them.
//
// A remux is the case the planner returns most often and the one that costs
// almost nothing to serve: the device can play these streams, it just cannot
// open this box.
//
// # -c copy is the whole contract
//
// If this ever re-encodes, it has stopped being a remux and started being a
// transcode with none of a transcode's decisions made — no quality target, no
// ladder, no thought about what the device can actually take. A test compares
// stream-level checksums before and after for exactly that reason: a "remux"
// that silently re-encoded looks perfectly fine from the outside.
package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Capability is what a worker must advertise to run one (ADR-0023).
const Capability = "ffmpeg"

// Container is an output container a remux can target.
type Container string

const (
	// ContainerMP4 is the safe default: almost every device takes it.
	ContainerMP4 Container = "mp4"
	// ContainerMatroska takes almost every stream.
	ContainerMatroska Container = "mkv"
)

// Containers is every supported target, in a stable order.
func Containers() []Container { return []Container{ContainerMP4, ContainerMatroska} }

// ParseContainer validates a target container.
func ParseContainer(s string) (Container, error) {
	for _, c := range Containers() {
		if string(c) == s {
			return c, nil
		}
	}
	return "", fmt.Errorf("ffmpeg: container must be one of mp4, mkv, not %q", s)
}

// Extension is the filename suffix for a container.
func (c Container) Extension() string { return "." + string(c) }

// defaultTimeout bounds one remux. Generous because it covers copying a large
// file twice — in and out — over whatever storage the operator has, and a
// remux that is merely slow must not be killed and retried into the same wall.
const defaultTimeout = 2 * time.Hour

// Options configure a Remuxer.
type Options struct {
	// FFmpegPath is the resolved binary (ADR-0023). Required.
	FFmpegPath string
	// WorkDir is where output is written before it is taken into the store.
	WorkDir string
	Timeout time.Duration
	Logger  *slog.Logger
}

// Remuxer rewraps streams into a different container.
type Remuxer struct {
	bin     string
	workDir string
	timeout time.Duration
	log     *slog.Logger
}

// New builds a Remuxer.
func New(opts Options) (*Remuxer, error) {
	if opts.FFmpegPath == "" {
		return nil, errors.New("ffmpeg: a binary path is required")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Remuxer{
		bin: opts.FFmpegPath, workDir: opts.WorkDir, timeout: timeout,
		log: log.With("component", "ffmpeg"),
	}, nil
}

// ErrRemuxFailed means FFmpeg could not rewrap the input.
//
// Typed so the job layer can tell "these streams cannot go in that container"
// — a permanent condition no retry fixes — from "the disk filled up", which
// one might.
var ErrRemuxFailed = errors.New("ffmpeg: the remux failed")

// Result is what a remux produced.
type Result struct {
	// Path is the output file. The caller owns it and must remove it.
	Path string
	Size int64
	// Elapsed is how long FFmpeg took, for the operator asking why a queue is
	// backed up.
	Elapsed time.Duration
}

// Remux rewraps srcPath into target, writing a new file.
//
// It never modifies the input. The source is a blob in the CAS and blobs are
// immutable (§14): a remux that wrote in place would corrupt the thing every
// other asset sharing those bytes points at.
func (r *Remuxer) Remux(ctx context.Context, srcPath string, target Container) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	out, err := os.CreateTemp(r.workDir, "heyarr-remux-*"+target.Extension())
	if err != nil {
		return Result{}, fmt.Errorf("ffmpeg: creating the output file: %w", err)
	}
	path := out.Name()
	// FFmpeg writes the file itself; this only reserved the name.
	if err := out.Close(); err != nil {
		return Result{}, err
	}

	started := time.Now()
	// #nosec G204 -- the binary is the resolved toolchain (ADR-0023) and every
	// argument is either a literal or a path this process created.
	cmd := exec.CommandContext(ctx, r.bin, remuxArgs(srcPath, path, target)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Leave nothing behind. A failed remux that left a partial file would
		// be indistinguishable from a finished one to anything that only
		// checked for existence.
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			r.log.Warn("a failed remux left a file behind", "path", path, "error", rmErr)
		}
		detail := strings.TrimSpace(stderr.String())
		if ctx.Err() != nil {
			return Result{}, fmt.Errorf("%w: timed out: %s", ErrRemuxFailed, detail)
		}
		return Result{}, fmt.Errorf("%w: %s", ErrRemuxFailed, detail)
	}

	info, err := os.Stat(path)
	if err != nil {
		return Result{}, fmt.Errorf("ffmpeg: the output is not there: %w", err)
	}
	return Result{Path: path, Size: info.Size(), Elapsed: time.Since(started)}, nil
}

// remuxArgs is the command line, in one place so it can be read and tested.
//
// Every flag here is load-bearing:
//
//	-nostdin       FFmpeg reads stdin by default and a worker has none; without
//	               this a remux can block forever on a terminal that is not there.
//	-map 0         take every stream, not just the first of each kind — dropping
//	               the second audio track or the subtitles would be a silent
//	               downgrade nobody asked for.
//	-c copy        THE contract. Anything else is a transcode wearing this
//	               function's name.
//	-movflags      MP4 only: put the moov at the front, so the result can be
//	  +faststart   probed and played over ranges without reading to the end
//	               (§29, ADR-0013).
func remuxArgs(src, dst string, target Container) []string {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-i", src,
		"-map", "0",
		"-c", "copy",
	}
	if target == ContainerMP4 {
		args = append(args, "-movflags", "+faststart")
	}
	return append(args, dst)
}

// TargetFor picks the container to remux into for a device.
//
// It prefers whichever of the supported targets the device declared, MP4
// first: MP4 is the more widely accepted of the two and the one a device is
// likelier to hardware-decode. A device declaring neither gets MP4 — remuxing
// into something it did not ask for is a guess, but it is a better guess than
// leaving the file in a container it definitely refused.
func TargetFor(declared []string) Container {
	for _, c := range Containers() {
		for _, d := range declared {
			if strings.EqualFold(strings.TrimSpace(d), string(c)) {
				return c
			}
		}
	}
	return ContainerMP4
}

// Base is the filename part of a path, for logging.
func Base(p string) string { return filepath.Base(p) }

// JobType is the queue's name for a remux (§75).
//
// It is called "transcode" because that is what §75 lists, and because the
// same job will carry a real transcode in a later milestone. Milestone 2 only
// ever asks it for a remux, and the payload says which.
const JobType = "transcode"

// Payload is the transcode job's payload.
type Payload struct {
	// BlobHash is the source bytes.
	BlobHash string `json:"blob_hash"`
	// AssetID is the asset being derived from, so the result can be attached
	// to the same edition.
	AssetID string `json:"asset_id"`
	// Container is the target. Milestone 2 supports remuxing only, so this is
	// always a container change.
	Container Container `json:"container"`
}

// DedupeKey makes the job idempotent: asking twice for the same output while
// the first is still live yields one job (ADR-0008).
func DedupeKey(blobHash string, target Container) string {
	return "transcode:" + blobHash + ":" + string(target)
}

// DerivedRole is the assets.role given to a remux output.
//
// It is a role rather than a separate table because a remux IS an asset: a
// usable local representation of the same Edition (§11). Giving it a role
// means replication, integrity and garbage collection treat it like every
// other managed asset without a special case — which is the behaviour we want
// and would otherwise have to write four times.
const DerivedRole = "derived"
