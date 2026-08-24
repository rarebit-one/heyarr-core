package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/client"
)

// Playing something, from a terminal (§68).
//
// # Why these talk to the controller instead of to the television
//
// Everything interesting happens server-side, and not for tidiness. SSDP
// discovery is multicast and does not cross a routed link, so a laptop on a
// VPN finds nothing at all; and a Samsung's DLNA renderer refuses control from
// off its own subnet — 401 from a tunnel address, 200 from a host on the same
// /24 — while fetching content from anywhere quite happily. A CLI that spoke
// UPnP itself would work only from a machine already sitting in the living
// room, and would stop when the terminal closed.
//
// So these are four thin calls onto /api/v1/renderers. The same endpoints back
// the MCP tools, which means a person at a prompt and a model in a chat cannot
// end up able to do different things.
//
// # The renderer is named, never identified
//
// Every command takes a name — "living room" — matched against what the device
// calls itself. A UDN is accepted too, for scripts, but nobody is going to
// type uuid:9cf4b79e-8ddf-4f8d-a3e3-9266fb4f5484 at a prompt.

func newPlayCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "play <asset-id> <renderer>",
		Short: "Play an asset on a television, speaker or projector (§68)",
		Long: `Play an asset on a renderer found on the controller's network.

The renderer is named the way you would say it out loud — "living room" — and
matched against what the device calls itself. Run ` + "`heyarr renderers discover`" + `
to see what is there.

This plans the playback first, and a plan that is not DIRECT is refused with
the reason: your device does not declare this codec, or the only replica is on
another peer. That refusal is the answer, not an error to work around.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				rend, err := c.ResolveRenderer(ctx, args[1])
				if err != nil {
					return err
				}
				result, err := c.PlayOnRenderer(ctx, rend.UDN, args[0])
				if err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), result)
				}
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "playing %s\n", orDash(result.Playing))
				fmt.Fprintf(out, "  on          %s\n", orDash(result.On))
				fmt.Fprintf(out, "  session     %s\n", result.SessionID)
				// The decision is printed even when it is `direct`, because
				// "why is my television transcoding this" is the question this
				// class of software gets asked most, and the answer should not
				// require a second command.
				fmt.Fprintf(out, "  decision    %s\n", orDash(result.Decision))
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}

// newRenderersTransportCommand builds pause, resume, stop and seek.
//
// They are grouped under `renderers` rather than promoted to top-level verbs:
// `heyarr pause` reads like it pauses Heyarr. Only `play` is top-level,
// because that is the one anybody types.
func newRenderersTransportCommands(_ Options, configPath *string) []*cobra.Command {
	simple := []struct {
		verb, short, long string
	}{
		{
			verb:  "pause",
			short: "Hold position on a renderer",
			long:  "Hold position. Use this when someone is coming back; `stop` releases the content.",
		},
		{
			verb:  "resume",
			short: "Continue a paused renderer",
			long: "Continue from where it was paused.\n\n" +
				"This is UPnP's Play verb — there is no separate resume: Play from paused\ncontinues, and Play from stopped restarts.",
		},
		{
			verb:  "stop",
			short: "Stop a renderer and release the content",
			long:  "End playback. The session keeps the position it had reached, so it can be resumed later from the catalog.",
		},
	}

	cmds := make([]*cobra.Command, 0, len(simple)+2)
	for _, s := range simple {
		var flags clientFlags
		cmd := &cobra.Command{
			Use:   s.verb + " <renderer>",
			Short: s.short,
			Long:  s.long,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
					rend, err := c.ResolveRenderer(ctx, args[0])
					if err != nil {
						return err
					}
					status, err := c.ControlRenderer(ctx, rend.UDN, s.verb)
					if err != nil {
						return err
					}
					return emitStatus(cmd.OutOrStdout(), status, flags.asJSON)
				})
			},
		}
		flags.register(cmd)
		cmds = append(cmds, cmd)
	}

	var seekFlags clientFlags
	seek := &cobra.Command{
		Use:   "seek <renderer> <position>",
		Short: "Jump to a position",
		Long: `Jump to a position, given as seconds or as [h:]mm:ss.

The position is ABSOLUTE — measured from the start — not a delta from where
the renderer is now. "90", "1:30" and "0:01:30" are the same place.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			seconds, err := parsePosition(args[1])
			if err != nil {
				return err
			}
			return seekFlags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				rend, err := c.ResolveRenderer(ctx, args[0])
				if err != nil {
					return err
				}
				status, err := c.SeekRenderer(ctx, rend.UDN, seconds)
				if err != nil {
					return err
				}
				return emitStatus(cmd.OutOrStdout(), status, seekFlags.asJSON)
			})
		},
	}
	seekFlags.register(seek)
	cmds = append(cmds, seek)

	var statusFlags clientFlags
	status := &cobra.Command{
		Use:   "status <renderer>",
		Short: "Report what a renderer is playing and how far in",
		Long: `Report transport state and position.

A renderer that reports no position is not broken. Some report none until they
have parsed enough of the stream, and some never report one at all — the field
is simply omitted rather than shown as zero.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return statusFlags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				rend, err := c.ResolveRenderer(ctx, args[0])
				if err != nil {
					return err
				}
				st, err := c.RendererStatusFor(ctx, rend.UDN)
				if err != nil {
					return err
				}
				return emitStatus(cmd.OutOrStdout(), st, statusFlags.asJSON)
			})
		},
	}
	statusFlags.register(status)
	return append(cmds, status)
}

func emitStatus(w io.Writer, s client.RendererStatus, asJSON bool) error {
	if asJSON {
		return emitJSON(w, s)
	}
	fmt.Fprintf(w, "  renderer    %s\n", orDash(s.Renderer))
	fmt.Fprintf(w, "  state       %s\n", orDash(s.State))
	// Elapsed is omitted rather than printed as 0:00:00 when the device did
	// not report one. Zero and "would not say" are different facts, and a
	// resume decided from the wrong one starts a film over.
	if s.Elapsed > 0 || s.Duration > 0 {
		line := formatPosition(s.Elapsed)
		if s.Duration > 0 {
			line += " of " + formatPosition(s.Duration)
		}
		fmt.Fprintf(w, "  position    %s\n", line)
	}
	return nil
}

// parsePosition reads seconds, or [h:]mm:ss.
//
// Both spellings are accepted because both are natural: a script computes
// seconds, and a person reads a timestamp off a screen.
func parsePosition(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("give a position, as seconds or [h:]mm:ss")
	}
	if !strings.Contains(s, ":") {
		seconds, err := strconv.ParseFloat(s, 64)
		if err != nil || seconds < 0 {
			return 0, fmt.Errorf("%q is not a position — give seconds, or [h:]mm:ss", s)
		}
		return seconds, nil
	}

	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, fmt.Errorf("%q is not a position — give seconds, or [h:]mm:ss", s)
	}
	var total float64
	for _, p := range parts {
		n, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%q is not a position — give seconds, or [h:]mm:ss", s)
		}
		total = total*60 + n
	}
	return total, nil
}

// formatPosition renders seconds as h:mm:ss.
func formatPosition(seconds float64) string {
	d := time.Duration(seconds * float64(time.Second))
	total := int64(d / time.Second)
	return fmt.Sprintf("%d:%02d:%02d", total/3600, (total%3600)/60, total%60)
}

// orDash renders an empty string as a dash.
//
// Separate from dash(), which takes a *string: these fields are values that a
// server may legitimately leave blank, not pointers that may be absent, and
// conflating the two would mean taking the address of a loop variable to
// print a word.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
