package catalogop_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
)

var selAt = time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)

const (
	hashA = "0123456789abcdef0123456789abcdef01234567"
	hashB = "fedcba9876543210fedcba9876543210fedcba98"
)

func publicAcquire() catalogop.Acquire {
	return catalogop.Acquire{
		Scope:      catalogop.ScopeWork,
		Aspect:     catalogop.AspectPrimary,
		Profile:    "living-room",
		Infohash:   hashA,
		Title:      "A Film (1982) 2160p",
		WorkTitle:  "A Film",
		Year:       1982,
		Size:       15_679_021_336,
		SelectedAt: selAt,
		Site:       "north",
		Trackers:   []string{"udp://tracker.example.org:6969/announce", "https://tracker.example.net/announce"},
	}
}

func (p *peer) acquire(ct, wk string, a catalogop.Acquire, prev []string, at time.Time) string {
	p.t.Helper()
	tok, err := catalogop.SignAcquire(p.priv, ct, wk, a, prev, at)
	if err != nil {
		p.t.Fatalf("sign acquire %s/%s: %v", ct, wk, err)
	}
	return tok
}

// TestAcquireRoundTrip: an acquire op parses back to the body it carried, and
// the view names it as the effective acquisition for its scope and aspect.
func TestAcquireRoundTrip(t *testing.T) {
	p := newPeer(t)
	want := publicAcquire()
	tok := p.acquire("movie", "a-film-1982", want, nil, iat)
	op, err := catalogop.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if op.Kind != catalogop.OpAcquire || op.Acquire == nil {
		t.Fatalf("kind = %q, acquire = %v", op.Kind, op.Acquire)
	}
	got := *op.Acquire
	if got.Scope != want.Scope || got.Aspect != want.Aspect || got.Profile != want.Profile ||
		got.Infohash != want.Infohash || got.Title != want.Title || got.Size != want.Size ||
		got.WorkTitle != want.WorkTitle || got.Year != want.Year ||
		!got.SelectedAt.Equal(want.SelectedAt) || got.Site != want.Site || got.Private != want.Private ||
		strings.Join(got.Trackers, ",") != strings.Join(want.Trackers, ",") {
		t.Errorf("acquire round trip:\n got %+v\nwant %+v", got, want)
	}

	v := catalogop.Evaluate([]string{tok})
	eff, ok := v.AcquisitionFor(target("movie", "a-film-1982"), catalogop.ScopeWork, catalogop.AspectPrimary)
	if !ok || eff.Hash != op.Hash {
		t.Fatalf("effective acquisition = %v %v, want %s", ok, eff.Hash, op.Hash)
	}
	if len(v.Rejected) != 0 {
		t.Errorf("rejected = %v", v.Rejected)
	}
}

// TestAcquireCarriesNoSecret: the serialised op has exactly the fields ADR-0119
// allows — no source URL, no torrent bytes, no credential-bearing tracker —
// and a delete op's bytes are unchanged by the new kind (no `acq` key).
func TestAcquireCarriesNoSecret(t *testing.T) {
	p := newPeer(t)
	tok := p.acquire("movie", "a-film-1982", publicAcquire(), nil, iat)
	body := decodePayload(t, tok)
	acq, ok := body["acq"].(map[string]any)
	if !ok {
		t.Fatalf("payload has no acq object: %v", body)
	}
	wantKeys := map[string]bool{"scope": true, "aspect": true, "profile": true, "infohash": true, "title": true, "work_title": true, "year": true, "size": true, "sel": true, "site": true, "trackers": true, "private": true}
	for k := range acq {
		if !wantKeys[k] {
			t.Errorf("acquire payload carries unexpected key %q", k)
		}
	}
	for _, forbidden := range []string{"source", "url", "magnet", "torrent", "passkey", "metainfo"} {
		if _, has := acq[forbidden]; has {
			t.Errorf("acquire payload carries %q", forbidden)
		}
	}

	del := p.del("movie", "a-film-1982", nil, iat)
	if _, has := decodePayload(t, del)["acq"]; has {
		t.Errorf("a delete op grew an acq key; existing log hashes would change")
	}
}

// TestAcquireRejectsCredentialBearingTrackers: a tracker URL with userinfo, a
// query string, a fragment, no host, or an odd scheme is malformed, at Sign
// and at Verify alike.
func TestAcquireRejectsCredentialBearingTrackers(t *testing.T) {
	p := newPeer(t)
	bad := []string{
		"https://user:pass@tracker.example.org/announce",
		"https://tracker.example.org/announce?passkey=abc",
		"https://tracker.example.org/announce#frag",
		"https:///announce",
		"ftp://tracker.example.org/announce",
		"not a url at all\x00",
	}
	for _, tr := range bad {
		a := publicAcquire()
		a.Trackers = []string{tr}
		if _, err := catalogop.SignAcquire(p.priv, "movie", "a-film-1982", a, nil, iat); !errors.Is(err, catalogop.ErrMalformed) {
			t.Errorf("tracker %q: err = %v, want ErrMalformed", tr, err)
		}
	}
	// A private release publishes no trackers at all.
	a := publicAcquire()
	a.Private = true
	if _, err := catalogop.SignAcquire(p.priv, "movie", "a-film-1982", a, nil, iat); !errors.Is(err, catalogop.ErrMalformed) {
		t.Errorf("private with trackers: err = %v, want ErrMalformed", err)
	}
	a.Trackers = nil
	if _, err := catalogop.SignAcquire(p.priv, "movie", "a-film-1982", a, nil, iat); err != nil {
		t.Errorf("private without trackers: %v", err)
	}
}

// TestAcquireRejectsBadShape: the other shape rules, one each.
func TestAcquireRejectsBadShape(t *testing.T) {
	p := newPeer(t)
	cases := map[string]func(*catalogop.Acquire){
		"upper-case infohash": func(a *catalogop.Acquire) { a.Infohash = strings.ToUpper(hashA) },
		"short infohash":      func(a *catalogop.Acquire) { a.Infohash = hashA[:39] },
		"empty profile":       func(a *catalogop.Acquire) { a.Profile = "" },
		"empty title":         func(a *catalogop.Acquire) { a.Title = "" },
		"empty work title":    func(a *catalogop.Acquire) { a.WorkTitle = "" },
		"absurd year":         func(a *catalogop.Acquire) { a.Year = 10000 },
		"negative size":       func(a *catalogop.Acquire) { a.Size = -1 },
		"zero selected-at":    func(a *catalogop.Acquire) { a.SelectedAt = time.Time{} },
		"empty site":          func(a *catalogop.Acquire) { a.Site = "" },
		"bare scope":          func(a *catalogop.Acquire) { a.Scope = "edition:" },
		"unknown scope":       func(a *catalogop.Acquire) { a.Scope = "season:1" },
		"unknown aspect":      func(a *catalogop.Acquire) { a.Aspect = "trailer" },
		"bad subtitle lang":   func(a *catalogop.Acquire) { a.Aspect = "subtitle:English" },
		"too many trackers": func(a *catalogop.Acquire) {
			a.Trackers = make([]string, catalogop.MaxTrackers+1)
			for i := range a.Trackers {
				a.Trackers[i] = "udp://tracker.example.org/announce"
			}
		},
	}
	for name, mutate := range cases {
		a := publicAcquire()
		mutate(&a)
		if _, err := catalogop.SignAcquire(p.priv, "movie", "a-film-1982", a, nil, iat); !errors.Is(err, catalogop.ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
	// And the shapes that must pass.
	for _, ok := range []func(*catalogop.Acquire){
		func(a *catalogop.Acquire) { a.Scope = catalogop.ScopeEditionPrefix + "bluray-2160p" },
		func(a *catalogop.Acquire) { a.Scope = catalogop.ScopeItemPrefix + "s01e04" },
		func(a *catalogop.Acquire) { a.Aspect = catalogop.AspectSubtitlePrefix + "en" },
		func(a *catalogop.Acquire) { a.Aspect = catalogop.AspectSubtitlePrefix + "pt-BR" },
		func(a *catalogop.Acquire) { a.Infohash = strings.Repeat("ab", 32) },
		func(a *catalogop.Acquire) { a.Size = 0 },
		func(a *catalogop.Acquire) { a.Year = 0 },
		func(a *catalogop.Acquire) { a.Trackers = nil },
	} {
		a := publicAcquire()
		ok(&a)
		if _, err := catalogop.SignAcquire(p.priv, "movie", "a-film-1982", a, nil, iat); err != nil {
			t.Errorf("valid shape %+v refused: %v", a, err)
		}
	}
}

// TestVerifyRejectsKindBodyMismatch: a delete carrying an acquire body, and an
// acquire without one, are both malformed — the kind decides the shape.
func TestVerifyRejectsKindBodyMismatch(t *testing.T) {
	p := newPeer(t)
	tok := p.acquire("movie", "a-film-1982", publicAcquire(), nil, iat)
	body := decodePayload(t, tok)

	body["op"] = "delete"
	if _, err := catalogop.Verify(reencode(t, tok, body)); !errors.Is(err, catalogop.ErrMalformed) && !errors.Is(err, catalogop.ErrBadSignature) {
		t.Errorf("delete with acq: err = %v", err)
	}
	body = decodePayload(t, tok)
	delete(body, "acq")
	if _, err := catalogop.Verify(reencode(t, tok, body)); !errors.Is(err, catalogop.ErrMalformed) && !errors.Is(err, catalogop.ErrBadSignature) {
		t.Errorf("acquire without acq: err = %v", err)
	}
}

// TestNewestSelectionWins: two acquires for one scope keep the later
// selection, whichever order they arrive in; a different scope or aspect is
// its own slot; and a tombstoned work has no effective acquisition.
func TestNewestSelectionWins(t *testing.T) {
	p := newPeer(t)
	tgt := target("movie", "a-film-1982")
	first := publicAcquire()
	later := publicAcquire()
	later.Infohash = hashB
	later.SelectedAt = selAt.Add(time.Hour)
	subs := publicAcquire()
	subs.Aspect = catalogop.AspectSubtitlePrefix + "en"

	t1 := p.acquire("movie", "a-film-1982", first, nil, iat)
	t2 := p.acquire("movie", "a-film-1982", later, nil, iat.Add(time.Hour))
	t3 := p.acquire("movie", "a-film-1982", subs, nil, iat)

	for _, order := range [][]string{{t1, t2, t3}, {t3, t2, t1}, {t2, t1, t3}} {
		v := catalogop.Evaluate(order)
		eff, ok := v.AcquisitionFor(tgt, catalogop.ScopeWork, catalogop.AspectPrimary)
		if !ok || eff.Acquire.Infohash != hashB {
			t.Fatalf("effective primary = %v %+v, want %s", ok, eff.Acquire, hashB)
		}
		sub, ok := v.AcquisitionFor(tgt, catalogop.ScopeWork, catalogop.AspectSubtitlePrefix+"en")
		if !ok || sub.Acquire.Infohash != hashA {
			t.Fatalf("effective subtitle = %v, want %s", ok, hashA)
		}
		if len(v.Acquisitions) != 2 {
			t.Errorf("acquisitions = %d, want 2", len(v.Acquisitions))
		}
	}

	// A tombstone on the work suppresses its acquisitions; a causal restore
	// brings them back.
	del := p.del("movie", "a-film-1982", nil, iat.Add(2*time.Hour))
	v := catalogop.Evaluate([]string{t1, t2, t3, del})
	if len(v.Acquisitions) != 0 {
		t.Errorf("tombstoned work still has %d acquisitions", len(v.Acquisitions))
	}
	res := p.restore("movie", "a-film-1982", v.Heads, iat.Add(3*time.Hour))
	v = catalogop.Evaluate([]string{t1, t2, t3, del, res})
	if len(v.Acquisitions) != 2 {
		t.Errorf("restored work has %d acquisitions, want 2", len(v.Acquisitions))
	}
}

// TestEqualSelectionTieIsDeterministic: equal selected-at falls to the hash,
// so two peers pick the same op whatever the arrival order.
func TestEqualSelectionTieIsDeterministic(t *testing.T) {
	p := newPeer(t)
	a := publicAcquire()
	b := publicAcquire()
	b.Infohash = hashB
	ta := p.acquire("movie", "a-film-1982", a, nil, iat)
	tb := p.acquire("movie", "a-film-1982", b, nil, iat)
	x, _ := catalogop.Evaluate([]string{ta, tb}).AcquisitionFor(target("movie", "a-film-1982"), catalogop.ScopeWork, catalogop.AspectPrimary)
	y, _ := catalogop.Evaluate([]string{tb, ta}).AcquisitionFor(target("movie", "a-film-1982"), catalogop.ScopeWork, catalogop.AspectPrimary)
	if x.Hash != y.Hash {
		t.Errorf("tie broke differently: %s vs %s", x.Hash, y.Hash)
	}
	want := catalogop.Hash(ta)
	if catalogop.Hash(tb) > want {
		want = catalogop.Hash(tb)
	}
	if x.Hash != want {
		t.Errorf("tie picked %s, want the greater hash %s", x.Hash, want)
	}
}

func decodePayload(t *testing.T, tok string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.SplitN(tok, ".", 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// reencode swaps a token's payload for body, keeping its (now wrong)
// signature: Verify must refuse it one way or the other.
func reencode(t *testing.T, tok string, body map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw) + "." + strings.SplitN(tok, ".", 2)[1]
}
