package problem_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

// The wire shape is the contract. These golden files are what a client parses,
// so a field renamed in a refactor has to show up as a diff here rather than as
// a broken integration six weeks later.
func TestProblemWireShapes(t *testing.T) {
	tests := []struct {
		name    string
		golden  string
		problem *problem.Problem
		status  int
	}{
		{"unauthorized", "unauthorized.json", problem.Unauthorized("the presented credential was rejected"), 401},
		{"forbidden", "forbidden.json", problem.Forbidden("this token does not carry the write scope"), 403},
		{"not_found", "not_found.json", problem.NotFound("no route matches /api/v1/nope"), 404},
		{"bad_request", "bad_request.json", problem.BadRequest("year must be a number"), 400},
		{"conflict", "conflict.json", problem.Conflict("that library name is already taken"), 409},
		{"internal", "internal.json", problem.Internal(), 500},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/api/v1/example", nil)
			problem.Write(rec, r, tt.problem.WithRequestID("01920000-0000-7000-8000-000000000000"))

			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d", rec.Code, tt.status)
			}
			if got := rec.Header().Get("Content-Type"); got != problem.MediaType {
				t.Errorf("Content-Type = %q, want %q", got, problem.MediaType)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store — a cached 401 is a sticky 401", got)
			}

			var pretty any
			if err := json.Unmarshal(rec.Body.Bytes(), &pretty); err != nil {
				t.Fatalf("the body is not JSON: %v", err)
			}
			out, err := json.MarshalIndent(pretty, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			testutil.Golden(t, "testdata/"+tt.golden, append(out, '\n'))
		})
	}
}

func TestTypeURIsAreStableAndDistinct(t *testing.T) {
	// Two problems sharing a type URI would make them indistinguishable to a
	// client branching on it, which is the entire point of the field.
	seen := map[string]string{}
	for name, uri := range map[string]string{
		"bad request":  problem.TypeBadRequest,
		"unauthorized": problem.TypeUnauthorized,
		"forbidden":    problem.TypeForbidden,
		"not found":    problem.TypeNotFound,
		"conflict":     problem.TypeConflict,
		"internal":     problem.TypeInternal,
		// A capability nothing is configured to answer (#451): the request was
		// fine and nothing broke, there is just no provider — a different action
		// (configure one) from a 400 or a 500.
		"service unavailable": problem.TypeServiceUnavailable,
		// M5-05's 404 that is not "no such thing": the node holds the blob and
		// has no chunk manifest for it. A destination takes a DIFFERENT action
		// on each, so they must not collapse.
		"no chunk manifest": problem.TypeNoChunkManifest,
	} {
		if prev, dup := seen[uri]; dup {
			t.Errorf("%q and %q share the type URI %s", name, prev, uri)
		}
		seen[uri] = name
		if !strings.HasPrefix(uri, problem.TypeBase) {
			t.Errorf("%s does not start with %s", uri, problem.TypeBase)
		}
	}

	// Distinct is not enough: no URI may CONTAIN another. A caller doing a
	// substring check on one would match both, which is the assert_contains
	// failure mode this repository has already shipped once — and here it
	// would silently merge "there is no manifest, pull whole from this source"
	// with "there is no such blob, try another source".
	for a := range seen {
		for b := range seen {
			if a != b && strings.Contains(a, b) {
				t.Errorf("the type URI %s contains %s, so a `contains` check on the shorter one "+
					"matches both", a, b)
			}
		}
	}
}

func TestInternalNeverCarriesTheUnderlyingFailure(t *testing.T) {
	// A 500 is where implementation detail leaks. The constructor takes no
	// argument on purpose; this asserts nobody has quietly added one back.
	p := problem.Internal()
	for _, forbidden := range []string{"sql", "/var/lib", "goroutine", "panic"} {
		if strings.Contains(strings.ToLower(p.Detail), forbidden) {
			t.Errorf("the internal problem detail mentions %q: %q", forbidden, p.Detail)
		}
	}
}

func TestWriteFillsInstanceFromTheRequest(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/works/17", nil)
	problem.Write(rec, r, problem.NotFound("no such work"))

	var got problem.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Instance != "/api/v1/works/17" {
		t.Errorf("instance = %q, want the request path", got.Instance)
	}
}

func TestWriteHandlesANilProblem(t *testing.T) {
	// A handler that reaches an error path with nothing to say must still
	// produce a valid document rather than a nil dereference.
	rec := httptest.NewRecorder()
	problem.Write(rec, httptest.NewRequest(http.MethodGet, "/x", nil), nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestProblemIsAnError(t *testing.T) {
	if got := problem.NotFound("no such work").Error(); got != "Not Found: no such work" {
		t.Errorf("Error() = %q", got)
	}
	if got := problem.New(418, "urn:x", "Teapot", "").Error(); got != "Teapot" {
		t.Errorf("Error() = %q", got)
	}
}
