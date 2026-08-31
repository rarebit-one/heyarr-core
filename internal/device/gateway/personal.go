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
	roles     SpaceRoles
}

// SpaceRoles names which space holds which non-playlist personal-state CRDT. A
// space holds one CRDT and which one is decided by the caller, never carried on
// the wire (see internal/personalstate/statesync) — so the device is told, at
// construction, the id of the space that holds its starred set and the id of the
// space that holds its play history. An empty id means "no such space yet", and
// the matching read returns empty rather than failing.
type SpaceRoles struct {
	// StarredSpace holds the starred OR-Set (feeds getStarred2, getAlbumList2?type=starred).
	StarredSpace string
	// HistorySpace holds the play-history G-Set (feeds getNowPlaying, getAlbumList2?type=recent|frequent).
	HistorySpace string
}

// NewSpaceLibrary builds the production library over an already-constructed API
// client (which holds the controller bearer) and the device key directory. It
// serves playlists out of the box; call [SpaceLibrary.WithRoles] to also serve
// the starred and history surfaces from their spaces.
func NewSpaceLibrary(c *apiclient.Client, deviceDir string) *SpaceLibrary {
	return &SpaceLibrary{client: c, deviceDir: deviceDir}
}

// WithRoles records which spaces hold the starred set and the play history, and
// returns the library for chaining. Without it, Starred/Recent/Frequent/NowPlaying
// return empty — the honest answer for a device with no such space configured.
func (l *SpaceLibrary) WithRoles(r SpaceRoles) *SpaceLibrary {
	l.roles = r
	return l
}

var _ Library = (*SpaceLibrary)(nil)

// Playlists lists every space this device can decrypt AS A PLAYLIST, each as a
// playlist. A space the device holds no key for is silently omitted rather than
// errored: getPlaylists should show the playlists the user can actually see, not
// fail because some space on the controller was wrapped for another device.
//
// The spaces the device knows hold a NON-playlist CRDT — its configured starred
// and history spaces — are skipped: a space holds one CRDT, and materialising a
// starred set's changes as a playlist would fabricate a bogus playlist. Which
// space holds which type is the device's own knowledge (SpaceRoles), never a tag
// the controller could read.
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
		if l.isNonPlaylistSpace(sp.ID) {
			continue
		}
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

// isNonPlaylistSpace reports whether a space is one this device knows holds a
// non-playlist CRDT (its starred or history space), so getPlaylists skips it.
func (l *SpaceLibrary) isNonPlaylistSpace(spaceID string) bool {
	return spaceID != "" && (spaceID == l.roles.StarredSpace || spaceID == l.roles.HistorySpace)
}

// Playlist returns one space's playlist, or ok=false when this device cannot
// decrypt it (no wrapped key for this device, or no such space) — or when the id
// is a space this device knows holds a non-playlist CRDT, which is "not a
// playlist" rather than an error.
func (l *SpaceLibrary) Playlist(ctx context.Context, id string) (Playlist, bool, error) {
	if l.isNonPlaylistSpace(id) {
		return Playlist{}, false, nil
	}
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
	mgr, ok, err := l.openWrapped(ctx, priv, spaceID)
	if err != nil || !ok {
		return nil, ok, err
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

// openWrapped finds the copy of the space key sealed for THIS device and opens
// the space with it. ok is false (no error) when the controller holds no space of
// that id, or no copy wrapped for this device — the ADR-0049 confidentiality gate,
// reached before any change is decrypted. The controller is never handed a key.
func (l *SpaceLibrary) openWrapped(ctx context.Context, priv *ecdh.PrivateKey, spaceID string) (*psclient.Manager, bool, error) {
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
	return mgr, true, nil
}

// Starred returns this device's starred item ids, most-recently-starred first,
// decrypted on the device from the configured starred space (§46, §72). No
// configured or decryptable space is an empty answer, not an error.
func (l *SpaceLibrary) Starred(ctx context.Context) ([]string, error) {
	changes, err := decodeChanges[crdt.StarChange](ctx, l, l.roles.StarredSpace)
	if err != nil {
		return nil, err
	}
	s := crdt.NewStarSet()
	s.Apply(changes...)
	ids := s.StarredIDs()
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// Recent returns distinct played item ids most-recently-played first, decrypted
// from the configured history space (§46, §72).
func (l *SpaceLibrary) Recent(ctx context.Context) ([]string, error) {
	log, err := l.playLog(ctx)
	if err != nil {
		return nil, err
	}
	return idsOf(log.Recent()), nil
}

// Frequent returns distinct played item ids most-played first, decrypted from the
// configured history space (§46, §72).
func (l *SpaceLibrary) Frequent(ctx context.Context) ([]string, error) {
	log, err := l.playLog(ctx)
	if err != nil {
		return nil, err
	}
	return idsOf(log.Frequent()), nil
}

// NowPlaying returns the item id of the single most recent play, decrypted from
// the configured history space (§46, §72), or ok=false when nothing has played.
func (l *SpaceLibrary) NowPlaying(ctx context.Context) (string, bool, error) {
	log, err := l.playLog(ctx)
	if err != nil {
		return "", false, err
	}
	id, ok := log.NowPlaying()
	return id, ok, nil
}

// playLog materialises the configured history space's play log once, for the
// three history reads to derive their views from.
func (l *SpaceLibrary) playLog(ctx context.Context) (*crdt.PlayLog, error) {
	changes, err := decodeChanges[crdt.PlayChange](ctx, l, l.roles.HistorySpace)
	if err != nil {
		return nil, err
	}
	log := crdt.NewPlayLog()
	log.Apply(changes...)
	return log, nil
}

// idsOf projects a slice of play entries to their item ids, in the entry order.
func idsOf(entries []crdt.PlayEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	return ids
}

// decodeChanges is the shared device-side decrypt path for a typed non-playlist
// CRDT read: open the space by unwrapping its key with this device's key, fetch
// the opaque changes the controller holds, and decrypt+decode them into CRDT
// changes of type T. An empty space id, an unconfigured space, or one this device
// cannot decrypt all yield no changes and no error — the honest empty answer. The
// controller only ever serves ciphertext.
func decodeChanges[T any](ctx context.Context, l *SpaceLibrary, spaceID string) ([]T, error) {
	if spaceID == "" {
		return nil, nil
	}
	priv, err := l.loadEncKey()
	if err != nil {
		return nil, err
	}
	mgr, ok, err := l.openWrapped(ctx, priv, spaceID)
	if err != nil || !ok {
		return nil, err
	}
	changes, err := l.client.Changes(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	return statesync.DecodeAllChanges[T](mgr, changes)
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
