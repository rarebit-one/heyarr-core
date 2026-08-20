package controller

import (
	"os/exec"
	"strings"
	"testing"
)

// §69: Heyarr stores and serves EPUB, PDF, CBZ and CBR. It does not render
// them. Clients render.
//
// # Why this lives here and not next to internal/domain/publication
//
// It shells out to `go list` to walk the real import graph, and depguard
// forbids os/exec inside internal/domain — correctly, and it caught this on
// the first lint run. The rule is about the whole module rather than about one
// package, so it belongs where the other module-wide contract test already
// lives: beside the OpenAPI parity check, which walks the real router for the
// same reason this walks the real import graph.
//
// That is a constraint, and a constraint nobody has watched reject anything is
// decoration. This walks the real import graph of every package in the module
// and fails if a rendering library appears anywhere in it.
//
// It will look like ceremony on the day it is written. That is the point. Its
// first real job is to fail on the pull request, two milestones from now, that
// adds a thumbnail generator because it was three lines — at which point the
// question "should Heyarr rasterise a page?" gets asked deliberately, in
// review, instead of being answered by accident.
//
// # What it does and does not forbid
//
// It forbids libraries whose purpose is to turn a document into pixels or text
// layout: PDF renderers and parsers, EPUB renderers, image decoders, comic
// renderers.
//
// It does NOT forbid archive/zip or encoding/xml. Reading a container's own
// index is not rendering — an EPUB's OPF spine and a CBZ's central directory
// are manifests the container publishes about itself, and reading one is the
// same kind of act as reading an MP4's `moov`. The line is between reading
// what a document says about itself and interpreting its contents, and this
// test is that line, written down.
var forbidden = []struct {
	substring string
	why       string
}{
	{"pdfcpu", "a PDF processor — §69 says clients render, not Heyarr"},
	{"unidoc", "a PDF library"},
	{"ledongthuc/pdf", "a PDF parser"},
	{"pdfium", "a PDF renderer"},
	{"gopdf", "a PDF renderer"},
	{"go-fitz", "a MuPDF binding — rendering"},
	{"mupdf", "a PDF renderer"},
	{"poppler", "a PDF renderer"},
	{"epub", "an EPUB renderer or layout engine"},
	{"mobi", "an ebook format converter"},
	{"image/jpeg", "an image decoder — Heyarr serves comic pages, it does not decode them"},
	{"image/png", "an image decoder"},
	{"image/gif", "an image decoder"},
	{"golang.org/x/image", "image decoding and manipulation"},
	{"disintegration/imaging", "image manipulation"},
	{"nfnt/resize", "thumbnailing"},
	{"h2non/bimg", "image processing"},
	{"chai2010/webp", "an image decoder"},
	{"unarr", "a comic-archive extractor for rendering"},
	{"nwaples/rardecode", "a RAR decoder — see internal/domain/publication on why CBR is not indexed"},
}

func TestNoRenderingLibraryIsReachable(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "go", "list", "-deps", "../...").Output()
	if err != nil {
		t.Fatalf("listing the import graph: %v", err)
	}

	var found []string
	for _, pkg := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pkg = strings.TrimSpace(pkg)
		if pkg == "" || isThisTest(pkg) {
			continue
		}
		for _, f := range forbidden {
			if strings.Contains(strings.ToLower(pkg), f.substring) {
				found = append(found, pkg+" — "+f.why)
			}
		}
	}

	if len(found) > 0 {
		t.Errorf("§69: Heyarr stores and serves publications, it does not render them.\n"+
			"These are reachable from the module's import graph:\n  %s\n\n"+
			"If this is deliberate, it needs an ADR recording the departure — not an\n"+
			"exclusion added here, which would make the rule mean whatever the last\n"+
			"person to hit it wanted it to mean.",
			strings.Join(found, "\n  "))
	}
}

// isThisTest excludes the package under test itself, whose name contains
// "publication" and whose doc comment discusses every library above.
func isThisTest(pkg string) bool {
	return strings.HasSuffix(pkg, "/internal/domain/publication")
}

// The guard is only worth having if it can fail. This asserts the matcher
// itself works, by running it against a package list that contains a violation
// — which is the closest thing to a sabotage that can live in the suite
// permanently rather than being run by hand once.
func TestTheRenderGuardCanFail(t *testing.T) {
	pretend := []string{
		"archive/zip",
		"encoding/xml",
		"github.com/pdfcpu/pdfcpu/pkg/api",
		"github.com/rarebit-one/heyarr-core/internal/domain/publication",
	}
	var found []string
	for _, pkg := range pretend {
		if isThisTest(pkg) {
			continue
		}
		for _, f := range forbidden {
			if strings.Contains(strings.ToLower(pkg), f.substring) {
				found = append(found, pkg)
			}
		}
	}
	if len(found) != 1 || found[0] != "github.com/pdfcpu/pdfcpu/pkg/api" {
		t.Errorf("the guard matched %v; it must catch the PDF library and nothing else — "+
			"archive/zip and encoding/xml are how a container's own index is read", found)
	}
}
