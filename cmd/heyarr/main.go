// Command heyarr is the single Heyarr binary. Every role — controller, worker
// and peer — is a subcommand of it; see docs/adr/0002.
//
// This file contains wiring only. Logic belongs in internal packages.
package main

import "github.com/rarebit-one/heyarr-core/internal/cli"

func main() { cli.Main() }
