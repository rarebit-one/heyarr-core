package probe

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// runCommand executes ffprobe and returns its stdout.
//
// stderr is folded into the error rather than discarded: ffprobe's diagnostics
// are the only explanation of why a file could not be read, and an exit status
// alone turns "moov atom not found" into "exit status 1".
func runCommand(ctx context.Context, bin string, args ...string) ([]byte, error) {
	// #nosec G204 -- bin is the operator-configured or PATH-resolved ffprobe
	// (ADR-0023) and the arguments are literals plus a target this package
	// constructed. There is no untrusted input on this line.
	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		// The context deadline is reported distinctly, because "it hung" and
		// "it refused" need different responses and both otherwise arrive as
		// "exit status 1".
		if ctx.Err() != nil {
			return nil, &commandError{msg: fmt.Sprintf("%s timed out: %s", bin, detail), err: err, timedOut: true}
		}
		return nil, &commandError{msg: fmt.Sprintf("%s failed: %s", bin, detail), err: err}
	}
	return out, nil
}

// commandError keeps the message runCommand has always produced and the cause
// beneath it, so a caller can tell ffprobe RUNNING and rejecting its input (an
// *exec.ExitError within the deadline) from ffprobe never finishing or never
// starting (#670).
type commandError struct {
	msg      string
	err      error
	timedOut bool
}

func (e *commandError) Error() string { return e.msg }
func (e *commandError) Unwrap() error { return e.err }

// rejectedInput reports whether err is ffprobe having run to completion and
// exited non-zero: it read the input and refused it. A timeout, a binary that
// would not start, or a cancelled context is a property of the moment.
func rejectedInput(err error) bool {
	var ce *commandError
	if !errors.As(err, &ce) || ce.timedOut {
		return false
	}
	var exit *exec.ExitError
	return errors.As(ce.err, &exit)
}
