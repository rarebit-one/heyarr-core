package worker

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/providers/httpjson"
)

// A poll that fails for a reason of the moment retries on the long backoff;
// one whose source is wrong fails once; anything else keeps the attempt cap
// (#670). Each row is the error as the poll actually receives it.
func TestAPollFailureIsClassifiedByWhatItSays(t *testing.T) {
	// Strict decoding, as the podcast and YouTube adapters do (the web feed
	// parser is lenient and accepts this): the class #557 saw dying at five
	// attempts.
	var feed struct{}
	malformed := xml.Unmarshal([]byte(`<feed><entry><title>a &#xZZ; b</title></entry></feed>`), &feed)
	if malformed == nil {
		t.Fatal("the malformed feed parsed; the row below would test nothing")
	}
	malformed = fmt.Errorf("youtube: parsing the feed: %w", malformed)
	refused := fmt.Errorf("youtube: fetching the feed: %w", &net.OpError{
		Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED),
	})
	feedStatus := func(code int) error { return &httpjson.Error{Service: "podcast", Op: "the feed", Status: code} }

	const (
		transient = "transient"
		permanent = "permanent"
		capped    = "attempt cap"
	)
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"HTTP 408", feedStatus(408), transient},
		{"HTTP 429", feedStatus(429), transient},
		{"HTTP 500", feedStatus(500), transient},
		{"HTTP 503", feedStatus(503), transient},
		{"a timeout", fmt.Errorf("webfeed: fetching the feed: %w", context.DeadlineExceeded), transient},
		{"a refused connection", refused, transient},
		{"a name that does not resolve", &net.DNSError{Err: "no such host", Name: "feed.invalid", IsNotFound: true}, transient},
		{"HTTP 404", feedStatus(404), permanent},
		{"HTTP 410", feedStatus(410), permanent},
		{"HTTP 401", feedStatus(401), permanent},
		{"HTTP 403", feedStatus(403), permanent},
		{"malformed feed XML", malformed, permanent},
		{"HTTP 406 needs a look, so it keeps the default", feedStatus(406), capped},
		{"an error nothing recognises", errors.New("tvdb: the series has no episodes"), capped},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyPollFailure(fmt.Errorf("worker: enumerating source s1: %w", tc.err))
			class := capped
			switch {
			case errors.Is(got, jobs.ErrPermanent):
				class = permanent
			case errors.Is(got, jobs.ErrTransient):
				class = transient
			}
			if class != tc.want {
				t.Errorf("classified %s, want %s: %v", class, tc.want, got)
			}
			if !errors.Is(got, tc.err) {
				t.Error("classifying lost the cause, so last_error would no longer say why")
			}
		})
	}
}
