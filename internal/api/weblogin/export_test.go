package weblogin

import "sync"

// LoginWakes tracks every login wake started in this test binary, so a test can
// wait for in-flight wakes (pushHarness.settle) now that they run off the
// request path.
var LoginWakes sync.WaitGroup

func init() {
	runLoginWake = func(wake func()) {
		LoginWakes.Add(1)
		go func() {
			defer LoginWakes.Done()
			wake()
		}()
	}
}
