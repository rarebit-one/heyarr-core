package weblogin

import "sync"

// loginWakes counts login wakes still in flight across this test binary. A
// mutex and condition variable, not a sync.WaitGroup: parallel tests start new
// wakes while another test waits, which WaitGroup's reuse rules forbid.
var loginWakes = struct {
	mu   sync.Mutex
	cond *sync.Cond
	n    int
}{}

func init() {
	loginWakes.cond = sync.NewCond(&loginWakes.mu)
	runLoginWake = func(wake func()) {
		loginWakes.mu.Lock()
		loginWakes.n++
		loginWakes.mu.Unlock()
		go func() {
			defer func() {
				loginWakes.mu.Lock()
				loginWakes.n--
				if loginWakes.n == 0 {
					loginWakes.cond.Broadcast()
				}
				loginWakes.mu.Unlock()
			}()
			wake()
		}()
	}
}

// WaitLoginWakes blocks until no login wake is in flight.
func WaitLoginWakes() {
	loginWakes.mu.Lock()
	for loginWakes.n > 0 {
		loginWakes.cond.Wait()
	}
	loginWakes.mu.Unlock()
}
