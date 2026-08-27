// Package voidbinddep anchors heyarr-core's module dependency on
// github.com/rarebit-one/voidbind-go while the identity packages are being
// converted to shims that forward into it (Track C, the extract-first
// migration). Until those shims land, nothing else imports the library, so a
// `go mod tidy` would prune the require and CI would never exercise the
// private-module fetch — the one thing that most needs proving first.
//
// TODO(track-c): delete this file once the identity packages import
// voidbind-go directly.
package voidbinddep

import _ "github.com/rarebit-one/voidbind-go/identity"
