package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/client"
)

func newEventsCommand(opts Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Follow the event log",
		Long: `Every state transition Heyarr makes emits an event (§76, ADR-0009).

The stream is the integration model: an external tool watches it instead of
polling, and reconnection is gapless — a client that saw sequence N reconnects
with --after N and receives everything since, with no hole and no duplicate.`,
	}
	cmd.AddCommand(newEventsTailCommand(opts, configPath))
	return cmd
}

// gapLine is how a dropped-events notice appears in --json output. It carries
// the same `type` field an event does, so a consumer reading JSON Lines can
// branch on one key rather than on the presence of another.
type gapLine struct {
	Type        string `json:"type"`
	ResumeAfter int64  `json:"resume_after"`
	Dropped     int64  `json:"dropped"`
	Detail      string `json:"detail"`
}

func newEventsTailCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags     clientFlags
		after     int64
		types     []string
		limit     int
		reconnect bool
	)
	cmd := &cobra.Command{
		Use:   "tail",
		Short: "Print events as they happen",
		Long: `Follow the event stream, printing each event as it arrives.

--after resumes from a sequence number: pass the last one you saw and nothing
is missed or repeated. --types filters server-side and accepts exact type names
and trailing-* namespace prefixes, e.g. --types 'content.*,job.succeeded'.

If the server reports that it dropped events for this connection — which it
does rather than quietly continuing with a hole — the notice is printed and,
unless --reconnect=false, the stream is reopened from the resume point it gave.
A dropped-events notice is never swallowed: losing events is recoverable, not
knowing you lost them is not.

--json emits one compact JSON object per line rather than an array, because a
stream has no end at which to close one.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				return tailEvents(ctx, cmd, c, flags.asJSON, after, types, limit, reconnect)
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().Int64Var(&after, "after", 0, "resume after this sequence number")
	cmd.Flags().StringSliceVar(&types, "types", nil, "only these event types; accepts trailing-* prefixes")
	cmd.Flags().IntVarP(&limit, "limit", "n", 0, "stop after this many events (default: follow forever)")
	cmd.Flags().BoolVar(&reconnect, "reconnect", true, "reopen the stream after a gap notice or a dropped connection")
	return cmd
}

// tailEvents follows the stream, reconnecting from the last sequence seen.
func tailEvents(ctx context.Context, cmd *cobra.Command, c *client.Client,
	asJSON bool, after int64, types []string, limit int, reconnect bool,
) error {
	out := cmd.OutOrStdout()
	// Compact, one object per line. Not the indented encoder the other
	// commands use: this output is a stream, and an indented object per event
	// is unreadable in a terminal and unparseable line by line.
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)

	printed := 0
	for {
		stream, err := c.Events(ctx, after, types)
		if err != nil {
			return err
		}
		// pump only returns when something stops it, so its error is never
		// nil: the cases below are a classification of how the stream ended
		// rather than a success check.
		err = pump(stream, func(msg client.StreamMessage) error {
			switch {
			case msg.Gap != nil:
				if asJSON {
					return enc.Encode(gapLine{
						Type:        client.StreamGapType,
						ResumeAfter: msg.Gap.ResumeAfter,
						Dropped:     msg.Gap.Dropped,
						Detail:      msg.Gap.Detail,
					})
				}
				_, err := fmt.Fprintf(cmd.ErrOrStderr(),
					"the stream dropped %d event(s): %s — resuming after %d\n",
					msg.Gap.Dropped, msg.Gap.Detail, msg.Gap.ResumeAfter)
				return err
			case msg.Event != nil:
				printed++
				if asJSON {
					if err := enc.Encode(msg.Event); err != nil {
						return err
					}
				} else if err := printEvent(out, msg.Event); err != nil {
					return err
				}
				if limit > 0 && printed >= limit {
					return errEnough
				}
			}
			return nil
		})
		after = stream.LastSeq()
		_ = stream.Close()

		switch {
		case errors.Is(err, errEnough):
			return nil
		case ctx.Err() != nil:
			// A cancelled context is how `heyarr events tail` normally ends:
			// somebody pressed Ctrl-C. That is not a failure.
			return nil
		case !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF):
			return err
		case !reconnect:
			return nil
		}
	}
}

// errEnough stops the pump once --limit events have been printed. A sentinel
// rather than a bool return so that the callback signature stays "report a
// problem or nothing".
var errEnough = errors.New("enough")

// pump delivers frames until the stream ends or fn stops it. It always returns
// a non-nil error, because there is no other way for it to return: a stream
// that is still open is still being read.
func pump(stream *client.EventStream, fn func(client.StreamMessage) error) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		if err := fn(msg); err != nil {
			return err
		}
	}
}

func printEvent(w io.Writer, e *client.Event) error {
	subject := e.SubjectType
	if e.SubjectID != "" {
		subject = strings.TrimSpace(subject + " " + e.SubjectID)
	}
	if subject == "" {
		subject = "-"
	}
	_, err := fmt.Fprintf(w, "%d\t%s\t%s\t%s\n",
		e.Seq, e.CreatedAt.UTC().Format(time.RFC3339), e.Type, subject)
	return err
}
