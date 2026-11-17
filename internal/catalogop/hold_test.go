package catalogop_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
)

var heldAt = time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)

const blobA = "blake3:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func romHold() catalogop.Hold {
	return catalogop.Hold{
		Scope:     catalogop.ScopeEditionPrefix + "usa",
		Blob:      blobA,
		Size:      524_288,
		Path:      "snes/Super Mario World (USA).sfc",
		Role:      "primary",
		WorkTitle: "Super Mario World",
		Library:   "roms",
		Site:      "north",
		HeldAt:    heldAt,
	}
}

func (p *peer) hold(ct, wk string, h catalogop.Hold, prev []string, at time.Time) string {
	p.t.Helper()
	tok, err := catalogop.SignHold(p.priv, ct, wk, h, prev, at)
	if err != nil {
		p.t.Fatalf("sign hold %s/%s: %v", ct, wk, err)
	}
	return tok
}

// TestHoldRoundTrip: a hold op parses back to the body it carried, and the
// view names it as the effective hold for its (work, blob).
func TestHoldRoundTrip(t *testing.T) {
	p := newPeer(t)
	want := romHold()
	tok := p.hold("rom", "rom:snes:super mario world", want, nil, iat)
	op, err := catalogop.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if op.Kind != catalogop.OpHold || op.Hold == nil || op.Acquire != nil {
		t.Fatalf("kind = %q, hold = %v, acquire = %v", op.Kind, op.Hold, op.Acquire)
	}
	if got := *op.Hold; got != want {
		t.Fatalf("hold = %+v, want %+v", got, want)
	}
	v := catalogop.Evaluate([]string{tok})
	target := catalogop.Target{ContentType: "rom", WorkKey: "rom:snes:super mario world"}
	if got, ok := v.Holds[catalogop.HoldKey(target, blobA)]; !ok || got.Hash != op.Hash {
		t.Fatalf("effective hold = %v/%v, want %s", got.Hash, ok, op.Hash)
	}
	if len(v.Tombstoned) != 0 || len(v.Acquisitions) != 0 {
		t.Fatalf("a hold changed another view: %d tombstones, %d acquisitions", len(v.Tombstoned), len(v.Acquisitions))
	}
}

// TestHoldShapeIsRefused: SignHold refuses what Verify would, so a sibling
// never sees a hold its own build would reject.
func TestHoldShapeIsRefused(t *testing.T) {
	p := newPeer(t)
	cases := map[string]func(h *catalogop.Hold){
		"work scope":         func(h *catalogop.Hold) { h.Scope = catalogop.ScopeWork },
		"empty edition":      func(h *catalogop.Hold) { h.Scope = catalogop.ScopeEditionPrefix },
		"sha256 digest":      func(h *catalogop.Hold) { h.Blob = "sha256:" + strings.Repeat("a", 64) },
		"upper-case digest":  func(h *catalogop.Hold) { h.Blob = "blake3:" + strings.Repeat("A", 64) },
		"negative size":      func(h *catalogop.Hold) { h.Size = -1 },
		"absolute path":      func(h *catalogop.Hold) { h.Path = "/srv/media/roms/snes/a.sfc" },
		"climbing path":      func(h *catalogop.Hold) { h.Path = "snes/../../etc/passwd" },
		"dot segment":        func(h *catalogop.Hold) { h.Path = "./snes/a.sfc" },
		"empty segment":      func(h *catalogop.Hold) { h.Path = "snes//a.sfc" },
		"backslash":          func(h *catalogop.Hold) { h.Path = `snes\a.sfc` },
		"empty path":         func(h *catalogop.Hold) { h.Path = "" },
		"overlong path":      func(h *catalogop.Hold) { h.Path = strings.Repeat("a", catalogop.MaxPath+1) },
		"no role":            func(h *catalogop.Hold) { h.Role = "" },
		"no work title":      func(h *catalogop.Hold) { h.WorkTitle = "" },
		"year out of range":  func(h *catalogop.Hold) { h.Year = 10000 },
		"no library":         func(h *catalogop.Hold) { h.Library = "" },
		"overlong library":   func(h *catalogop.Hold) { h.Library = strings.Repeat("l", catalogop.MaxLibrary+1) },
		"no site":            func(h *catalogop.Hold) { h.Site = "" },
		"no held-at":         func(h *catalogop.Hold) { h.HeldAt = time.Time{} },
		"invalid utf-8 path": func(h *catalogop.Hold) { h.Path = "snes/\xff.sfc" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := romHold()
			mutate(&h)
			_, err := catalogop.SignHold(p.priv, "rom", "rom:snes:x", h, nil, iat)
			if !errors.Is(err, catalogop.ErrMalformed) {
				t.Fatalf("err = %v, want ErrMalformed", err)
			}
		})
	}
}

// A hold token whose kind and body disagree does not verify: a delete with a
// hold body, or a hold relabelled as an acquire, is not either.
func TestHoldBodyMustMatchItsKind(t *testing.T) {
	p := newPeer(t)
	tok := p.hold("rom", "rom:snes:x", romHold(), nil, iat)
	body, _ := splitToken(t, tok)
	for _, kind := range []string{`"op":"delete"`, `"op":"acquire"`} {
		forged := strings.Replace(string(body), `"op":"hold"`, kind, 1)
		if _, err := catalogop.Verify(resign(t, p, []byte(forged))); !errors.Is(err, catalogop.ErrMalformed) {
			t.Fatalf("%s with a hold body: err = %v, want ErrMalformed", kind, err)
		}
	}
}

// Newest by issued-at wins per (work, blob), ties fall to the hash; a
// different blob of the same work is its own hold.
func TestNewestHoldWinsPerBlob(t *testing.T) {
	p := newPeer(t)
	target := catalogop.Target{ContentType: "rom", WorkKey: "rom:snes:x"}
	old := p.hold(target.ContentType, target.WorkKey, romHold(), nil, iat)
	moved := romHold()
	moved.Path = "snes/Moved/Super Mario World (USA).sfc"
	newer := p.hold(target.ContentType, target.WorkKey, moved, nil, iat.Add(time.Hour))
	other := romHold()
	other.Blob = "blake3:" + strings.Repeat("e", 64)
	second := p.hold(target.ContentType, target.WorkKey, other, nil, iat)

	v := catalogop.Evaluate([]string{newer, second, old})
	if len(v.Holds) != 2 {
		t.Fatalf("holds = %d, want one per blob", len(v.Holds))
	}
	if got := v.Holds[catalogop.HoldKey(target, blobA)]; got.Hash != catalogop.Hash(newer) {
		t.Fatalf("effective hold for blob A is not the newer op")
	}
}

// A tombstone on the work suppresses its holds, as it does acquisitions.
func TestATombstoneSuppressesAHold(t *testing.T) {
	p := newPeer(t)
	h := p.hold("rom", "rom:snes:x", romHold(), nil, iat)
	del := p.sign(catalogop.OpDelete, "rom", "rom:snes:x", nil, iat)
	v := catalogop.Evaluate([]string{h, del})
	if len(v.Holds) != 0 {
		t.Fatalf("a tombstoned work still has %d holds", len(v.Holds))
	}
}

// Adding the kind changes no existing op's bytes: a delete signs to the same
// payload it did before, with no hold key in it, so every op already in a log
// keeps its hash.
func TestAHoldLeavesOtherKindsBytesAlone(t *testing.T) {
	p := newPeer(t)
	for _, tok := range []string{
		p.sign(catalogop.OpDelete, "movie", "a-film-1982", nil, iat),
		p.acquire("movie", "a-film-1982", publicAcquire(), nil, iat),
	} {
		body, _ := splitToken(t, tok)
		if strings.Contains(string(body), `"hold"`) {
			t.Fatalf("payload %s carries a hold key", body)
		}
	}
}

func splitToken(t *testing.T, tok string) ([]byte, []byte) {
	t.Helper()
	dot := strings.IndexByte(tok, '.')
	body, err := base64.RawURLEncoding.DecodeString(tok[:dot])
	if err != nil {
		t.Fatal(err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(tok[dot+1:])
	if err != nil {
		t.Fatal(err)
	}
	return body, sig
}

// resign signs a payload as p, so a forged body fails on its shape rather than
// on its signature.
func resign(t *testing.T, p *peer, body []byte) string {
	t.Helper()
	return base64.RawURLEncoding.EncodeToString(body) + "." +
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(p.priv, body))
}
