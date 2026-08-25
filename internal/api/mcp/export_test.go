package mcp

// DeferredVerbs is the deferral table, for this package's external tests.
//
// # Why this file exists
//
// boundary_test.go is in package mcp_test and therefore cannot see
// deferredTools, so it kept its own hand-written copy of the same names. That
// copy is what #226 is about, one layer down: two lists of what is deferred,
// nothing keeping them in step, and a test comparing the code against a stale
// duplicate of itself. When two verbs shipped, the production table lost them
// and the copy did not.
//
// An export_test.go bridge is the standard answer and costs nothing at runtime:
// it is compiled only under test, so nothing outside the tests can reach the
// table and no production surface grows to serve one.
func DeferredVerbs() map[string]bool {
	out := make(map[string]bool, len(deferredTools))
	for name := range deferredTools {
		out[name] = true
	}
	return out
}
