package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/device/gateway"
)

// GatewayPasswordEnvVar is where the device password is read from when no file
// gives one. An environment variable, never an argument, for the reason a token
// is: a secret on the command line is in shell history and every user's `ps`.
const GatewayPasswordEnvVar = "HEYARR_GATEWAY_PASSWORD" // #nosec G101 -- the name of a variable, not a credential

// newDeviceGatewayCommand runs the device-side compatibility gateway (§70, §73,
// ADR-0051) on this machine.
//
// It is a Subsonic server the user points a STOCK app at as its one origin. It
// serves the personal-state methods (getPlaylists, getPlaylist) from state this
// device decrypts locally, and proxies the library and stream methods to the
// controller. Like `device mcp` it is a CLIENT concern: the decrypt happens here,
// on the device, and the controller only ever sees ciphertext (§72).
func newDeviceGatewayCommand(_ Options, dir *string) *cobra.Command {
	var (
		flags            clientFlags
		configPath       string
		listen           string
		controllerURL    string
		deviceUser       string
		devicePasswdFile string
		starredSpace     string
		historySpace     string
	)
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Run the device-side Subsonic compatibility gateway (§73, ADR-0051)",
		Long: `Serve a Subsonic API a STOCK music app points at as its one origin.

Two families of method, served from where each honestly lives:

  - getPlaylists / getPlaylist, and — with --starred-space / --history-space —
    getStarred2, getNowPlaying and getAlbumList2?type=recent|frequent|starred are
    your ENCRYPTED personal state. The gateway fetches the ciphertext from the
    controller, unwraps the space key with THIS device's key, and materialises the
    matching CRDT locally — the controller sees only ciphertext and can read none
    of it (§72).
  - ping, getArtists, getArtist, the catalogue getAlbumList2 types, getAlbum,
    stream and download are proxied to the controller's Subsonic adapter, which
    serves the server-readable library. The gateway substitutes its own controller
    bearer, so the app never holds it.

The app authenticates to the DEVICE with a Subsonic username and password (set
--device-user and the password via --device-password-file or ` + GatewayPasswordEnvVar + `).
The device authenticates to the controller with its own bearer token. The two
credentials are distinct by design.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			token, err := flags.resolveToken(cfg)
			if err != nil {
				return err
			}
			if token == "" {
				return errors.New("the gateway needs a controller bearer token to proxy the library — " +
					"set " + TokenEnvVar + ", --token-file, or a cli.token in the data directory")
			}
			base, err := resolveControllerURL(controllerURL, cfg)
			if err != nil {
				return err
			}
			passwd, err := resolveDevicePassword(devicePasswdFile)
			if err != nil {
				return err
			}
			apiClient, err := flags.newClient(configPath)
			if err != nil {
				return err
			}

			srv, err := gateway.New(gateway.Options{
				Personal: gateway.NewSpaceLibrary(apiClient, *dir).
					WithRoles(gateway.SpaceRoles{StarredSpace: starredSpace, HistorySpace: historySpace}),
				Controller: gateway.Controller{
					BaseURL: base,
					User:    deviceUser,
					Bearer:  token,
				},
				DeviceUser:     deviceUser,
				DevicePassword: passwd,
				ServerVersion:  buildinfo.Get().Version,
			})
			if err != nil {
				return err
			}
			return serveGateway(cmd.Context(), cmd.ErrOrStderr(), listen, srv.Handler())
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&configPath, "config", "", "controller config, to fetch your encrypted personal state (§73)")
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:4040", "address to serve the gateway on")
	cmd.Flags().StringVar(&controllerURL, "controller-url", "",
		"the controller's Subsonic origin for proxied library/stream methods (default: http.addr from the config)")
	cmd.Flags().StringVar(&deviceUser, "device-user", "heyarr", "the username the stock app authenticates to this device with")
	cmd.Flags().StringVar(&devicePasswdFile, "device-password-file", "",
		"read the device password from this file (or set "+GatewayPasswordEnvVar+")")
	cmd.Flags().StringVar(&starredSpace, "starred-space", "",
		"space id holding your starred set, to serve getStarred2 and getAlbumList2?type=starred (§46)")
	cmd.Flags().StringVar(&historySpace, "history-space", "",
		"space id holding your play history, to serve getNowPlaying and getAlbumList2?type=recent|frequent (§46)")
	return cmd
}

// resolveControllerURL settles the origin the proxy forwards library/stream to.
// An explicit flag wins; otherwise the config's http.addr, as an http:// origin.
// A unix-socket-only controller has no origin a stock app's stream can reach, so
// that is an error the operator must resolve by naming a reachable URL.
func resolveControllerURL(flag string, cfg config.Config) (string, error) {
	if s := strings.TrimSpace(flag); s != "" {
		return s, nil
	}
	if addr := strings.TrimSpace(cfg.HTTP.Addr); addr != "" {
		if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
			return addr, nil
		}
		return "http://" + addr, nil
	}
	return "", errors.New("no controller URL for proxied library/stream methods — " +
		"pass --controller-url http://host:port (a unix socket cannot serve a stock app's stream)")
}

// resolveDevicePassword reads the device password from the file or the
// environment. It is required: an empty one would accept every caller.
func resolveDevicePassword(file string) (string, error) {
	if file != "" {
		raw, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			return "", fmt.Errorf("reading the device password file %s: %w", file, err)
		}
		return strings.TrimSpace(string(raw)), nil
	}
	if v := os.Getenv(GatewayPasswordEnvVar); v != "" {
		return v, nil
	}
	return "", errors.New("no device password — set --device-password-file or " + GatewayPasswordEnvVar +
		"; the stock app authenticates to this device with it")
}

// serveGateway runs the HTTP server until the context is cancelled, then shuts it
// down. Readiness goes to stderr, so nothing but the gateway's own protocol
// responses is ever written to a caller.
func serveGateway(ctx context.Context, stderr io.Writer, listen string, h http.Handler) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("gateway: listening on %s: %w", listen, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	fmt.Fprintf(stderr, "heyarr device gateway: serving Subsonic on http://%s/rest\n", ln.Addr())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
