// Multi-range blob reads (replication and a web-seed ask for several pieces in
// one request; the client blob route answers multipart/byteranges) rest on
// net/http.ServeContent. Go 1.26.9 / 1.27.2 (CVE-2026-78667) cap the ranges a
// Range header may carry, and key the cap's default to the module's go line:
// 200 for a module on 1.26+, but ONE for an older module — which this one is —
// so a three-range request came back 200 with the whole blob. Say the bound
// out loud rather than inherit it from the go directive: Go's own current
// default, which is plenty for a transfer and still bounds the CPU the fix
// exists to bound.
//
//go:debug httpservecontentmaxranges=200

// Command mnemosyne is the personal-media service binary (ADR-0107). It runs
// only the controller role in personal profile: encrypted vaults, drive CRDT,
// placement pins and grants — no libraries, no scanner, no media stack.
//
// This file contains wiring only. Logic belongs in internal packages.
package main

import "github.com/rarebit-one/heyarr-core/internal/cli"

func main() { cli.MnemosyneMain() }
