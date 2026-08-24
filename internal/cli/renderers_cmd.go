package cli

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/domain/playback"
	"github.com/rarebit-one/heyarr-core/internal/renderer"
)

// newRenderersCommand builds `heyarr renderers`.
//
// # Why this is not `heyarr devices`
//
// A Device (§68) is a capability profile the controller stores and the planner
// reads. A renderer is a box on the local network that may be unplugged
// tomorrow. They are related — a discovered renderer is the best possible
// source for a Device profile — and they are not the same noun, so conflating
// them would leave no way to say "this television exists but is not registered".
//
// `heyarr device` is a third thing again: this machine's Ed25519 identity
// (ADR-0032). Three nouns, three names, none of them plural of another.
//
// # This talks to the network, not to the controller
//
// Discovery is multicast on the local segment, so it only finds what is on the
// same LAN as the process running it. That makes the CLI the right place for
// an operator to run it from a laptop in the living room, and it also means a
// result here is not what the peer would see if the peer is somewhere else.
// The command says so rather than letting someone discover it the hard way.
func newRenderersCommand(opts Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "renderers",
		Short: "Find media renderers on the local network (§68)",
		Long: `Find UPnP MediaRenderers — televisions, speakers, projectors — on the network
this machine is attached to.

A renderer is not yet a Device. Discovery reports what answered and what it
says it can play; registering one as a Device, so the planner can decide
against it, is a separate step.

Discovery is multicast and does not leave the local segment: run it on the same
network as the screen you are looking for.`,
	}
	cmd.AddCommand(newRenderersDiscoverCommand(opts))
	// The transport verbs live here rather than at the top level:
	// `heyarr pause` reads like it pauses Heyarr. Only `play` is
	// promoted, because that is the one anybody types.
	cmd.AddCommand(newRenderersTransportCommands(opts, configPath)...)
	return cmd
}

func newRenderersDiscoverCommand(_ Options) *cobra.Command {
	var (
		timeout     time.Duration
		withProfile bool
		asJSON      bool
	)
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "Search the local network for media renderers",
		Long: `Search the local network for UPnP MediaRenderers.

An empty result means nothing answered within the search window — NOT that
there is nothing there. A television in standby closes every listener and on
some models leaves the network entirely, so a screen that is switched off is
indistinguishable from a screen that does not exist. Switch it on and search
again before concluding anything.

With --profile, each renderer is also asked what formats it accepts, and the
answer is mapped into the capability profile the playback planner uses. That is
one extra round trip per device.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			client := &http.Client{Timeout: 15 * time.Second}

			found, problems := renderer.DiscoverRenderers(ctx, client, renderer.DiscoverOptions{
				Timeout: timeout,
			})
			// Problems are reported and do not fail the command: one
			// unreachable device must not hide the others, and the exit code
			// should mean "discovery ran", not "everything on the LAN was
			// healthy".
			for _, p := range problems {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", p)
			}

			views := make([]rendererView, 0, len(found))
			for _, r := range found {
				v := rendererView{
					UDN:          r.UDN,
					Name:         r.FriendlyName,
					Manufacturer: r.Manufacturer,
					Model:        r.ModelName,
					Location:     r.Location,
					AVTransport:  r.AVTransport.Type,
				}
				if withProfile {
					profile, err := renderer.FetchProfile(ctx, client, r)
					if err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s: %v\n", r.FriendlyName, err)
					} else {
						v.Profile = &profile
					}
				}
				views = append(views, v)
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return encodeJSON(w, views)
			}
			printRenderers(w, views, withProfile)
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Second,
		"how long to listen for answers; devices may wait up to 3s before replying")
	cmd.Flags().BoolVar(&withProfile, "profile", false,
		"also ask each renderer what it can play")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

// rendererView is the `--json` shape.
type rendererView struct {
	UDN          string                  `json:"udn"`
	Name         string                  `json:"name"`
	Manufacturer string                  `json:"manufacturer"`
	Model        string                  `json:"model"`
	Location     string                  `json:"location"`
	AVTransport  string                  `json:"av_transport"`
	Profile      *playback.DeviceProfile `json:"profile,omitempty"`
}

func printRenderers(w io.Writer, views []rendererView, withProfile bool) {
	if len(views) == 0 {
		fmt.Fprintln(w, "nothing answered — a screen in standby is invisible to discovery, so switch it on and try again")
		return
	}
	for i, v := range views {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s\n", v.Name)
		fmt.Fprintf(w, "  %-14s %s %s\n", "device", v.Manufacturer, v.Model)
		fmt.Fprintf(w, "  %-14s %s\n", "udn", v.UDN)
		fmt.Fprintf(w, "  %-14s %s\n", "location", v.Location)
		fmt.Fprintf(w, "  %-14s %s\n", "avtransport", v.AVTransport)
		if !withProfile {
			continue
		}
		if v.Profile == nil {
			fmt.Fprintf(w, "  %-14s unavailable\n", "plays")
			continue
		}
		printProfileLine(w, "containers", v.Profile.Containers)
		printProfileLine(w, "video", v.Profile.VideoCodecs)
		printProfileLine(w, "audio", v.Profile.AudioCodecs)
	}
}

func printProfileLine(w io.Writer, label string, values []string) {
	if len(values) == 0 {
		return
	}
	fmt.Fprintf(w, "  %-14s %s\n", label, strings.Join(values, " "))
}
