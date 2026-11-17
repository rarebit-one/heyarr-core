package catalogop_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
)

func (p *peer) unsourced(ct, wk, acquire string, prev []string) string {
	p.t.Helper()
	tok, err := catalogop.SignUnsourced(p.priv, ct, wk, catalogop.Unsourced{Acquire: acquire, Site: "south"}, prev, iat)
	if err != nil {
		p.t.Fatalf("sign unsourced %s/%s: %v", ct, wk, err)
	}
	return tok
}

// TestUnsourcedRoundTrip: a report parses back to its body, cites the acquire
// it names, and the view lists it against that acquire.
func TestUnsourcedRoundTrip(t *testing.T) {
	origin, sibling := newPeer(t), newPeer(t)
	acq := origin.acquire("movie", "a-film-1982", publicAcquire(), nil, iat)
	tok := sibling.unsourced("movie", "a-film-1982", catalogop.Hash(acq), nil)

	op, err := catalogop.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if op.Kind != catalogop.OpUnsourced || op.Unsourced == nil || op.Acquire != nil || op.Hold != nil {
		t.Fatalf("kind = %q, unsourced = %v", op.Kind, op.Unsourced)
	}
	if got := *op.Unsourced; got != (catalogop.Unsourced{Acquire: catalogop.Hash(acq), Site: "south"}) {
		t.Fatalf("unsourced = %+v", got)
	}
	if !slices.Contains(op.Prev, catalogop.Hash(acq)) {
		t.Fatalf("prev = %v, want it to cite the acquire", op.Prev)
	}
	v := catalogop.Evaluate([]string{acq, tok})
	if got := v.Unsourced[catalogop.Hash(acq)]; len(got) != 1 || got[0].Hash != op.Hash {
		t.Fatalf("reports for the acquire = %v, want the one op", got)
	}
	if len(v.Heads) != 1 || v.Heads[0] != op.Hash {
		t.Fatalf("heads = %v, want only the report", v.Heads)
	}
	if len(v.Tombstoned) != 0 || len(v.Holds) != 0 || len(v.Acquisitions) != 1 {
		t.Fatal("a report changed another view")
	}
}

// Before the acquire has arrived the report cannot be judged: it cites a past
// not seen, so it is bad_prev, and it counts once the acquire is in the set.
func TestAReportBeforeItsAcquireIsHeld(t *testing.T) {
	origin, sibling := newPeer(t), newPeer(t)
	acq := origin.acquire("movie", "a-film-1982", publicAcquire(), nil, iat)
	tok := sibling.unsourced("movie", "a-film-1982", catalogop.Hash(acq), nil)

	v := catalogop.Evaluate([]string{tok})
	if v.Rejected[catalogop.Hash(tok)] != catalogop.ReasonBadPrev || len(v.Unsourced) != 0 {
		t.Fatalf("a report without its acquire: rejected = %v, unsourced = %v", v.Rejected, v.Unsourced)
	}
	if v := catalogop.Evaluate([]string{tok, acq}); len(v.Unsourced[catalogop.Hash(acq)]) != 1 {
		t.Fatal("the report does not count once its acquire arrives")
	}
}

// A report counts only for an acquire on the same work by another signer, on
// a work that is not tombstoned, and once per reporter.
func TestAReportCountsOnlyForAnotherSitesAcquire(t *testing.T) {
	origin, sibling := newPeer(t), newPeer(t)
	acq := origin.acquire("movie", "a-film-1982", publicAcquire(), nil, iat)
	h := catalogop.Hash(acq)
	del := origin.del("movie", "x", nil, iat)
	cases := map[string][]string{
		"its own acquire":   {acq, origin.unsourced("movie", "a-film-1982", h, nil)},
		"another work":      {acq, sibling.unsourced("movie", "another-film-1990", h, nil)},
		"not an acquire":    {acq, del, sibling.unsourced("movie", "x", catalogop.Hash(del), nil)},
		"a tombstoned work": {acq, sibling.unsourced("movie", "a-film-1982", h, nil), origin.del("movie", "a-film-1982", nil, iat)},
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			if v := catalogop.Evaluate(set); len(v.Unsourced) != 0 {
				t.Fatalf("unsourced = %v, want no report to count", v.Unsourced)
			}
		})
	}

	twice := catalogop.Evaluate([]string{
		acq,
		sibling.unsourced("movie", "a-film-1982", h, nil),
		sibling.unsourced("movie", "a-film-1982", h, []string{h, catalogop.Hash(sibling.del("movie", "y", nil, iat))}),
		sibling.del("movie", "y", nil, iat),
	})
	if got := len(twice.Unsourced[h]); got != 1 {
		t.Fatalf("two reports by one site count %d times, want once", got)
	}
}

// SignUnsourced refuses what Verify would.
func TestUnsourcedShapeIsRefused(t *testing.T) {
	p := newPeer(t)
	good := catalogop.Hash("x")
	cases := map[string]catalogop.Unsourced{
		"no acquire":        {Site: "south"},
		"not a hash":        {Acquire: "blake3:" + strings.Repeat("a", 64), Site: "south"},
		"upper-case hash":   {Acquire: catalogop.HashPrefix + strings.Repeat("A", 64), Site: "south"},
		"short hash":        {Acquire: catalogop.HashPrefix + strings.Repeat("a", 63), Site: "south"},
		"no site":           {Acquire: good},
		"an over-long site": {Acquire: good, Site: strings.Repeat("s", catalogop.MaxSite+1)},
	}
	for name, u := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := catalogop.SignUnsourced(p.priv, "movie", "a-film-1982", u, nil, iat); !errors.Is(err, catalogop.ErrMalformed) {
				t.Fatalf("err = %v, want ErrMalformed", err)
			}
		})
	}
}

// A report that does not cite the acquire it names, or whose kind and body
// disagree, does not verify.
func TestUnsourcedMustCiteItsAcquireAndMatchItsKind(t *testing.T) {
	origin, sibling := newPeer(t), newPeer(t)
	acq := catalogop.Hash(origin.acquire("movie", "a-film-1982", publicAcquire(), nil, iat))
	tok := sibling.unsourced("movie", "a-film-1982", acq, nil)
	body, _ := splitToken(t, tok)

	uncited := strings.Replace(string(body), `"prev":["`+acq+`"]`, `"prev":[]`, 1)
	if uncited == string(body) {
		t.Fatalf("payload %s does not carry the expected prev", body)
	}
	if _, err := catalogop.Verify(resign(t, sibling, []byte(uncited))); !errors.Is(err, catalogop.ErrMalformed) {
		t.Fatalf("a report not citing its acquire: err = %v, want ErrMalformed", err)
	}
	for _, kind := range []string{`"op":"delete"`, `"op":"acquire"`, `"op":"hold"`} {
		forged := strings.Replace(string(body), `"op":"unsourced"`, kind, 1)
		if _, err := catalogop.Verify(resign(t, sibling, []byte(forged))); !errors.Is(err, catalogop.ErrMalformed) {
			t.Fatalf("%s with an unsourced body: err = %v, want ErrMalformed", kind, err)
		}
	}
}

// Adding the kind changes no existing op's bytes.
func TestAReportLeavesOtherKindsBytesAlone(t *testing.T) {
	p := newPeer(t)
	for _, tok := range []string{
		p.sign(catalogop.OpDelete, "movie", "a-film-1982", nil, iat),
		p.acquire("movie", "a-film-1982", publicAcquire(), nil, iat),
		p.hold("rom", "rom:snes:x", romHold(), nil, iat),
	} {
		body, _ := splitToken(t, tok)
		if strings.Contains(string(body), `"uns"`) {
			t.Fatalf("payload %s carries an unsourced key", body)
		}
	}
}
