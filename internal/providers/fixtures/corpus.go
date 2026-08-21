package fixtures

import "path/filepath"

// CorpusRoot is where the committed captures live, relative to the repository
// root.
//
// A constant rather than a scattered string literal because two things need
// it — the loader and the scanner — and they must not be able to disagree
// about where the corpus is. A scanner pointed at the wrong directory reports
// a clean corpus with great confidence.
const CorpusRoot = "internal/providers/fixtures/testdata"

// corpusDir resolves CorpusRoot from within this package's own directory.
func corpusDir() string { return "testdata" }

// ServiceDir is the corpus directory for one service.
func ServiceDir(root, service string) string { return filepath.Join(root, service) }
