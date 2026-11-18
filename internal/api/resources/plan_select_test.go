//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Planning by work (#885): the node chooses which of a film's copies to plan
// from what the client can show.

const (
	editionFHDID = "01990000-0000-7000-8000-00000000e1fh"
	assetFHDID   = "01990000-0000-7000-8000-00000000a1fh"
	blobFHDHash  = "blake3:abababababababababababababababababababababababababababababababab"
)

// seedTwoCopies gives Arrival (work1) the issue's two copies: the seeded
// 2160p HDR remux (asset1, edition1) and a 1080p SDR encode in an edition of
// its own, both probed and both held here.
func (h *harness) seedTwoCopies() *harness {
	h.t.Helper()
	h.exec(`INSERT INTO blob_probes (blob_hash, container, format_long, duration_seconds, bitrate_bps, streams, bytes_read, materialised, probed_at)
		VALUES (?, 'matroska,webm', '', 6960, 60000000, ?, 0, 0, ?)`, blob1Hash,
		`[{"index":0,"type":"video","codec":"hevc","profile":"Main 10","width":3840,"height":2160,"color_transfer":"smpte2084"},`+
			`{"index":1,"type":"audio","codec":"eac3","channels":6}]`, seedTime)
	h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 4294967296, 'video/x-matroska', ?)`,
		blobFHDHash, seedTime)
	h.exec(`INSERT INTO blob_probes (blob_hash, container, format_long, duration_seconds, bitrate_bps, streams, bytes_read, materialised, probed_at)
		VALUES (?, 'matroska,webm', '', 6960, 8000000, ?, 0, 0, ?)`, blobFHDHash,
		`[{"index":0,"type":"video","codec":"h264","profile":"High","width":1920,"height":1080,"color_transfer":"bt709"},`+
			`{"index":1,"type":"audio","codec":"aac","channels":2}]`, seedTime)
	h.exec(`INSERT INTO editions (id, work_id, label, edition_type, language, attributes, edition_key, created_at)
		VALUES (?, ?, '1080p', 'web', 'en', '{}', '1080p', ?)`, editionFHDID, work1ID, seedTime)
	h.exec(`INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash, source_path,
			role, filename, mime, identification_source, missing_since, created_at, updated_at)
		VALUES (?, ?, ?, 'managed', ?, '/srv/films/Arrival.1080p.mkv', 'primary', 'Arrival.1080p.mkv',
			'video/x-matroska', 'path', NULL, ?, ?)`,
		assetFHDID, editionFHDID, libFilmsID, blobFHDHash, seedTime, seedTime)
	h.exec(`INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, verified_at, reported_at, updated_at)
		VALUES (?, ?, 'present', 4294967296, ?, ?, ?)`, blobFHDHash, peerID, seedTime, seedTime, seedTime)
	return h
}

type selectedPlan struct {
	clientPlan
	AssetID   string `json:"asset_id"`
	Selection *struct {
		Chosen       selectionCandidate   `json:"chosen"`
		Alternatives []selectionCandidate `json:"alternatives"`
	} `json:"selection"`
}

type selectionCandidate struct {
	AssetID      string     `json:"asset_id"`
	EditionLabel string     `json:"edition_label"`
	Height       *int       `json:"height"`
	HDR          *bool      `json:"hdr"`
	VideoCodec   string     `json:"video_codec"`
	Reason       planReason `json:"reason"`
}

func (h *harness) planBody(t *testing.T, body string) (*http.Response, selectedPlan, []byte) {
	t.Helper()
	resp := h.do(http.MethodPost, "/api/v1/playback/plan", "", strings.NewReader(body))
	raw := h.body(resp)
	var p selectedPlan
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("undecodable plan: %v\n%s", err, raw)
		}
	}
	return resp, p, raw
}

const phoneSDR = `{"containers":["mp4","mkv"],"video":["h264","hevc"],"audio":["aac","eac3"],"max_height":1080}`

// The issue's acceptance: 1080 cap, no HDR declared, picks the 1080p copy and
// plays it without a video re-encode.
func TestPlanByWorkPicksTheCopyThatFits(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedTwoCopies()

	resp, p, raw := h.planBody(t, `{"work_id":"`+work1ID+`","client":`+phoneSDR+`}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	if p.AssetID != assetFHDID {
		t.Fatalf("asset_id = %s, want the 1080p copy %s\n%s", p.AssetID, assetFHDID, raw)
	}
	if p.Mode != "direct" {
		t.Errorf("mode = %q (reason %q), want direct: the 1080p copy needs no video re-encode", p.Mode, p.Reason)
	}
	if p.Selection == nil {
		t.Fatalf("no selection block on a plan by work\n%s", raw)
	}
	chosen := p.Selection.Chosen
	if chosen.AssetID != assetFHDID || chosen.Reason.Code != "best_fit" || chosen.EditionLabel != "1080p" {
		t.Errorf("chosen = %+v", chosen)
	}
	if chosen.Height == nil || *chosen.Height != 1080 || chosen.HDR == nil || *chosen.HDR || chosen.VideoCodec != "h264" {
		t.Errorf("chosen facts = height %v hdr %v codec %q", chosen.Height, chosen.HDR, chosen.VideoCodec)
	}
	if len(p.Selection.Alternatives) != 1 {
		t.Fatalf("alternatives = %+v, want the 2160p copy", p.Selection.Alternatives)
	}
	alt := p.Selection.Alternatives[0]
	if alt.AssetID != asset1ID || alt.Reason.Code != "resolution_too_high" || alt.HDR == nil || !*alt.HDR {
		t.Errorf("alternative = %+v", alt)
	}
}

// The issue's acceptance: 2160 cap with HDR declared picks the 2160p copy.
func TestPlanByWorkWithHDRDeclaredPicksTheHDRCopy(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedTwoCopies()

	resp, p, raw := h.planBody(t, `{"work_id":"`+work1ID+`","client":{"containers":["mkv"],"video":["h264","hevc"],"audio":["aac","eac3"],"max_height":2160,"supports_hdr":true}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	if p.AssetID != asset1ID || p.Selection == nil || p.Selection.Chosen.Reason.Code != "best_fit" {
		t.Fatalf("asset_id = %s, want the 2160p HDR copy\n%s", p.AssetID, raw)
	}
	if alt := p.Selection.Alternatives; len(alt) != 1 || alt[0].Reason.Code != "smaller" {
		t.Errorf("alternatives = %+v, want the 1080p copy passed over as smaller", alt)
	}
}

// The same 2160 cap without declaring HDR prefers the SDR copy: nothing here
// tone-maps (#886).
func TestPlanByWorkWithoutHDRPrefersSDR(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedTwoCopies()

	_, p, raw := h.planBody(t, `{"work_id":"`+work1ID+`","client":{"video":["h264","hevc"],"max_height":2160}}`)
	if p.AssetID != assetFHDID || p.Selection == nil || p.Selection.Alternatives[0].Reason.Code != "hdr_not_declared" {
		t.Fatalf("asset_id = %s, want the SDR copy with the HDR one passed over as hdr_not_declared\n%s", p.AssetID, raw)
	}
}

// An explicit asset_id always wins, even beside a work_id, and the answer is
// byte-for-byte the plan an older client gets by sending asset_id alone.
func TestAnExplicitAssetWinsAndPlansAsBefore(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedTwoCopies()

	resp, alone, rawAlone := h.planBody(t, `{"asset_id":"`+asset1ID+`","client":`+phoneSDR+`}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, rawAlone)
	}
	if alone.AssetID != asset1ID || alone.Selection != nil || bytes.Contains(rawAlone, []byte(`"selection"`)) {
		t.Fatalf("a plan by asset_id carries a selection or a different asset\n%s", rawAlone)
	}
	// The planner's view of an HDR source is what it was: the better HDR flag
	// steers selection only, so a client that declared nothing about HDR is
	// not suddenly told this needs a transcode.
	if alone.has("hdr_unsupported") {
		t.Errorf("an asset_id plan gained hdr_unsupported: %+v", alone.Reasons)
	}
	if alone.Mode != "direct" || alone.Reason != "video 2160p exceeds client max_height 1080 (transcoding video); this node has no ffmpeg, so the bytes are handed over as they are" {
		t.Errorf("mode = %q reason = %q", alone.Mode, alone.Reason)
	}

	_, both, rawBoth := h.planBody(t, `{"asset_id":"`+asset1ID+`","work_id":"`+work1ID+`","client":`+phoneSDR+`}`)
	if !bytes.Equal(rawAlone, rawBoth) {
		t.Errorf("work_id beside asset_id changed the answer:\nalone %s\nboth  %s", rawAlone, rawBoth)
	}
	if both.AssetID != asset1ID {
		t.Errorf("asset_id = %s, want the explicit %s", both.AssetID, asset1ID)
	}
}

// A request that names a registered device and declares nothing is chosen
// for by the device's stored capabilities.
func TestPlanByWorkFallsBackToTheDevicesStoredCapabilities(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedTwoCopies()

	// device1 is a 2160p HDR television.
	_, p, raw := h.planBody(t, `{"work_id":"`+work1ID+`","device_id":"`+device1ID+`"}`)
	if p.AssetID != asset1ID || p.Selection == nil {
		t.Fatalf("television: asset_id = %s, want the 2160p HDR copy\n%s", p.AssetID, raw)
	}

	// The same television capped at 1080 and without HDR.
	h.exec(`UPDATE devices SET max_height = 1080, supports_hdr = 0 WHERE id = ?`, device1ID)
	_, p, raw = h.planBody(t, `{"work_id":"`+work1ID+`","device_id":"`+device1ID+`"}`)
	if p.AssetID != assetFHDID {
		t.Fatalf("capped television: asset_id = %s, want the 1080p copy\n%s", p.AssetID, raw)
	}

	// A client declaration beats the stored value field by field: HDR
	// declared here, the device's 1080 cap kept.
	_, p, raw = h.planBody(t, `{"work_id":"`+work1ID+`","device_id":"`+device1ID+`","client":{"supports_hdr":true}}`)
	if p.AssetID != assetFHDID || p.Selection.Alternatives[0].Reason.Code != "resolution_too_high" {
		t.Fatalf("declared HDR, stored 1080 cap: asset_id = %s\n%s", p.AssetID, raw)
	}
}

// edition_id narrows the choice to one edition's copies; with only the
// 2160p copy left and a 1080 cap, it is the smallest over the limit.
func TestPlanByWorkWithinAnEdition(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedTwoCopies()

	_, p, raw := h.planBody(t, `{"work_id":"`+work1ID+`","edition_id":"`+edition1ID+`","client":`+phoneSDR+`}`)
	if p.AssetID != asset1ID || p.Selection == nil || p.Selection.Chosen.Reason.Code != "smallest_over_limit" {
		t.Fatalf("asset_id = %s, want the edition's only copy as smallest_over_limit\n%s", p.AssetID, raw)
	}
	if len(p.Selection.Alternatives) != 0 {
		t.Errorf("alternatives = %+v, want none outside the edition", p.Selection.Alternatives)
	}
}

func TestPlanByWorkRefusals(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedTwoCopies()

	for _, tc := range []struct {
		name, body string
		status     int
		detail     string
	}{
		{"neither asset nor work", `{"client":` + phoneSDR + `}`, 400, "asset_id or work_id is required"},
		{"a work and no client or device", `{"work_id":"` + work1ID + `"}`, 400, "device_id is required"},
		{"an unknown work", `{"work_id":"nope","client":` + phoneSDR + `}`, 404, ""},
		{"a work that is not a film", `{"work_id":"` + work3ID + `","client":` + phoneSDR + `}`, 400, "plan an episode or track by asset_id"},
		// work2's only copy is missing from disk.
		{"a film with no playable copy", `{"work_id":"` + work2ID + `","client":` + phoneSDR + `}`, 404, "no playable copy"},
		{"an unknown device", `{"work_id":"` + work1ID + `","device_id":"nope"}`, 404, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, _, raw := h.planBody(t, tc.body)
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.status, raw)
			}
			if tc.detail != "" && !strings.Contains(string(raw), tc.detail) {
				t.Errorf("body %s does not say %q", raw, tc.detail)
			}
		})
	}
}

// Browse's primary_asset for a film is the copy a plan by work_id chooses for
// a client that declares nothing (ADR-0123). The pick is SQL and the choice is
// Go, so this holds them to one answer across the layouts that matter.
func TestBrowseDefaultIsTheCopyAnUndeclaredPlanChooses(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedTwoCopies()

	agree := func(t *testing.T, want string) {
		t.Helper()
		var detail struct {
			PrimaryAsset struct {
				AssetID string `json:"asset_id"`
			} `json:"primary_asset"`
		}
		decode(t, h, "/api/v1/works/"+work1ID, &detail)
		_, p, raw := h.planBody(t, `{"work_id":"`+work1ID+`","client":{}}`)
		if p.AssetID != detail.PrimaryAsset.AssetID {
			t.Fatalf("browse default %s, undeclared plan %s — they must agree\n%s",
				detail.PrimaryAsset.AssetID, p.AssetID, raw)
		}
		if p.AssetID != want {
			t.Errorf("both chose %s, want %s", p.AssetID, want)
		}
	}

	// 2160p HDR and 1080p SDR: SDR, though it is shorter and newer.
	agree(t, assetFHDID)

	// A 2160p SDR copy joins: the tallest SDR.
	const (
		editionUHDSDR = "01990000-0000-7000-8000-00000000e1us"
		assetUHDSDR   = "01990000-0000-7000-8000-00000000a1us"
		blobUHDSDR    = "blake3:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	)
	h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 21474836480, 'video/x-matroska', ?)`, blobUHDSDR, seedTime)
	h.exec(`INSERT INTO blob_probes (blob_hash, container, format_long, duration_seconds, bitrate_bps, streams, bytes_read, materialised, probed_at)
		VALUES (?, 'matroska,webm', '', 6960, 30000000, ?, 0, 0, ?)`, blobUHDSDR,
		`[{"index":0,"type":"video","codec":"hevc","width":3840,"height":2160,"color_transfer":"bt709"}]`, seedTime)
	h.exec(`INSERT INTO editions (id, work_id, label, edition_type, language, attributes, edition_key, created_at)
		VALUES (?, ?, '2160p SDR', 'web', 'en', '{}', '2160p-sdr', ?)`, editionUHDSDR, work1ID, seedTime)
	h.exec(`INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash, source_path,
			role, filename, mime, identification_source, missing_since, created_at, updated_at)
		VALUES (?, ?, ?, 'managed', ?, '/srv/films/Arrival.2160p.sdr.mkv', 'primary', 'Arrival.2160p.sdr.mkv',
			'video/x-matroska', 'path', NULL, ?, ?)`, assetUHDSDR, editionUHDSDR, libFilmsID, blobUHDSDR, seedTime, seedTime)
	h.exec(`INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, verified_at, reported_at, updated_at)
		VALUES (?, ?, 'present', 21474836480, ?, ?, ?)`, blobUHDSDR, peerID, seedTime, seedTime, seedTime)
	agree(t, assetUHDSDR)

	// Nothing probed: the lowest id, which is what "oldest" used to mean.
	h.exec(`DELETE FROM blob_probes`)
	agree(t, asset1ID)
}

// A series keeps creation order: its primary-role files are different
// episodes, and ranking them by picture would pick an arbitrary one.
func TestBrowseDefaultForASeriesIsStillTheFirstFile(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed()

	const (
		series   = "01990000-0000-7000-8000-00000000ws01"
		ep1, ep2 = "01990000-0000-7000-8000-00000000es01", "01990000-0000-7000-8000-00000000es02"
		a1, a2   = "01990000-0000-7000-8000-00000000as01", "01990000-0000-7000-8000-00000000as02"
		b1       = "blake3:e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1"
		b2       = "blake3:e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2"
	)
	h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES (?, 'series', 'a-series', 'A Series', 'series', 2020, '{}', ?, ?)`, series, seedTime, seedTime)
	h.exec(`INSERT INTO editions (id, work_id, label, edition_type, language, attributes, edition_key, created_at) VALUES
		(?, ?, 'S01E01', 'episode', NULL, '{}', 's01e01', ?), (?, ?, 'S01E02', 'episode', NULL, '{}', 's01e02', ?)`,
		ep1, series, seedTime, ep2, series, seedTime)
	h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 1, 'video/mp4', ?), (?, 1, 'video/mp4', ?)`,
		b1, seedTime, b2, seedTime)
	// The second episode is the taller, probed one; the first is unprobed.
	h.exec(`INSERT INTO blob_probes (blob_hash, container, format_long, duration_seconds, bitrate_bps, streams, bytes_read, materialised, probed_at)
		VALUES (?, 'mov,mp4', '', 2700, 9000000, '[{"index":0,"type":"video","codec":"h264","height":2160}]', 0, 0, ?)`, b2, seedTime)
	h.exec(`INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash, source_path,
			role, filename, mime, identification_source, missing_since, created_at, updated_at) VALUES
		(?, ?, ?, 'managed', ?, '/srv/tv/e1.mp4', 'primary', 'e1.mp4', 'video/mp4', 'path', NULL, ?, ?),
		(?, ?, ?, 'managed', ?, '/srv/tv/e2.mp4', 'primary', 'e2.mp4', 'video/mp4', 'path', NULL, ?, ?)`,
		a1, ep1, libFilmsID, b1, seedTime, seedTime, a2, ep2, libFilmsID, b2, seedTime, seedTime)

	var detail struct {
		PrimaryAsset struct {
			AssetID string `json:"asset_id"`
		} `json:"primary_asset"`
	}
	decode(t, h, "/api/v1/works/"+series, &detail)
	if detail.PrimaryAsset.AssetID != a1 {
		t.Errorf("series primary_asset = %s, want the first episode's file %s", detail.PrimaryAsset.AssetID, a1)
	}
}
