package resources

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Devices are capability profiles (§68), and that is the whole of what they
// are in this milestone.
//
// The playback planner needs three things: the Asset, the device's
// capabilities, and replica availability. This is the second, and it is the
// only one of the three that Heyarr cannot work out for itself — there is no
// way to interrogate a television. So a client declares what it can play and
// Heyarr believes it.
//
// That is why this whole lane is independent of ffprobe: nothing here decodes
// anything.
//
// # What a device is NOT, here
//
// Not an identity. ADR-0022 covers enrolment, device keypairs and key
// recovery, and that is Milestone 8. Nothing in this file authenticates
// anything, and a `device_key` is a client's own stable string for its own
// deduplication — not a credential, not a secret, and never checked against
// one. If it starts being treated as either, the milestone ordering has been
// broken and the personal-state plane will have to be retrofitted around it.

// maxCodecs bounds a declared list. A real profile names a handful of codecs;
// this is not a limit anyone meets, it is a limit on what one write token can
// make the planner iterate over on every playback decision.
const maxCodecs = 64

// maxDimension is a sanity ceiling on a declared resolution. 16× UHD, which no
// device does and which is comfortably past anything real, so a client that
// sends 4294967295 is caught while one that sends 7680 is not.
const maxDimension = 30720

// Device is the wire type.
type Device struct {
	ID        string `json:"id"`
	DeviceKey string `json:"device_key"`
	Name      string `json:"name"`
	Platform  string `json:"platform"`
	// Profile is what this device says it can play.
	Profile    DeviceProfile `json:"profile"`
	CreatedAt  time.Time     `json:"created_at"`
	UpdatedAt  time.Time     `json:"updated_at"`
	LastSeenAt time.Time     `json:"last_seen_at"`
}

// DeviceProfile is a declared capability set (§68).
//
// A zero maximum means "no limit stated", which is deliberately different from
// a limit of zero and must stay tellable apart: a client that omits
// max_bitrate_bps is not claiming it can play nothing.
type DeviceProfile struct {
	Containers    []string `json:"containers"`
	VideoCodecs   []string `json:"video_codecs"`
	AudioCodecs   []string `json:"audio_codecs"`
	MaxWidth      int64    `json:"max_width"`
	MaxHeight     int64    `json:"max_height"`
	MaxBitrateBPS int64    `json:"max_bitrate_bps"`
	SupportsHDR   bool     `json:"supports_hdr"`
}

// registerDeviceRequest is the POST /devices body.
type registerDeviceRequest struct {
	DeviceKey string         `json:"device_key"`
	Name      string         `json:"name"`
	Platform  string         `json:"platform"`
	Profile   *DeviceProfile `json:"profile"`
}

const deviceColumns = `id, device_key, name, platform,
	containers, video_codecs, audio_codecs,
	max_width, max_height, max_bitrate_bps, supports_hdr,
	created_at, updated_at, last_seen_at`

func scanDeviceRow(row interface{ Scan(...any) error }) (Device, error) {
	var d Device
	var containers, video, audio string
	var hdr int
	var created, updated, lastSeen string
	if err := row.Scan(&d.ID, &d.DeviceKey, &d.Name, &d.Platform,
		&containers, &video, &audio,
		&d.Profile.MaxWidth, &d.Profile.MaxHeight, &d.Profile.MaxBitrateBPS, &hdr,
		&created, &updated, &lastSeen); err != nil {
		return Device{}, err
	}
	d.Profile.SupportsHDR = hdr == 1
	for _, pair := range []struct {
		raw  string
		dest *[]string
	}{
		{containers, &d.Profile.Containers},
		{video, &d.Profile.VideoCodecs},
		{audio, &d.Profile.AudioCodecs},
	} {
		list, err := decodeStringList(pair.raw)
		if err != nil {
			return Device{}, err
		}
		*pair.dest = list
	}
	d.CreatedAt = parseTime(created)
	d.UpdatedAt = parseTime(updated)
	d.LastSeenAt = parseTime(lastSeen)
	return d, nil
}

// decodeStringList reads one of the JSON list columns, normalising null and an
// absent value to an empty list rather than nil — a client should not have to
// handle both null and [] for "this device declares no containers".
func decodeStringList(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{}, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("decoding a device capability list: %w", err)
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// registerDevice is POST /api/v1/devices, and it is an UPSERT on device_key.
//
// An app announces itself on every launch. If that created a row each time,
// this table would fill with duplicates named "Living Room" and the planner
// would be choosing between four thousand copies of one television. So
// registration converges: same key, same row, profile updated.
func (a *API) registerDevice(w http.ResponseWriter, r *http.Request) {
	var body registerDeviceRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if err := required("device_key", body.DeviceKey); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if err := required("name", body.Name); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	profile := DeviceProfile{}
	if body.Profile != nil {
		profile = *body.Profile
	}
	if err := validateProfile(&profile); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	now := a.now().UTC()
	device := Device{
		ID:         a.newID(),
		DeviceKey:  strings.TrimSpace(body.DeviceKey),
		Name:       strings.TrimSpace(body.Name),
		Platform:   strings.TrimSpace(body.Platform),
		Profile:    profile,
		CreatedAt:  now,
		UpdatedAt:  now,
		LastSeenAt: now,
	}

	var (
		event   events.Event
		emitted bool
		created bool
	)
	err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		existing, err := deviceByKey(r.Context(), tx, device.DeviceKey)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			created = true
		case err != nil:
			return err
		default:
			// Keep the identity and the creation time; everything else is
			// what the client is telling us now.
			device.ID = existing.ID
			device.CreatedAt = existing.CreatedAt
		}

		if err := upsertDevice(r.Context(), tx, device); err != nil {
			return err
		}

		// Invariant 7: every state transition emits an event.
		//
		// "Every state transition" is doing work here — a re-registration that
		// changes nothing is not a transition. Emitting anyway would turn every
		// app launch in the house into an event, and an event stream that is
		// mostly noise is one nobody can follow, which defeats the point of
		// having one.
		//
		// The events live under playback.* rather than a new namespace: §76
		// enumerates the categories, a device is not content, and it exists
		// for playback.
		switch {
		case created:
			event, err = a.events.EmitTx(r.Context(), tx, events.TypeDeviceRegistered, "device", device.ID,
				map[string]any{"device_id": device.ID, "device_key": device.DeviceKey, "name": device.Name})
			emitted = true
		case !sameDevice(existing, device):
			event, err = a.events.EmitTx(r.Context(), tx, events.TypeDeviceUpdated, "device", device.ID,
				map[string]any{"device_id": device.ID, "device_key": device.DeviceKey, "name": device.Name})
			emitted = true
		}
		return err
	})
	if err != nil {
		a.fail(w, r, "device", err)
		return
	}
	if emitted {
		a.events.Publish(event)
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
		w.Header().Set("Location", httpapi.APIPrefix+"/devices/"+device.ID)
	}
	a.write(w, r, status, device)
}

// sameDevice reports whether a re-registration is a no-op.
//
// last_seen_at is deliberately excluded: it changes on every registration by
// definition, so including it would make every comparison unequal and every
// launch an event — which is precisely the behaviour this function exists to
// prevent.
func sameDevice(a, b Device) bool {
	return a.Name == b.Name &&
		a.Platform == b.Platform &&
		equalStrings(a.Profile.Containers, b.Profile.Containers) &&
		equalStrings(a.Profile.VideoCodecs, b.Profile.VideoCodecs) &&
		equalStrings(a.Profile.AudioCodecs, b.Profile.AudioCodecs) &&
		a.Profile.MaxWidth == b.Profile.MaxWidth &&
		a.Profile.MaxHeight == b.Profile.MaxHeight &&
		a.Profile.MaxBitrateBPS == b.Profile.MaxBitrateBPS &&
		a.Profile.SupportsHDR == b.Profile.SupportsHDR
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func deviceByKey(ctx context.Context, tx *sql.Tx, key string) (Device, error) {
	return scanDeviceRow(tx.QueryRowContext(ctx,
		`SELECT `+deviceColumns+` FROM devices WHERE device_key = ?`, key))
}

func upsertDevice(ctx context.Context, tx *sql.Tx, d Device) error {
	containers, _ := json.Marshal(d.Profile.Containers)
	video, _ := json.Marshal(d.Profile.VideoCodecs)
	audio, _ := json.Marshal(d.Profile.AudioCodecs)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO devices (id, device_key, name, platform,
			containers, video_codecs, audio_codecs,
			max_width, max_height, max_bitrate_bps, supports_hdr,
			created_at, updated_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (device_key) DO UPDATE SET
			name = excluded.name,
			platform = excluded.platform,
			containers = excluded.containers,
			video_codecs = excluded.video_codecs,
			audio_codecs = excluded.audio_codecs,
			max_width = excluded.max_width,
			max_height = excluded.max_height,
			max_bitrate_bps = excluded.max_bitrate_bps,
			supports_hdr = excluded.supports_hdr,
			updated_at = excluded.updated_at,
			last_seen_at = excluded.last_seen_at`,
		d.ID, d.DeviceKey, d.Name, d.Platform,
		string(containers), string(video), string(audio),
		d.Profile.MaxWidth, d.Profile.MaxHeight, d.Profile.MaxBitrateBPS, boolToInt(d.Profile.SupportsHDR),
		d.CreatedAt.Format(timeFormat),
		d.UpdatedAt.Format(timeFormat),
		d.LastSeenAt.Format(timeFormat))
	return err
}

// validateProfile rejects a profile that cannot describe a real device.
//
// It normalises as well as checks, so that two clients spelling the same
// capability differently — "H264" and "h264" — converge instead of producing a
// planner that matches one and not the other. That normalisation is why this
// takes a pointer.
func validateProfile(p *DeviceProfile) error {
	for _, list := range []struct {
		name string
		vals *[]string
	}{
		{"containers", &p.Containers},
		{"video_codecs", &p.VideoCodecs},
		{"audio_codecs", &p.AudioCodecs},
	} {
		normalised, err := normaliseCodecList(list.name, *list.vals)
		if err != nil {
			return err
		}
		*list.vals = normalised
	}

	for _, dim := range []struct {
		name  string
		value int64
	}{
		{"max_width", p.MaxWidth},
		{"max_height", p.MaxHeight},
	} {
		if dim.value < 0 {
			return fmt.Errorf("%s must not be negative", dim.name)
		}
		if dim.value > maxDimension {
			return fmt.Errorf("%s of %d is not a real resolution", dim.name, dim.value)
		}
	}
	if p.MaxBitrateBPS < 0 {
		return errors.New("max_bitrate_bps must not be negative")
	}
	// A device that declares one dimension and not the other has described
	// half a limit, and the planner would have to guess what the other half
	// means. Guessing wrong makes something transcode that did not need to.
	if (p.MaxWidth == 0) != (p.MaxHeight == 0) {
		return errors.New("max_width and max_height must be given together, or neither")
	}
	return nil
}

// normaliseCodecList lower-cases, trims, rejects nonsense and de-duplicates.
func normaliseCodecList(field string, in []string) ([]string, error) {
	if len(in) > maxCodecs {
		return nil, fmt.Errorf("%s lists %d entries, which is more than %d", field, len(in), maxCodecs)
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		v := strings.ToLower(strings.TrimSpace(raw))
		if v == "" {
			return nil, fmt.Errorf("%s contains an empty entry", field)
		}
		// A codec name is a short token. Anything else is either a mistake or
		// an attempt to put something structured in a field that is not.
		if len(v) > 32 || strings.ContainsAny(v, " \t\n\"'") {
			return nil, fmt.Errorf("%s contains %q, which is not a codec or container name", field, raw)
		}
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out, nil
}

func (a *API) getDevice(w http.ResponseWriter, r *http.Request) {
	device, err := scanDeviceRow(a.reader.QueryRowContext(r.Context(),
		`SELECT `+deviceColumns+` FROM devices WHERE id = ?`, chi.URLParam(r, "id")))
	if err != nil {
		a.fail(w, r, "device", err)
		return
	}
	a.write(w, r, http.StatusOK, device)
}

func (a *API) listDevices(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r, "devices", 2)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	where := []string{"1 = 1"}
	args := []any{}
	if q.cursor != nil {
		where = append(where, "(name, id) > (?, ?)")
		args = append(args, q.cursor[0], q.cursor[1])
	}
	args = append(args, q.limit+1)

	//nolint:gosec // assembled only from the literal fragments above; every value is bound
	stmt := `SELECT ` + deviceColumns + ` FROM devices WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY name ASC, id ASC LIMIT ?`

	rows, err := a.reader.QueryContext(r.Context(), stmt, args...)
	if err != nil {
		a.fail(w, r, "device", err)
		return
	}
	defer func() { _ = rows.Close() }()

	var devices []Device
	for rows.Next() {
		d, err := scanDeviceRow(rows)
		if err != nil {
			a.fail(w, r, "device", err)
			return
		}
		devices = append(devices, d)
	}
	if err := rows.Err(); err != nil {
		a.fail(w, r, "device", err)
		return
	}
	a.write(w, r, http.StatusOK, newPage(devices, q.limit,
		func(d Device) []string { return []string{d.Name, d.ID} }, "devices"))
}
