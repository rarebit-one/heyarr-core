package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
)

// Execute runs the heyarr command tree and returns the process exit code.
//
// The command tree is deliberately thin: roles (controller, worker, peer, all)
// are wired here and nowhere else, so that a single-process `heyarr all` and a
// multi-process deployment share exactly one wiring path. See docs/adr/0002.
func Execute(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		usage(stderr)
		return 2
	}
	switch args[1] {
	case "version":
		return version(args[2:], stdout)
	case "controller", "worker", "peer", "all":
		fmt.Fprintf(stderr, "heyarr %s: not implemented yet — see milestone 1\n", args[1])
		return 69 // EX_UNAVAILABLE
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "heyarr: unknown command %q\n", args[1])
		usage(stderr)
		return 2
	}
}

func version(args []string, w io.Writer) int {
	info := buildinfo.Get()
	for _, a := range args {
		if a == "--json" {
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			if err := enc.Encode(info); err != nil {
				return 1
			}
			return 0
		}
	}
	fmt.Fprintf(w, "heyarr %s (%s, built %s, %s)\n", info.Version, info.Commit, info.Date, info.GoVersion)
	return 0
}

func usage(w io.Writer) {
	fmt.Fprint(w, `heyarr — self-hosted content lifecycle, replication and consumption

Usage:
  heyarr <command> [flags]

Roles:
  controller   own coordinated mutable state: catalog, policy, jobs, API
  worker       execute leased jobs
  peer         serve and replicate bytes
  all          run every role in one process (small deployments)

Commands:
  version      print build information (--json for machine output)
  help         show this message

Heyarr is pre-alpha; the roles above are not implemented yet.
`)
}

// Main is the process entry point, split out so it is testable.
func Main() {
	os.Exit(Execute(os.Args, os.Stdout, os.Stderr))
}
