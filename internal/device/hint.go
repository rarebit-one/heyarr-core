package device

// The device store itself — keys, records, certs, rendering — is voidbind-go's
// (github.com/rarebit-one/voidbind-go/device), and callers import it directly.
// What stays here is the one piece that is heyarr's own: the name a device
// rendering tells the operator to run.

import vb "github.com/rarebit-one/voidbind-go/device"

// CommandName is the binary heyarr presents as (matches root.go's `Use`). It is
// here so every place that renders a device reads the same name from one spot.
const CommandName = "heyarr"

// CommandHint is the option every heyarr device rendering passes so voidbind-go's
// `authorises` caveat names `heyarr …`, not the voidbind CLI the operator does
// not have (#369). One instance, shared by every render site across the CLI and
// the Personal MCP.
var CommandHint = vb.WithCommandName(CommandName)

// NotYetAuthorisingFor renders the un-enrolled caveat for a command name, for
// the one caller that has no Device to render: the Personal MCP's LIST response,
// whose top-level `authorises` must still speak for an empty list. voidbind-go
// exposes the parameterised string only through a Device, so we ask a zero
// (un-enrolled) device — its empty cert makes AuthorisationNote return exactly
// this caveat. Keeping the trick here means no render site repeats it.
func NotYetAuthorisingFor(opts ...vb.HintOption) string {
	return vb.Device{}.AuthorisationNote(opts...)
}
