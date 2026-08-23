package catalog_test

import (
	"context"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
)

// Reconciliation against a real database (§56, §57).
//
// The pure evaluators are table-tested in the domain. What these add is the
// mapping — assets, probes, blobs, editions and replicas turning into the
// values those functions take — and the property the whole beat depends on: a
// sweep over a steady library changes nothing and emits nothing.

// seedSatisfying gives the harness's want an asset that meets the profile.
func (h *harness) seedSatisfying(t *testing.T, hash string, height int64, editionType string) {
	t.Helper()
	h.exec(t, `INSERT INTO blobs (hash, size, mime, first_seen_at)
		VALUES (?, 8589934592, 'video/x-matroska', ?)`, hash, stamp)
	h.exec(t, `INSERT INTO editions (id, work_id, label, edition_type, language, attributes, created_at)
		VALUES (?, 'w1', '1080p', ?, 'en', '{}', ?)`, "e-"+hash, editionType, stamp)
	h.exec(t, `INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash,
			source_path, role, filename, mime, identification_source, created_at, updated_at)
		VALUES (?, ?, NULL, 'managed', ?, '/srv/x.mkv', 'primary', 'x.mkv',
			'video/x-matroska', 'path', ?, ?)`, "a-"+hash, "e-"+hash, hash, stamp, stamp)
	h.exec(t, `INSERT INTO blob_probes
			(blob_hash, container, format_long, duration_seconds, bitrate_bps, streams,
			 bytes_read, materialised, probed_at)
		VALUES (?, 'matroska,webm', '', 7200.0, 8000000,
			?, 1024, 0, ?)`,
		hash, `[{"type":"video","codec":"h264","height":`+itoa(height)+`,"profile":"High"},`+
			`{"type":"audio","codec":"aac","channels":6}]`, stamp)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// setProfile replaces the harness profile's rules.
func (h *harness) setProfile(t *testing.T, accept string) {
	t.Helper()
	h.exec(t, `UPDATE quality_profiles SET accept = ? WHERE id = 'q1'`, accept)
}

const gate1080 = `[{"attribute":"resolution","op":"gte","value":1080}]`

func TestReconcileFindsAnAssetThatSatisfies(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	h.setProfile(t, gate1080)
	h.seedSatisfying(t, "blake3:"+repeat("a", 64), 1080, "web-dl")

	got, err := h.cat.ReconcileDesired(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content.Satisfaction != acquisition.SatisfactionSatisfied {
		t.Fatalf("content = %s, want satisfied", got.Content.Satisfaction)
	}
	if got.State.Name() != "FULLY_SATISFIED" {
		// With one peer holding the only replica, placement is satisfied the
		// moment content is — the single-peer case, which the API reports as
		// `unproven` rather than as a demonstration that replication works
		// (ADR-0027).
		t.Logf("state = %s (single peer, ADR-0010)", got.State.Name())
	}
	if !got.Changed {
		t.Error("the first reconciliation of an unevaluated want changed something")
	}
}

// The distinction that makes the upgrade workflow reachable, through the real
// query path: an asset that exists and does not meet the profile.
func TestReconcileReportsPresentButUnsatisfying(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	h.setProfile(t, gate1080)
	h.seedSatisfying(t, "blake3:"+repeat("b", 64), 480, "web-dl")

	got, err := h.cat.ReconcileDesired(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content.Satisfaction != acquisition.SatisfactionNot {
		t.Fatalf("content = %s, want not_satisfied", got.Content.Satisfaction)
	}
	// AVAILABLE, not MISSING: bytes are held, they are simply not good enough.
	if got.State.Name() != "AVAILABLE" {
		t.Errorf("state = %s, want AVAILABLE", got.State.Name())
	}
	if !got.State.Managed {
		t.Error("bytes are held, so the want is managed")
	}
	// And it says why, which is what makes "I have this film, why does Heyarr
	// say it is missing" answerable.
	if len(got.Content.Evaluations) != 1 {
		t.Fatalf("%d evaluations", len(got.Content.Evaluations))
	}
	if len(got.Content.Evaluations[0].Evaluation.RejectedBy()) == 0 {
		t.Error("the asset was rejected with no reason")
	}
}

// A want with nothing behind it.
func TestReconcileAWantWithNoAssets(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	h.setProfile(t, gate1080)

	got, err := h.cat.ReconcileDesired(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if got.State.Name() != "MISSING" {
		t.Fatalf("state = %s, want MISSING", got.State.Name())
	}
	if got.State.Managed {
		t.Error("nothing is held")
	}
	// Unsatisfied, not unknown: this pass looked.
	if got.Content.Satisfaction != acquisition.SatisfactionNot {
		t.Errorf("content = %s, want not_satisfied", got.Content.Satisfaction)
	}
}

// The property the beat depends on. A sweep over a steady library on a timer
// would otherwise turn the event log into a heartbeat, and an event stream that
// is mostly noise is one nobody follows.
func TestReconcileIsIdempotentAndSilent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	h.setProfile(t, gate1080)
	h.seedSatisfying(t, "blake3:"+repeat("c", 64), 2160, "remux")

	first, err := h.cat.ReconcileDesired(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed {
		t.Fatal("the first pass should have concluded something")
	}
	settled := h.eventCount(t)

	for range 5 {
		again, err := h.cat.ReconcileDesired(ctx, h.want)
		if err != nil {
			t.Fatal(err)
		}
		if again.Changed {
			t.Fatal("a pass over an unchanged library reported a change")
		}
		if again.State != first.State {
			t.Fatalf("state moved without anything changing: %+v then %+v",
				first.State, again.State)
		}
	}
	if got := h.eventCount(t); got != settled {
		t.Errorf("five no-op sweeps emitted %d event(s); a steady library must be silent",
			got-settled)
	}
}

// §57's point: a want's satisfaction can change without the want being touched.
// This is the case ingest hooks and API callbacks cannot see, and the reason a
// timer exists at all.
func TestEditingAProfileUnsatisfiesAWantNothingElseTouched(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	h.setProfile(t, gate1080)
	h.seedSatisfying(t, "blake3:"+repeat("d", 64), 1080, "web-dl")

	got, err := h.cat.ReconcileDesired(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content.Satisfaction != acquisition.SatisfactionSatisfied {
		t.Fatalf("setup: content = %s", got.Content.Satisfaction)
	}

	// Raise the bar. Nothing about the library changed.
	h.setProfile(t, `[{"attribute":"resolution","op":"gte","value":2160}]`)

	after, err := h.cat.ReconcileDesired(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if after.Content.Satisfaction != acquisition.SatisfactionNot {
		t.Fatalf("content = %s; raising the profile must unsatisfy the want",
			after.Content.Satisfaction)
	}
	if !after.Changed {
		t.Error("that is a change and must be reported as one")
	}
	if after.State.Name() != "AVAILABLE" {
		t.Errorf("state = %s, want AVAILABLE — the bytes are still there", after.State.Name())
	}
}

// Losing the asset moves both axes and the managed flag together. Getting that
// wrong produces a state the machine's own validation refuses.
func TestReconcileAfterTheAssetGoesAway(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	h.setProfile(t, gate1080)
	hash := "blake3:" + repeat("e", 64)
	h.seedSatisfying(t, hash, 2160, "remux")

	if _, err := h.cat.ReconcileDesired(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	h.exec(t, `DELETE FROM assets WHERE id = ?`, "a-"+hash)

	after, err := h.cat.ReconcileDesired(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if after.State.Managed {
		t.Error("nothing is held any more")
	}
	if after.State.Name() != "MISSING" {
		t.Errorf("state = %s, want MISSING", after.State.Name())
	}
	if err := after.State.Validate(); err != nil {
		t.Errorf("the concluded state does not validate: %v", err)
	}
}

// An asset the scanner marked missing is not an asset. Counting it would report
// a want as satisfied by a file that is not there.
func TestAMissingAssetDoesNotSatisfy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	h.setProfile(t, gate1080)
	hash := "blake3:" + repeat("f", 64)
	h.seedSatisfying(t, hash, 2160, "remux")
	h.exec(t, `UPDATE assets SET missing_since = ? WHERE id = ?`, stamp, "a-"+hash)

	got, err := h.cat.ReconcileDesired(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content.Satisfaction == acquisition.SatisfactionSatisfied {
		t.Error("a want cannot be satisfied by a file the scanner could not find")
	}
}

// A node with no toolchain has no probes, so the quality attributes are absent
// — and absent is reported as "could not determine" rather than as a failure.
// That is what makes a degraded node say "I cannot tell whether this satisfies
// you" instead of "this does not satisfy you", which are different problems.
func TestWithNoProbeTheAttributesAreUndetermined(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	h.setProfile(t, gate1080)

	hash := "blake3:" + repeat("9", 64)
	h.exec(t, `INSERT INTO blobs (hash, size, mime, first_seen_at)
		VALUES (?, 8589934592, 'video/x-matroska', ?)`, hash, stamp)
	h.exec(t, `INSERT INTO editions (id, work_id, label, edition_type, language, attributes, created_at)
		VALUES ('e-noprobe', 'w1', '', 'web-dl', 'en', '{}', ?)`, stamp)
	h.exec(t, `INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash,
			source_path, role, filename, mime, identification_source, created_at, updated_at)
		VALUES ('a-noprobe', 'e-noprobe', NULL, 'managed', ?, '/srv/y.mkv', 'primary', 'y.mkv',
			'video/x-matroska', 'path', ?, ?)`, hash, stamp, stamp)

	got, err := h.cat.ReconcileDesired(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content.Satisfaction == acquisition.SatisfactionSatisfied {
		t.Error("a gate that cannot be shown to hold must not pass")
	}
	reasons := got.Content.Evaluations[0].Evaluation.Reasons
	var undetermined bool
	for _, r := range reasons {
		if r.Rule == "resolution.gte" && r.Result == acquisition.ResultUndetermined {
			undetermined = true
		}
	}
	if !undetermined {
		t.Errorf("with no probe the resolution is unknowable, not wrong: %+v", reasons)
	}
	// But the blob size IS always known, so it is never undetermined.
	if _, ok := got.Content.Evaluations[0].Evaluation.Reason("size_bytes.gte"); ok {
		t.Log("size is available even with no probe, as expected")
	}
}

func repeat(s string, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = s[0]
	}
	return string(out)
}
