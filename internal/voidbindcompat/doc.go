// Package voidbindcompat holds the cross-compatibility tests for the voidbind-go
// migration: the proof that voidbind-go, which heyarr-core now imports directly
// for its identity primitives, PRODUCES and VERIFIES identity artifacts
// byte-identical to the ones PRE-migration heyarr minted, so those still verify
// unchanged.
//
// The golden vectors are heyarr's history, not heyarr's code: they belong beside
// the implementation, in voidbind-go, and this package retires once they move.
//
// It has no runtime code — the assertions live in crosscompat_test.go.
package voidbindcompat
