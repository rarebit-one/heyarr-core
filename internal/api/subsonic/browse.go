package subsonic

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Everything here is a read-only projection of the server-readable catalogue
// (works/editions/assets, §11) onto Subsonic's artist/album/song vocabulary
// (§70). The canonical model is never reshaped to suit the protocol: an artist
// is not an entity in Heyarr, so getArtists derives it by grouping music Works
// on works.attributes.artist rather than inventing an artist table.
//
// A music Work IS an album; a music Edition IS a track; the Asset carries the
// bytes. A track is listed only when it has a blob to stream (a managed or
// vault asset, blob_hash present) — a linked asset has no blob (ADR-0020) and
// nothing to serve, so listing it would advertise a song no client could play.

// musicFolders projects the enabled music libraries.
//
// The folder id is a row-ordinal integer, not the library's own id: Subsonic
// musicFolderId is historically an integer, and the adapter does not yet honour
// folder filtering, so the id needs to be stable within one response but never
// has to decode back to anything.
func (h *Handler) musicFolders(ctx context.Context) ([]musicFolder, error) {
	rows, err := h.reader.QueryContext(ctx,
		`SELECT name FROM libraries WHERE content_type = 'music' AND enabled = 1 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []musicFolder{}
	for i := 1; rows.Next(); i++ {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, musicFolder{ID: i, Name: name})
	}
	return out, rows.Err()
}

func (h *Handler) handleGetArtists(w http.ResponseWriter, r *http.Request) {
	h.authed(w, r, func(w http.ResponseWriter, r *http.Request, p params) {
		rows, err := h.reader.QueryContext(r.Context(), `
			SELECT json_extract(attributes, '$.artist') AS artist, COUNT(*) AS album_count
			FROM works
			WHERE content_type = 'music'
			  AND json_extract(attributes, '$.artist') IS NOT NULL
			  AND trim(json_extract(attributes, '$.artist')) <> ''
			GROUP BY artist`)
		if err != nil {
			h.internalError(w, r, p, "getArtists", err)
			return
		}
		defer func() { _ = rows.Close() }()

		type entry struct {
			name  string
			sort  string
			count int
		}
		var artists []entry
		for rows.Next() {
			var name string
			var count int
			if err := rows.Scan(&name, &count); err != nil {
				h.internalError(w, r, p, "getArtists", err)
				return
			}
			artists = append(artists, entry{name: name, sort: strings.ToLower(stripArticle(name)), count: count})
		}
		if err := rows.Err(); err != nil {
			h.internalError(w, r, p, "getArtists", err)
			return
		}
		sort.Slice(artists, func(i, j int) bool { return artists[i].sort < artists[j].sort })

		// Group into the letter buckets a client renders as a sidebar, keeping
		// bucket order stable by first appearance in the already-sorted list.
		var order []string
		buckets := map[string][]artistID3Item{}
		for _, a := range artists {
			key := indexKey(stripArticle(a.name))
			if _, seen := buckets[key]; !seen {
				order = append(order, key)
			}
			buckets[key] = append(buckets[key], artistID3Item{
				ID: artistID(a.name), Name: a.name, AlbumCount: a.count,
			})
		}
		idx := make([]artistIndex, 0, len(order))
		for _, key := range order {
			idx = append(idx, artistIndex{Name: key, Artist: buckets[key]})
		}

		resp := h.ok()
		resp.Artists = &artistsID3{IgnoredArticles: ignoredArticles, Index: idx}
		h.write(w, p.format, resp)
	})
}

func (h *Handler) handleGetArtist(w http.ResponseWriter, r *http.Request) {
	h.authed(w, r, func(w http.ResponseWriter, r *http.Request, p params) {
		name, ok := decodeArtistID(r.URL.Query().Get("id"))
		if !ok {
			h.write(w, p.format, h.fail(errNotFound, "artist not found"))
			return
		}
		albums, err := h.albumsWhere(r.Context(),
			`json_extract(w.attributes, '$.artist') = ?`, []any{name},
			`w.year, w.sort_title`, -1, 0)
		if err != nil {
			h.internalError(w, r, p, "getArtist", err)
			return
		}
		if len(albums) == 0 {
			h.write(w, p.format, h.fail(errNotFound, "artist not found"))
			return
		}
		resp := h.ok()
		resp.Artist = &artistID3{
			ID: artistID(name), Name: name, AlbumCount: len(albums), Album: albums,
		}
		h.write(w, p.format, resp)
	})
}

// albumOrder maps a Subsonic getAlbumList2 `type` to an ORDER BY, or reports
// that the type depends on personal state the server cannot read.
//
// newest/alphabetical*/random/byYear are pure catalogue reads. recent, frequent
// and starred are play-history and starred state — personal state, encrypted
// and opaque to the controller (§72). The adapter returns an empty list for
// those rather than faking one: a device-side Personal MCP (§73) is where that
// history lives, and pretending the server has it would be the exact invariant
// violation this milestone was scoped to avoid.
func albumOrder(listType string) (orderBy string, personal bool) {
	switch listType {
	case "", "alphabeticalByName":
		return "w.sort_title, w.id", false
	case "alphabeticalByArtist":
		return "json_extract(w.attributes, '$.artist') COLLATE NOCASE, w.sort_title, w.id", false
	case "newest":
		return "w.created_at DESC, w.id", false
	case "byYear":
		return "w.year, w.sort_title, w.id", false
	case "random":
		return "RANDOM()", false
	case "recent", "frequent", "starred":
		return "", true
	default:
		return "w.sort_title, w.id", false
	}
}

func (h *Handler) handleGetAlbumList2(w http.ResponseWriter, r *http.Request) {
	h.authed(w, r, func(w http.ResponseWriter, r *http.Request, p params) {
		q := r.URL.Query()
		orderBy, personal := albumOrder(q.Get("type"))
		if personal {
			resp := h.ok()
			resp.AlbumList2 = &albumList2{Album: []albumID3{}}
			h.write(w, p.format, resp)
			return
		}
		size := clampInt(q.Get("size"), 10, 1, 500)
		offset := clampInt(q.Get("offset"), 0, 0, 1<<30)

		albums, err := h.albumsWhere(r.Context(), `w.content_type = 'music'`, nil, orderBy, size, offset)
		if err != nil {
			h.internalError(w, r, p, "getAlbumList2", err)
			return
		}
		resp := h.ok()
		resp.AlbumList2 = &albumList2{Album: albums}
		h.write(w, p.format, resp)
	})
}

func (h *Handler) handleGetAlbum(w http.ResponseWriter, r *http.Request) {
	h.authed(w, r, func(w http.ResponseWriter, r *http.Request, p params) {
		workID, ok := decodeAlbumID(r.URL.Query().Get("id"))
		if !ok {
			h.write(w, p.format, h.fail(errNotFound, "album not found"))
			return
		}

		var title, artist string
		var year sql.NullInt64
		err := h.reader.QueryRowContext(r.Context(), `
			SELECT title, year, COALESCE(json_extract(attributes, '$.artist'), '')
			FROM works WHERE id = ? AND content_type = 'music'`, workID).
			Scan(&title, &year, &artist)
		if errors.Is(err, sql.ErrNoRows) {
			h.write(w, p.format, h.fail(errNotFound, "album not found"))
			return
		}
		if err != nil {
			h.internalError(w, r, p, "getAlbum", err)
			return
		}

		songs, err := h.songs(r.Context(), workID, title, artist, int(year.Int64))
		if err != nil {
			h.internalError(w, r, p, "getAlbum", err)
			return
		}

		album := albumID3{
			ID:        albumID(workID),
			Name:      title,
			Artist:    artist,
			SongCount: len(songs),
			Year:      int(year.Int64),
			Song:      songs,
		}
		if artist != "" {
			album.ArtistID = artistID(artist)
		}
		for _, s := range songs {
			album.Duration += s.Duration
		}
		resp := h.ok()
		resp.Album = &album
		h.write(w, p.format, resp)
	})
}

// albumsWhere is the shared album listing behind getAlbumList2 and getArtist.
// A negative limit means unbounded (getArtist wants every album an artist has).
func (h *Handler) albumsWhere(ctx context.Context, where string, args []any, orderBy string, limit, offset int) ([]albumID3, error) {
	//nolint:gosec // where/orderBy are fixed literals chosen by the caller from a closed set; every value is bound
	stmt := `
		SELECT w.id, w.title, w.year, COALESCE(json_extract(w.attributes, '$.artist'), ''),
		       (SELECT COUNT(*) FROM editions e
		          JOIN assets a ON a.edition_id = e.id AND a.blob_hash IS NOT NULL
		         WHERE e.work_id = w.id) AS song_count
		FROM works w
		WHERE ` + where + `
		ORDER BY ` + orderBy
	if limit >= 0 {
		stmt += " LIMIT " + strconv.Itoa(limit) + " OFFSET " + strconv.Itoa(offset)
	}

	rows, err := h.reader.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []albumID3{}
	for rows.Next() {
		var id, title, artist string
		var year sql.NullInt64
		var songCount int
		if err := rows.Scan(&id, &title, &year, &artist, &songCount); err != nil {
			return nil, err
		}
		a := albumID3{
			ID: albumID(id), Name: title, Artist: artist,
			SongCount: songCount, Year: int(year.Int64),
		}
		if artist != "" {
			a.ArtistID = artistID(artist)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// songs projects the streamable tracks of one album.
//
// One asset per edition is chosen deterministically (the lowest asset id with a
// blob), so a track resolves to exactly one blob and the listing is stable
// across calls — a client caches by id and a shifting list breaks it.
func (h *Handler) songs(ctx context.Context, workID, album, artist string, year int) ([]song, error) {
	rows, err := h.reader.QueryContext(ctx, `
		SELECT e.id,
		       COALESCE(json_extract(e.attributes, '$.track_title'), ''),
		       json_extract(e.attributes, '$.track'),
		       json_extract(e.attributes, '$.disc'),
		       e.edition_type,
		       b.hash, b.size, COALESCE(a.mime, b.mime, ''), COALESCE(a.filename, ''),
		       p.duration_seconds, p.bitrate_bps
		FROM editions e
		JOIN assets a ON a.id = (
			SELECT a2.id FROM assets a2
			WHERE a2.edition_id = e.id AND a2.blob_hash IS NOT NULL
			ORDER BY a2.id LIMIT 1)
		JOIN blobs b ON b.hash = a.blob_hash
		LEFT JOIN blob_probes p ON p.blob_hash = b.hash
		WHERE e.work_id = ?
		ORDER BY COALESCE(json_extract(e.attributes, '$.disc'), 0),
		         COALESCE(json_extract(e.attributes, '$.track'), 0),
		         e.id`, workID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []song{}
	for rows.Next() {
		var (
			editionID, trackTitle, editionType, hash, mime, filename string
			size                                                     int64
			track, disc                                              sql.NullInt64
			duration                                                 sql.NullFloat64
			bitrate                                                  sql.NullInt64
		)
		if err := rows.Scan(&editionID, &trackTitle, &track, &disc, &editionType,
			&hash, &size, &mime, &filename, &duration, &bitrate); err != nil {
			return nil, err
		}
		title := trackTitle
		if title == "" {
			title = titleFromFilename(filename)
		}
		s := song{
			ID:          trackID(editionID),
			Parent:      albumID(workID),
			IsDir:       false,
			Title:       title,
			Album:       album,
			Artist:      artist,
			Track:       int(track.Int64),
			DiscNumber:  int(disc.Int64),
			Year:        year,
			AlbumID:     albumID(workID),
			Size:        size,
			ContentType: mime,
			Suffix:      suffixOf(filename, editionType),
			Type:        "music",
		}
		if artist != "" {
			s.ArtistID = artistID(artist)
		}
		if duration.Valid {
			s.Duration = int(duration.Float64 + 0.5)
		}
		if bitrate.Valid {
			s.BitRate = int(bitrate.Int64 / 1000)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// clampInt parses a query integer, falling back to def and bounding to [lo,hi].
func clampInt(raw string, def, lo, hi int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// titleFromFilename is the last-ditch song title when an edition carries no
// track_title: the filename without its extension, which is at least what the
// user sees on disk.
func titleFromFilename(filename string) string {
	if filename == "" {
		return "Untitled"
	}
	base := filename
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	if base == "" {
		return "Untitled"
	}
	return base
}
