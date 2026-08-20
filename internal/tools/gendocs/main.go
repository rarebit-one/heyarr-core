// Command gendocs writes the CLI reference into a directory.
//
// It is a separate main package rather than a hidden `heyarr docs` subcommand
// so that cobra's documentation generator — which exists to produce files at
// build time — is not linked into the shipped binary, and so that it cannot be
// run by accident against a production install's working directory.
//
// It is invoked by scripts/gen.sh, which CI runs before asserting that nothing
// generated has drifted.
package main

import (
	"fmt"
	"os"

	"github.com/rarebit-one/heyarr-core/internal/clidocs"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gendocs <output-directory>")
		os.Exit(2)
	}
	if err := clidocs.Generate(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "gendocs: %v\n", err)
		os.Exit(1)
	}
}
