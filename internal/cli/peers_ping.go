package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/peer/endpoint"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

// newPeersPingCommand dials another peer over mTLS and prints what that peer
// worked out about this one.
//
// It exists because a refusal in this fabric is a failed handshake, not a
// status code, and nothing an operator had before this could tell "the pin
// refused me" from "the port is closed" from "the other end is down". It is
// also what makes the end-to-end demo able to assert pinning at all: without a
// command that presents a peer identity, a shell script can only observe the
// membership table, never the transport that consults it.
//
// It pins the record it just read. The controller has already told this
// process which key that peer is; consulting a membership table the CLI does
// not have would be strictly weaker, and accepting whatever answers at the
// endpoint would be trust on first use with extra steps.
func newPeersPingCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "ping <name-or-id>",
		Short: "Open a mutually authenticated connection to a peer and report who it says you are",
		Long: `Dial another peer over mTLS and print the identity it derived from this node's
certificate (§26, ADR-0012).

Both ends pin. This node presents a self-signed certificate carrying its
Ed25519 public key, the other end refuses it unless a membership record there
pins that key, and this node refuses the other end unless the certificate it
presents carries the key the local membership record pins. Neither end has
anything else to go on: there is no CA and no PKI in the inter-peer path.

The identity printed is the OTHER end's conclusion about this node, not
anything this node asserted. If it names this peer, the pin held in both
directions.

A refusal is a failed handshake rather than an error status, because a refused
peer must never reach a request handler. So this command's failure output is
the point of it: "not a member" means the key is not enrolled at the other
site, and a connection error means the endpoint is wrong or nothing is
listening there.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				return pingPeer(ctx, cmd.OutOrStdout(), c, cfg, args[0], flags.asJSON)
			})
		},
	}
	flags.register(cmd)
	return cmd
}

func pingPeer(ctx context.Context, out io.Writer, c *client.Client, cfg config.Config, ref string, asJSON bool) error {
	dial, err := peerDialer(ctx, c, cfg, ref)
	if err != nil {
		return err
	}
	var got peerapi.IdentityResponse
	if err := dial.call(ctx, http.MethodGet, "/identity", nil, &got); err != nil {
		return err
	}

	if asJSON {
		return emitJSON(out, got)
	}
	t := newTable("PEER", "SEEN AS", "NAME", "PUBLIC KEY", "SERVED BY")
	t.add(dial.target.Name, got.PeerID, got.Name, got.PublicKey, got.ServedBy)
	return t.render(out, "no identity")
}

// peerConnection is a pinned mTLS client aimed at one enrolled peer.
//
// It is shared by `peers ping` and `peers attach` rather than copied, because
// the copy is where a second pinning decision would appear — and the second
// one is always the one that pins less.
type peerConnection struct {
	http   *http.Client
	base   string
	target client.Peer
}

// peerDialer builds the connection to one enrolled peer.
//
// It pins the record it just read. The controller has already told this
// process which key that peer is; consulting a membership table the CLI does
// not have would be strictly weaker, and accepting whatever answers at the
// endpoint would be trust on first use with extra steps.
func peerDialer(ctx context.Context, c *client.Client, cfg config.Config, ref string) (*peerConnection, error) {
	var target client.Peer
	if err := c.Get(ctx, "/peers/"+url.PathEscape(ref), nil, &target); err != nil {
		return nil, err
	}
	if target.PublicKey == nil || *target.PublicKey == "" {
		return nil, fmt.Errorf("peer %q has no public key recorded, so there is nothing to pin — "+
			"enrol it with `heyarr peers add --public-key` (ADR-0012)", ref)
	}
	if target.Endpoint == nil || *target.Endpoint == "" {
		return nil, fmt.Errorf("peer %q has no endpoint recorded, so there is nowhere to dial — "+
			"register it again with --endpoint; the endpoint is not its identity and may change freely", ref)
	}
	// The recorded endpoint is normalised on the way OUT as well as on the way
	// in (#169), because a row written before that check exists can hold a
	// value `peers add` would refuse today — and what an operator got for one
	// was `parse "192.168.x.x:8443/peer/v1/identity": first path segment in URL
	// cannot contain colon`, a net/url message about a path nobody typed.
	//
	// It happens HERE, in the shared dialler, rather than in either command:
	// `ping` and `attach` reach the same peer over the same pinned connection,
	// and an endpoint one of them could dial and the other could not would be
	// the same bug wearing a different command's name.
	dialTo, err := endpoint.Normalise(*target.Endpoint)
	if err == nil && !strings.HasPrefix(dialTo, endpoint.Scheme+"://") {
		// A unix:// endpoint is a legitimate peer endpoint and a legitimate
		// probe target (§31), and it is not something this connection can
		// present a certificate over.
		err = fmt.Errorf("%w: %q is not an %s origin, and the peer surface is mutually "+
			"authenticated TLS (ADR-0012)", endpoint.ErrMalformed, *target.Endpoint, endpoint.Scheme)
	}
	if err != nil {
		return nil, fmt.Errorf("peer %q is registered at an endpoint this node cannot dial: %w.\n"+
			"Re-register it with `heyarr peers add --name %s --public-key <key> --endpoint <address>`; "+
			"the endpoint is not its identity and may change freely", ref, err, target.Name)
	}
	return pinnedConnection(ctx, c, cfg, target, dialTo)
}

// pinnedConnection builds the mTLS client for one peer, given the peer record
// and the origin to dial.
//
// It is separate from peerDialer because `peers add` needs it for a peer that
// is not enrolled yet (#186): the enrolment-time reachability check dials the
// candidate with the key the operator just typed, and there is no row to read
// it out of. Everything below this line is identical either way, which is the
// point — a second copy of it is where a second, weaker pinning decision would
// appear.
func pinnedConnection(
	ctx context.Context, c *client.Client, cfg config.Config, target client.Peer, dialTo string,
) (*peerConnection, error) {
	targetKey, keyErr := identity.ParsePublicKey(*target.PublicKey)
	if keyErr != nil {
		return nil, keyErr
	}

	self, err := selfPeer(ctx, c)
	if err != nil {
		return nil, err
	}
	priv, err := identity.Signer(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	// The private key on disk must be the one the catalog records for this
	// node. Refusing here rather than at the handshake means the operator is
	// told their data directory is wrong instead of being told the other site
	// has not enrolled them.
	if self.PublicKey != nil {
		recorded, parseErr := identity.ParsePublicKey(*self.PublicKey)
		if parseErr != nil {
			return nil, parseErr
		}
		if !recorded.Equal(priv.Public()) {
			return nil, fmt.Errorf("%w: the key in %s does not match the public key this node records",
				identity.ErrKeyMismatch, cfg.DataDir)
		}
	}

	material, err := mtls.NewMaterial(mtls.MaterialOptions{PrivateKey: priv, PeerID: self.ID})
	if err != nil {
		return nil, err
	}
	httpClient, err := mtls.Client(mtls.Options{
		Material: material,
		Members: mtls.PinnedKey(mtls.Peer{
			PeerID: target.ID, Name: target.Name, PublicKey: targetKey,
		}),
	})
	if err != nil {
		return nil, err
	}
	return &peerConnection{
		http: httpClient,
		// The normalised origin, not the recorded string: everything this
		// connection dials is built from one value that was checked once.
		base:   dialTo + peerapi.Prefix,
		target: target,
	}, nil
}

// call makes one request on the peer surface and decodes the answer.
func (p *peerConnection) call(ctx context.Context, method, path string, body []byte, out any) error {
	// Named dialURL rather than endpoint: the package that decides what an
	// endpoint may be is imported here now, and a local shadowing it is how
	// the next reader ends up looking at the wrong thing.
	dialURL := p.base + path
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, dialURL, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("the connection to %s was refused: %w.\n"+
			"A refusal in this fabric is a failed handshake rather than an error status, so this is "+
			"either that peer refusing this node's key, this node refusing that peer's key, or "+
			"nothing listening at the endpoint", dialURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d: %s", dialURL, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s answered something this command cannot read: %w", dialURL, err)
	}
	return nil
}

// selfPeer is this node's own membership record.
func selfPeer(ctx context.Context, c *client.Client) (client.Peer, error) {
	peers, err := client.List[client.Peer](ctx, c, "/peers", client.ListOptions{})
	if err != nil {
		return client.Peer{}, err
	}
	for _, p := range peers {
		if p.IsSelf {
			return p, nil
		}
	}
	return client.Peer{}, errors.New("this instance has no self peer, so it cannot present an identity (ADR-0010)")
}
