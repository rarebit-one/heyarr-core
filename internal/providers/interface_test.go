package providers

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The interface is defined in VALUES, and this file is the mechanical proof.
//
// #99's acceptance criterion says: "The interface compiles and is fully
// exercised with no HTTP anywhere in the test — if a provider test needs a
// listening socket, the interface is wrong."
//
// A comment claiming that would be true until somebody added `httptest` to make
// one test easier, at which point every downstream fixture strategy would have
// quietly lost its foundation and nothing would go red. So it is asserted
// instead, by reading this package's own source.

// forbidden are the imports that would mean a transport had leaked across the
// provider interface.
//
// net/http is the interesting one. A provider IMPLEMENTATION will import it —
// M3-09's Prowlarr client must — but that lives in its own package behind this
// interface. Nothing in the registry, and nothing testing the registry, has any
// business knowing what a socket is.
var forbidden = []string{
	"net/http",
	"net/http/httptest",
	"net",
	"crypto/tls",
}

func TestNothingInThisPackageKnowsAboutTransport(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	var checked int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Clean(name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		checked++

		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if path != bad {
					continue
				}
				t.Errorf("%s imports %q.\n\n"+
					"The provider interface is defined in VALUES, not in transport: a query "+
					"value in, []acquisition.ReleaseCandidate out. That is what lets a "+
					"recorded fixture, an in-process fake and a real HTTP client be "+
					"indistinguishable to every caller — and for an indexer, which can never "+
					"run in CI (ADR-0026), fixtures are not a convenience but the only test "+
					"path that will ever exist.\n\n"+
					"If this package needs a socket, the interface has leaked and every "+
					"downstream fixture strategy has lost its foundation.", name, path)
			}
		}
	}

	// A guard on the guard. If the directory walk ever silently matched
	// nothing, this test would pass having read no files at all — which is
	// exactly the failure mode of a test that asserts an absence.
	if checked < 5 {
		t.Fatalf("only %d Go files were checked; this test is not reading the package", checked)
	}
	t.Logf("%d files checked for transport imports", checked)
}

// The interface is fully exercised without one. Every capability, routing,
// health, aggregation and failure path in this package is driven by values —
// which is the positive half of the claim the import check makes negatively.
func TestTheWholeInterfaceIsExercisedWithoutASocket(t *testing.T) {
	reg := New(fixedNow)

	indexer := NewFake("an-indexer", CapabilityIndexer).
		Offer("Arrival", candidate("c1", "an-indexer", 2160))
	downloader := NewFake("a-client", CapabilityDownload)
	metadata := NewFake("a-metadata-service", CapabilityMetadata)

	for _, p := range []*Fake{indexer, downloader, metadata} {
		if err := reg.Register(p); err != nil {
			t.Fatal(err)
		}
	}

	// Routing, per capability.
	for _, c := range Capabilities() {
		if !reg.Has(c) {
			t.Errorf("nothing routes for %s", c)
		}
	}
	if len(reg.Indexers()) != 1 || len(reg.Downloaders()) != 1 {
		t.Errorf("typed routing: %d indexers, %d downloaders",
			len(reg.Indexers()), len(reg.Downloaders()))
	}

	// Searching, through the registry's fan-out.
	result, err := reg.Search(t.Context(), Query{Title: "Arrival", ContentType: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("candidates = %v", result.Candidates)
	}
	if indexer.Searches() != 1 {
		t.Errorf("the indexer was asked %d times", indexer.Searches())
	}
	// The downloader was not asked. Routing routed.
	if downloader.Searches() != 0 || metadata.Searches() != 0 {
		t.Error("a search reached a provider that is not an indexer")
	}

	// Transfers, through the downloader interface.
	if _, err := reg.Downloaders()[0].Transfers(t.Context()); err != nil {
		t.Fatal(err)
	}

	// Health, for all three.
	if got := reg.CheckAll(t.Context()); len(got) != 3 {
		t.Fatalf("%d checked", len(got))
	}

	// And what the node therefore advertises.
	caps := reg.JobCapabilities()
	if strings.Join(caps, ",") != "indexer,download,metadata" {
		t.Errorf("JobCapabilities = %v", caps)
	}
}
