// Package media resolves the external audiovisual toolchain — FFmpeg and
// ffprobe — and reports what this node can therefore do (spec §10, ADR-0023).
//
// # The toolchain is optional, and that is the whole design
//
// There is no ffmpeg or ffprobe on the development machines, on the CI runners
// or on the deployment host. Heyarr is a single static Go binary that people
// copy onto a NAS; requiring a 45 MB C toolchain before it will start would
// turn "run Heyarr" into "run Heyarr, and first solve packaging".
//
// So a node without the toolchain is a supported configuration, not a broken
// one. It scans, ingests, catalogues, verifies, garbage-collects and serves
// byte ranges exactly as before. What it cannot do is probe media (§29) and
// remux it (§10) — and those are expressed as JOB CAPABILITIES rather than as
// startup requirements, so the jobs that need them stay pending and visible
// instead of failing, and everything else carries on.
//
// §29 already establishes the shape of this: Range probing preferred,
// whole-blob materialisation as the fallback. A further rung down to "no
// probing at all on this node" is the same kind of decision, not a new one.
//
// # Configured versus discovered
//
// The two cases are deliberately not symmetrical, and the asymmetry is the
// interesting part of this package:
//
//   - A path given in configuration that does not resolve, or resolves to
//     something that does not answer `-version` like ffprobe does, is a
//     STARTUP ERROR. Someone said which binary to use. Silently using a
//     different one, or silently using none, is worse than not starting.
//   - A binary merely absent from PATH is not an error. Nobody asked for it,
//     so the node degrades and says so.
//
// Getting that backwards in either direction is the failure this package
// exists to prevent: a mandatory toolchain that nobody can install, or a
// configured path that is quietly ignored.
package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// Capability names, as advertised by a worker and required by a job (§75,
// ADR-0008). These strings are a wire contract between the worker's
// advertisement and the job's RequiredCapability, so they live here rather than
// being spelled out at each end.
const (
	CapabilityFFprobe = "ffprobe"
	CapabilityFFmpeg  = "ffmpeg"
)

// versionTimeout bounds `-version`. It is generous because the binary is 45 MB
// and may be on cold spinning storage the first time it is executed, and it
// exists at all because a startup path that can hang forever is worse than one
// that fails: an operator watching a service that never becomes ready has
// nothing to go on.
const versionTimeout = 10 * time.Second

// Tool is one external binary and what is known about it.
type Tool struct {
	// Name is the binary this describes: "ffprobe" or "ffmpeg".
	Name string
	// Path is where it was found. Empty when it was not.
	Path string
	// Version is what it reported, e.g. "6.1.1". Empty when unavailable.
	Version string
	// Available says whether it can be used. When false, Detail says why in a
	// few words.
	Available bool
	// Detail explains an unavailable tool: "not found on PATH", "did not
	// report a version". It is for an operator reading /api/v1/system, so it
	// says what happened rather than quoting an error.
	Detail string
}

// Toolchain is what this node resolved at startup.
//
// It is a value, captured once. The binaries are not re-resolved per job:
// installing ffmpeg under a running Heyarr and expecting it to be noticed is
// not a supported flow, and polling for it would mean every job carried the
// possibility of a different answer than the one the node advertised. Restart
// after installing it — which is what an operator does anyway.
type Toolchain struct {
	FFprobe Tool
	FFmpeg  Tool
}

// Tools returns both, in a stable order, for reporting.
func (t Toolchain) Tools() []Tool { return []Tool{t.FFprobe, t.FFmpeg} }

// Capabilities is what a worker built on this toolchain may advertise (§75).
//
// A node with neither binary advertises nothing, which is not the same as
// advertising an empty restriction: the job queue treats an empty capability
// list on a CLAIM as "no capabilities held", so a job requiring one is simply
// never claimed. That is the degrade path, and it is the queue's existing
// behaviour rather than anything special added here.
func (t Toolchain) Capabilities() []string {
	// Non-nil even when empty, so that "advertises nothing" and "was never
	// asked" are distinguishable wherever this is rendered. A nil slice logs
	// and marshals as null, which reads as "unset" — and the whole point of
	// this value is that "nothing" is a deliberate, reportable state.
	out := []string{}
	if t.FFprobe.Available {
		out = append(out, CapabilityFFprobe)
	}
	if t.FFmpeg.Available {
		out = append(out, CapabilityFFmpeg)
	}
	return out
}

// Options configure Resolve.
type Options struct {
	// FFprobePath and FFmpegPath name the binaries explicitly. Empty means
	// "look on PATH", and an empty result there is a degraded node rather than
	// an error. A non-empty value that does not work IS an error — see the
	// package comment.
	FFprobePath string
	FFmpegPath  string
	Logger      *slog.Logger

	// LookPath resolves a bare tool name against PATH. Nil means exec.LookPath.
	//
	// It is injectable because the most important case cannot otherwise be
	// tested where it matters: "this machine has no FFmpeg" is untestable on a
	// machine that has FFmpeg, and CI's Linux runners deliberately do. Without
	// a seam, the degrade path would be tested only on whichever runner
	// happened to lack the binary — which is to say, tested by accident.
	LookPath func(name string) (string, error)

	// runVersion is a narrower seam, kept unexported because a real executable
	// in a t.TempDir expresses everything it can express.
	runVersion func(ctx context.Context, path string) (string, error)
}

// Resolve locates the toolchain.
//
// It returns an error only for a configured path that does not work. Every
// other outcome is a Toolchain describing what is and is not available, which
// callers report rather than treat as a failure.
func Resolve(ctx context.Context, opts Options) (Toolchain, error) {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("component", "media")

	look := opts.LookPath
	if look == nil {
		look = exec.LookPath
	}
	run := opts.runVersion
	if run == nil {
		run = runVersion
	}

	probeTool, err := resolveOne(ctx, CapabilityFFprobe, opts.FFprobePath, look, run, log)
	if err != nil {
		return Toolchain{}, err
	}
	ffmpegTool, err := resolveOne(ctx, CapabilityFFmpeg, opts.FFmpegPath, look, run, log)
	if err != nil {
		return Toolchain{}, err
	}
	return Toolchain{FFprobe: probeTool, FFmpeg: ffmpegTool}, nil
}

func resolveOne(
	ctx context.Context,
	name, configured string,
	look func(string) (string, error),
	run func(context.Context, string) (string, error),
	log *slog.Logger,
) (Tool, error) {
	path := configured
	if path == "" {
		found, err := look(name)
		if err != nil {
			log.Info("the media toolchain is incomplete; jobs that need it will wait",
				"tool", name, "reason", "not found on PATH")
			return Tool{Name: name, Detail: "not found on PATH"}, nil
		}
		path = found
	}

	version, err := run(ctx, path)
	if err != nil {
		if configured != "" {
			// Someone named this binary. Refusing to start is the honest
			// response: the alternative is a node that was told to use a
			// specific ffprobe and quietly used none.
			return Tool{}, fmt.Errorf("media: %s at %s is configured but unusable: %w", name, path, err)
		}
		log.Warn("something on PATH answers to this name but is not usable",
			"tool", name, "path", path, "error", err)
		return Tool{Name: name, Detail: "did not report a version"}, nil
	}

	log.Info("resolved a media tool", "tool", name, "path", path, "version", version)
	return Tool{Name: name, Path: path, Version: version, Available: true}, nil
}

// runVersion executes `<path> -version` and parses what it says.
func runVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()

	// #nosec G204 -- path is either an operator-configured value or the result
	// of exec.LookPath. Both are, by construction, "the binary this operator
	// wants run"; there is no untrusted input on this line.
	out, err := exec.CommandContext(ctx, path, "-version").Output()
	if err != nil {
		return "", fmt.Errorf("running -version: %w", err)
	}
	return parseVersion(string(out))
}

// errNoVersion is what an unparseable banner produces.
var errNoVersion = errors.New("no version in the -version output")

// parseVersion reads the version out of an FFmpeg banner, whose first line is
//
//	ffprobe version 6.1.1 Copyright (c) 2007-2023 the FFmpeg developers
//
// A binary that prints nothing useful must NOT resolve to an empty version
// reported as success. That is the specific way this goes wrong: `/bin/true`
// exits 0, prints nothing, and would otherwise be a perfectly available
// ffprobe with version "" — which then fails at the first probe, hours later,
// looking like a probing bug rather than a configuration one.
func parseVersion(banner string) (string, error) {
	line, _, _ := strings.Cut(banner, "\n")
	fields := strings.Fields(line)
	// "<name> version <v> ..." — anything shorter is not a banner.
	if len(fields) < 3 || fields[1] != "version" {
		return "", errNoVersion
	}
	v := strings.TrimPrefix(fields[2], "n") // git builds print n6.1.1
	if v == "" {
		return "", errNoVersion
	}
	return v, nil
}
