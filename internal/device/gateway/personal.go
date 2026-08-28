package gateway

import (
	"context"
	"crypto/ecdh"
	"fmt"

	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/device"
	psclient "github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
)

// SpaceLibrary is the production [Library]: it reads the device's playlists by
// decrypting them locally (§73, ADR-0051, ADR-0049). For each space it fetches
// the opaque ciphertext from the controller over /api/v1, finds the wrapped key
// sealed for THIS device, unwraps it with the device's X25519 key, and
// materialises the playlist CRDT — all on the device. The controller only ever
// serves ciphertext; nothing here hands it a key or a plaintext.
//
// It is the same decrypt path the CLI's `space read` and the Personal MCP's
// personal_playlist tool take, reached from the gateway instead of a command.
type SpaceLibrary struct {
	client    *apiclient.Client
	deviceDir string
}

// NewSpaceLibrary builds the production library over an already-constructed API
// client (which holds the controller bearer) and the device key directory.
func NewSpaceLibrary(c *apiclient.Client, deviceDir string) *SpaceLibrary {
	return &SpaceLibrary{client: c, deviceDir: deviceDir}
}

var _ Library = (*SpaceLibrary)(nil)

// Playlists lists every space this device can decrypt, each as a playlist. A
// space the device holds no key for is silently omitted rather than errored:
// getPlaylists should show the playlists the user can actually see, not fail
// because some space on the controller was wrapped for another device.
func (l *SpaceLibrary) Playlists(ctx context.Context) ([]Playlist, error) {
	spaces, err := l.client.ListSpaces(ctx)
	if err != nil {
		return nil, err
	}
	priv, err := l.loadEncKey()
	if err != nil {
		return nil, err
	}
	out := make([]Playlist, 0, len(spaces))
	for _, sp := range spaces {
		items, ok, err := l.materialise(ctx, priv, sp.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, Playlist{ID: sp.ID, Name: playlistName(sp.ID), Items: items})
	}
	return out, nil
}

// Playlist returns one space's playlist, or ok=false when this device cannot
// decrypt it (no wrapped key for this device, or no such space).
func (l *SpaceLibrary) Playlist(ctx context.Context, id string) (Playlist, bool, error) {
	priv, err := l.loadEncKey()
	if err != nil {
		return Playlist{}, false, err
	}
	items, ok, err := l.materialise(ctx, priv, id)
	if err != nil {
		return Playlist{}, false, err
	}
	if !ok {
		return Playlist{}, false, nil
	}
	return Playlist{ID: id, Name: playlistName(id), Items: items}, true, nil
}

// materialise opens the space by unwrapping its key for this device, decrypts the
// changes the controller holds, and folds them into the playlist. ok is false
// (with no error) when this device holds no wrapped copy of the space's key — the
// confidentiality gate of ADR-0049, reached here before any change is decrypted.
func (l *SpaceLibrary) materialise(ctx context.Context, priv *ecdh.PrivateKey, spaceID string) (items []string, ok bool, err error) {
	mine := encryption.FormatPublicKey(priv.PublicKey().Bytes())
	keys, err := l.client.WrappedKeys(ctx, spaceID)
	if err != nil {
		if apiclient.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var wrapped []byte
	for _, k := range keys {
		if k.Recipient == mine {
			wrapped = k.Wrapped
			break
		}
	}
	if wrapped == nil {
		return nil, false, nil
	}
	mgr := psclient.New()
	if err := mgr.Open(spaceID, wrapped, psclient.NewKeyUnwrapper(priv)); err != nil {
		return nil, false, err
	}

	st := crdt.New()
	if snap, has, err := l.client.Snapshot(ctx, spaceID); err != nil {
		return nil, false, err
	} else if has {
		base, err := statesync.DecodeSnapshot(mgr, snap)
		if err != nil {
			return nil, false, err
		}
		st = base
	}
	changes, err := l.client.Changes(ctx, spaceID)
	if err != nil {
		return nil, false, err
	}
	decoded, err := statesync.DecodeAll(mgr, changes)
	if err != nil {
		return nil, false, err
	}
	st.Apply(decoded...)
	ids := st.IDs()
	if ids == nil {
		ids = []string{}
	}
	return ids, true, nil
}

func (l *SpaceLibrary) loadEncKey() (*ecdh.PrivateKey, error) {
	dir := l.deviceDir
	if dir == "" {
		resolved, err := device.DefaultDir()
		if err != nil {
			return nil, err
		}
		dir = resolved
	}
	ds, err := device.NewStore(device.StoreOptions{Dir: dir})
	if err != nil {
		return nil, err
	}
	priv, err := ds.LoadEncryptionKey()
	if err != nil {
		return nil, fmt.Errorf("gateway: loading this device's encryption key: %w", err)
	}
	return priv, nil
}

// playlistName is the display name for a space's playlist. A playlist's own name
// is itself encrypted personal state with no CRDT type yet (a space carries no
// plaintext name — that is deliberate, §39), so the honest name today is the
// space id. Naming rides on the same follow-up that adds the history/starred
// CRDT types — see doc.go.
func playlistName(spaceID string) string { return "Playlist " + spaceID }
