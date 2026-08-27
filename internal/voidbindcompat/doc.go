// Package voidbindcompat holds the cross-compatibility tests for the voidbind-go
// migration: the proof that heyarr-core (now a set of shims over voidbind-go) and
// the library itself PRODUCE and VERIFY byte-identical identity artifacts, and
// that artifacts minted by PRE-migration heyarr still verify unchanged.
//
// It has no runtime code — the assertions live in crosscompat_test.go.
package voidbindcompat
