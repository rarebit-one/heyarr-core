package probe

import (
	"context"
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
			return nil, fmt.Errorf("%s timed out: %s", bin, detail)
		}
		return nil, fmt.Errorf("%s failed: %s", bin, detail)
	}
	return out, nil
}
