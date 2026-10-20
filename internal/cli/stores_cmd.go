package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
)

// newStoresCommand is `heyarr stores`: the games an operator owns on a store,
// turned into wants (ADR-0120).
func newStoresCommand(_ Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stores",
		Short: "Back up the games you own on a store",
	}
	cmd.AddCommand(newStoresSyncCommand(configPath))
	return cmd
}

// ownedGame is the part of `lgogdownloader --list json` a sync reads: the
// store's name for the game and its title.
type ownedGame struct {
	GameName string `json:"gamename"`
	Title    string `json:"title"`
}

// storeSyncResult is what a sync did, for a person or --json.
type storeSyncResult struct {
	Store         string   `json:"store"`
	Owned         int      `json:"owned"`
	Wanted        []string `json:"wanted"`
	AlreadyWanted []string `json:"already_wanted"`
	Skipped       []string `json:"skipped"`
	// Failed names each game the node refused, with why — a title collision
	// with a different product, say. The rest of the library is still synced.
	Failed []string `json:"failed"`
}

// listOwnedGOG runs lgogdownloader as whoever runs this command — the service
// user whose lgogdownloader login the GOG download client uses.
func listOwnedGOG(ctx context.Context) ([]byte, error) {
	if _, err := exec.LookPath("lgogdownloader"); err != nil {
		return nil, errors.New("lgogdownloader is not on PATH — install it, or pass --from-file")
	}
	// Hidden products are still owned: a game hidden on the account page is
	// backed up like any other.
	out, err := exec.CommandContext(ctx, "lgogdownloader", "--list", "json", "--no-color",
		"--include-hidden-products").Output()
	if err != nil {
		return nil, errors.New("lgogdownloader could not list the library (log in as this user with `lgogdownloader --login`)")
	}
	return out, nil
}

func newStoresSyncCommand(configPath *string) *cobra.Command {
	var (
		flags    clientFlags
		fromFile string
		profile  string
		dryRun   bool
	)
	cmd := &cobra.Command{
		Use:   "sync gog",
		Short: "Want every game you own on GOG",
		Long: `List the games the logged-in GOG account owns and want each one, so the
store fetch backs it up: downloaded by lgogdownloader, verified file by file
against GOG's listing, and ingested as a game (ADR-0120).

Run it as the service user whose lgogdownloader login the GOG download
client uses:

  sudo -u heyarr lgogdownloader --login        # once
  sudo -u heyarr heyarr stores sync gog

Re-running is safe: a game already wanted is reported and left alone. Listing
a large library is slow (lgogdownloader asks GOG for every game's details); pass
--from-file with a saved 'lgogdownloader --list json' to sync from that instead.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "gog" {
				return fmt.Errorf("unknown store %q — only gog is supported", args[0])
			}
			var raw []byte
			var err error
			if fromFile != "" {
				raw, err = os.ReadFile(fromFile) //nolint:gosec // G304: an operator-named file is the point
			} else {
				raw, err = listOwnedGOG(cmd.Context())
			}
			if err != nil {
				return err
			}
			var games []ownedGame
			if err := json.Unmarshal(raw, &games); err != nil {
				return fmt.Errorf("reading the GOG library listing: %w", err)
			}
			res := storeSyncResult{Store: "gog", Owned: len(games)}
			want := func(ctx context.Context, c *client.Client) error {
				// The profile is the same for every game: check it once, rather
				// than discover it is missing as one refusal per game.
				if c != nil {
					name, err := profileExists(ctx, c, profile)
					if err != nil {
						return err
					}
					// Sent by name: the API resolves quality_profile as a name, so
					// an id given on the command line is normalised here.
					profile = name
				}
				for _, g := range games {
					name := strings.TrimSpace(g.GameName)
					title := strings.TrimSpace(g.Title)
					if !acquisition.ValidGOGGameName(name) || title == "" {
						res.Skipped = append(res.Skipped, name)
						continue
					}
					if dryRun {
						res.Wanted = append(res.Wanted, name)
						continue
					}
					req := client.CreateDesiredRequest{
						Work: &client.WorkDescriptor{
							ContentType: "game", Title: title,
							StoreRef: acquisition.GOGSource(name),
						},
						QualityProfile: profile,
						Reason:         "owned on GOG",
					}
					var item client.DesiredItem
					err := c.Post(ctx, "/desired", req, &item)
					var apiErr *client.Error
					switch {
					case err == nil:
						res.Wanted = append(res.Wanted, name)
					case errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict:
						res.AlreadyWanted = append(res.AlreadyWanted, name)
					case errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest:
						// A refusal of this one game (a title collision, a ref
						// another work holds): recorded, and the sync goes on.
						res.Failed = append(res.Failed, name+": "+err.Error())
					default:
						// Unreachable, unauthorised, a server error: every game
						// would fail the same way, so stop rather than repeat it.
						return fmt.Errorf("wanting %s: %w", name, err)
					}
				}
				return nil
			}
			if dryRun {
				if err := want(cmd.Context(), nil); err != nil {
					return err
				}
			} else if err := flags.withClient(cmd, configPath, want); err != nil {
				return err
			}
			if flags.asJSON {
				if err := emitJSON(cmd.OutOrStdout(), res); err != nil {
					return err
				}
				if len(res.Failed) > 0 {
					return fmt.Errorf("%d game(s) were refused", len(res.Failed))
				}
				return nil
			}
			verb := "wanted"
			if dryRun {
				verb = "would want"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "gog: %d owned, %s %d, %d already wanted, %d skipped, %d refused\n",
				res.Owned, verb, len(res.Wanted), len(res.AlreadyWanted), len(res.Skipped), len(res.Failed))
			for _, f := range res.Failed {
				fmt.Fprintf(cmd.ErrOrStderr(), "  refused %s\n", f)
			}
			if len(res.Failed) > 0 {
				return fmt.Errorf("%d game(s) were refused", len(res.Failed))
			}
			return nil
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&fromFile, "from-file", "",
		"read a saved 'lgogdownloader --list json' instead of running it")
	cmd.Flags().StringVar(&profile, "quality-profile", "",
		"the profile each game is wanted under (defaults to the game default, owned)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"list the valid games the listing would send, without asking the node (so it cannot "+
			"tell what is already wanted)")
	return cmd
}

// profileExists finds a sync's profile, by name or id, before any game is sent,
// and returns its name. An empty name is the game default, `owned`.
func profileExists(ctx context.Context, c *client.Client, name string) (string, error) {
	if name == "" {
		name = "owned"
	}
	profiles, err := client.List[client.QualityProfile](ctx, c, "/quality-profiles", client.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("reading quality profiles: %w", err)
	}
	for _, p := range profiles {
		if p.Name == name || p.ID == name {
			return p.Name, nil
		}
	}
	return "", fmt.Errorf("there is no quality profile called %q; nothing was wanted", name)
}
