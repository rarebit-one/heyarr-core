package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/problem"
)

func TestParseAddr(t *testing.T) {
	tests := []struct {
		name       string
		addr       string
		unixSocket string
		wantScheme string
		wantAddr   string
		wantErr    bool
	}{
		{
			name:       "the default is the unix socket",
			unixSocket: "/var/lib/heyarr/heyarr.sock",
			wantScheme: "unix", wantAddr: "/var/lib/heyarr/heyarr.sock",
		},
		{
			name: "no socket and no address is an error worth explaining",
			// The failure mode this avoids is a client that dials "" and
			// reports "dial unix: missing address".
			wantErr: true,
		},
		{
			name: "an explicit unix url", addr: "unix:///run/heyarr.sock",
			wantScheme: "unix", wantAddr: "/run/heyarr.sock",
		},
		{
			name: "an absolute path is a socket", addr: "/run/heyarr.sock",
			wantScheme: "unix", wantAddr: "/run/heyarr.sock",
		},
		{
			name: "an http url", addr: "http://127.0.0.1:7777",
			wantScheme: "tcp", wantAddr: "127.0.0.1:7777",
		},
		{
			name: "an https url", addr: "https://heyarr.example:8443",
			wantScheme: "tcp", wantAddr: "heyarr.example:8443",
		},
		{
			name: "a bare host:port", addr: "127.0.0.1:7777",
			wantScheme: "tcp", wantAddr: "127.0.0.1:7777",
		},
		{
			name: "a scheme this client cannot speak is refused rather than guessed at",
			addr: "ftp://host/path", wantErr: true,
		},
		{name: "a bare hostname with no port", addr: "heyarr.example", wantErr: true},
		{name: "an empty unix url", addr: "unix://", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAddr(tt.addr, tt.unixSocket)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseAddr(%q, %q) = %+v, want an error", tt.addr, tt.unixSocket, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.scheme != tt.wantScheme || got.address != tt.wantAddr {
				t.Errorf("ParseAddr(%q, %q) = %s %s, want %s %s",
					tt.addr, tt.unixSocket, got.scheme, got.address, tt.wantScheme, tt.wantAddr)
			}
		})
	}
}

// A client that dials a unix socket must actually dial the socket rather than
// resolving the fabricated host in the URL. The socket is the default
// transport, so this is the path almost every real invocation takes.
func TestAUnixClientTalksToTheSocket(t *testing.T) {
	// Not t.TempDir(): a socket path has a hard length limit that is a fixed
	// array in a C struct (104 bytes on darwin), and the test framework's
	// temporary paths are long enough to exceed it.
	dir, err := os.MkdirTemp("", "hy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix sockets are not usable here: %v", err)
	}

	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != APIPrefix+"/system" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, `{"auth_enabled":false}`)
		}),
	}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })

	c, err := New(Options{UnixSocket: socket})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		AuthEnabled bool `json:"auth_enabled"`
	}
	if err := c.Get(context.Background(), "/system", nil, &out); err != nil {
		t.Fatalf("GET /system over the socket: %v", err)
	}
	if c.Target() != "unix://"+socket {
		t.Errorf("Target() = %q", c.Target())
	}
}

// An error is what the server said, not the status it said it with.
func TestAnErrorRendersTheProblemDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		problem.Write(w, r, problem.NotFound("no work with that identifier"))
	}))
	t.Cleanup(srv.Close)

	c, err := New(Options{Addr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Get(context.Background(), "/works/nope", nil, &struct{}{})
	if err == nil {
		t.Fatal("a 404 was reported as success")
	}
	if err.Error() != "no work with that identifier" {
		t.Errorf("error = %q, want the server's own detail", err)
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is %T, want *client.Error", err)
	}
	if apiErr.Type() != problem.TypeNotFound {
		t.Errorf("Type() = %q, want the stable type URI clients branch on", apiErr.Type())
	}
	if !IsNotFound(err) {
		t.Error("IsNotFound did not recognise a 404")
	}
}

// Something in front of Heyarr answering instead must still produce a sentence
// rather than a decoding failure.
func TestANonProblemErrorBodyStillReadsAsASentence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>502 Bad Gateway</html>")
	}))
	t.Cleanup(srv.Close)

	c, err := New(Options{Addr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Get(context.Background(), "/works", nil, &struct{}{})
	if err == nil {
		t.Fatal("a 502 was reported as success")
	}
	for _, want := range []string{"502", "Bad Gateway"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// A transport failure must name what could not be reached. "connection
// refused" on its own does not say what was not listening.
func TestAnUnreachableAPISaysWhatItCouldNotReach(t *testing.T) {
	c, err := New(Options{UnixSocket: filepath.Join(t.TempDir(), "absent.sock")})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Get(context.Background(), "/system", nil, &struct{}{})
	if err == nil {
		t.Fatal("a call to a socket that does not exist succeeded")
	}
	if !strings.Contains(err.Error(), "absent.sock") || !strings.Contains(err.Error(), "is heyarr running") {
		t.Errorf("error = %q, want it to name the socket and suggest the obvious cause", err)
	}
}

// ---------------------------------------------------------------------------
// The event stream
// ---------------------------------------------------------------------------

// sseServer replays a canned stream, so the frame parsing can be driven
// through cases a real server produces rarely — a gap notice, a heartbeat, a
// multi-line payload.
func sseServer(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(Options{Addr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTheStreamIsReadyBeforeOpenReturns(t *testing.T) {
	c := sseServer(t, "retry: 3000\n\n: ready\n\n"+
		`id: 7`+"\n"+`event: blob.created`+"\n"+
		`data: {"seq":7,"id":"e1","type":"blob.created","created_at":"2026-08-20T12:00:00Z"}`+"\n\n"+
		": heartbeat\n\n")

	stream, err := c.Events(context.Background(), 0, nil)
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	msg, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if msg.Event == nil || msg.Event.Seq != 7 || msg.Event.Type != "blob.created" {
		t.Fatalf("first frame = %+v, want the blob.created event", msg)
	}
	if stream.LastSeq() != 7 {
		t.Errorf("LastSeq() = %d, want 7 — a reconnect would repeat or skip", stream.LastSeq())
	}

	// The heartbeat is a comment and must not surface as a frame.
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("after the last event the stream returned %v, want io.EOF", err)
	}
}

// A dropped-events notice is delivered, never swallowed. A client that ignores
// it carries on with a hole in its view believing it is current.
func TestAGapNoticeIsDeliveredToTheCaller(t *testing.T) {
	c := sseServer(t, ": ready\n\n"+
		"event: "+StreamGapType+"\n"+
		`data: {"resume_after":41,"dropped":12,"detail":"this connection fell behind"}`+"\n\n")

	stream, err := c.Events(context.Background(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()

	msg, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if msg.Gap == nil {
		t.Fatalf("frame = %+v, want a gap notice", msg)
	}
	if msg.Gap.Dropped != 12 || msg.Gap.ResumeAfter != 41 {
		t.Errorf("gap = %+v, want 12 dropped and a resume point of 41", msg.Gap)
	}
	if stream.LastSeq() != 41 {
		t.Errorf("LastSeq() = %d, want the gap's resume point so a reconnect is gapless", stream.LastSeq())
	}
}

// Not an event stream at all means something else answered, and pretending
// otherwise produces an infinite loop over HTML.
func TestANonStreamResponseIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(srv.Close)
	c, err := New(Options{Addr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Events(context.Background(), 0, nil); err == nil {
		t.Fatal("a JSON response was accepted as an event stream")
	}
}

// ---------------------------------------------------------------------------
// Pagination
// ---------------------------------------------------------------------------

// pagedServer serves n rows in pages of size, with an opaque cursor.
func pagedServer(t *testing.T, n int) (*Client, *int) {
	t.Helper()
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		limit := 2
		start := 0
		if c := r.URL.Query().Get("cursor"); c != "" {
			// The cursor is opaque to the client; this is this server's own
			// encoding of it, and the client must echo it back untouched.
			parsed, err := strconv.Atoi(c)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			start = parsed
		}
		type row struct {
			ID int `json:"id"`
		}
		page := Page[row]{Items: []row{}}
		for i := start; i < n && len(page.Items) < limit; i++ {
			page.Items = append(page.Items, row{ID: i})
		}
		if start+len(page.Items) < n {
			page.NextCursor = strconv.Itoa(start + len(page.Items))
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Options{Addr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c, &requests
}

// The property: every row, exactly once, across several pages.
func TestListFollowsCursorsToExhaustion(t *testing.T) {
	c, requests := pagedServer(t, 5)
	type row struct {
		ID int `json:"id"`
	}
	got, err := List[row](context.Background(), c, "/rows", ListOptions{PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("List returned %d rows, want 5 — it stopped at a page boundary", len(got))
	}
	seen := map[int]bool{}
	for _, r := range got {
		if seen[r.ID] {
			t.Errorf("row %d was returned twice", r.ID)
		}
		seen[r.ID] = true
	}
	if *requests < 3 {
		t.Errorf("the walk made %d requests for 5 rows in pages of 2 — it cannot have followed the cursors",
			*requests)
	}
}

// A server that repeats a cursor must stop the walk rather than spin it
// forever accumulating rows.
func TestListRefusesToLoopOnARepeatedCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"items":[{"id":1}],"next_cursor":"always-the-same"}`)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Options{Addr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		ID int `json:"id"`
	}
	if _, err := List[row](context.Background(), c, "/rows", ListOptions{PageSize: 1}); err == nil {
		t.Fatal("a repeated cursor was followed forever")
	}
}

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

// The client's types are declared independently of the server's, so that a
// field renamed on one side is a failing test rather than a change both sides
// agree about and every other consumer of the API does not.
//
// It decodes the API package's own golden files, with unknown fields rejected:
// a field added to the server and not here fails, and so does a field renamed.
func TestWireTypesMatchTheServer(t *testing.T) {
	tests := []struct {
		golden string
		into   func() any
	}{
		{"works_list.json", func() any { return &Page[Work]{} }},
		{"work.json", func() any { return &Work{} }},
		{"edition.json", func() any { return &Edition{} }},
		{"assets_list.json", func() any { return &Page[Asset]{} }},
		{"asset_linked.json", func() any { return &Asset{} }},
		{"blob.json", func() any { return &Blob{} }},
		{"libraries_list.json", func() any { return &Page[Library]{} }},
		{"library.json", func() any { return &Library{} }},
		{"library_created.json", func() any { return &Library{} }},
		{"root_created.json", func() any { return &LibraryRoot{} }},
		{"peers_list.json", func() any { return &Page[Peer]{} }},
		{"replicas_list.json", func() any { return &Page[Replica]{} }},
		{"jobs_list.json", func() any { return &Page[Job]{} }},
		{"job_dead.json", func() any { return &Job{} }},
		{"scan_accepted.json", func() any { return &ScanResponse{} }},
		{"desired_list.json", func() any { return &Page[DesiredItem]{} }},
		{"desired.json", func() any { return &DesiredItem{} }},
		{"quality_profiles_list.json", func() any { return &Page[QualityProfile]{} }},
		{"quality_profile.json", func() any { return &QualityProfile{} }},
	}

	for _, tt := range tests {
		t.Run(tt.golden, func(t *testing.T) {
			path := filepath.Join("..", "api", "resources", "testdata", tt.golden)
			raw, err := os.ReadFile(filepath.Clean(path))
			if err != nil {
				t.Fatalf("reading the API's golden file: %v", err)
			}
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(tt.into()); err != nil {
				t.Fatalf("the client's type no longer matches what the API returns: %v\n%s", err, raw)
			}
		})
	}
}
