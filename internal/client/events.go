package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// GET /api/v1/events is Server-Sent Events, and the property that matters is
// gapless reconnection: a client that saw seq N reconnects with ?after=N and
// receives everything since, with no hole and no duplicate.
//
// Two things in this file are load-bearing rather than convenient:
//
//   - Open does not return until the server's `: ready` comment has arrived.
//     That comment is written after the subscription exists and before the
//     catch-up read, so a caller that has an open EventStream knows it is
//     subscribed. Everything that needs "subscribe before you look" — `--wait`
//     above all — depends on being able to know that without sleeping.
//   - A gap notice is delivered to the caller, never swallowed. The server
//     sends one when it dropped events for a slow consumer, and a client that
//     ignores it carries on with a hole in its view believing it is current.
//     Losing events is recoverable; not knowing you lost them is not.

// StreamGapType is the SSE event name the server uses to report dropped
// events. It is namespaced away from §76's types so it cannot collide with a
// real one.
const StreamGapType = "heyarr.stream.gap"

// Gap says the stream lost events and where to resume from.
type Gap struct {
	ResumeAfter int64  `json:"resume_after"`
	Dropped     int64  `json:"dropped"`
	Detail      string `json:"detail"`
}

// StreamMessage is one frame: either an event or a gap notice, never both.
type StreamMessage struct {
	Event *Event
	Gap   *Gap
}

// EventStream is an open SSE connection.
type EventStream struct {
	body   io.ReadCloser
	reader *bufio.Reader
	// lastSeq is the highest sequence delivered, which is what a reconnect
	// resumes after.
	lastSeq int64
}

// maxFrameBytes bounds one SSE frame. An event payload is small; an unbounded
// reader here would let a wedged or hostile server grow the CLI's heap without
// limit.
const maxFrameBytes = 4 << 20

// Events opens the stream, resuming after the given sequence. A zero `after`
// starts from the beginning of the log, which is what "show me everything"
// means; `types` filters server-side and accepts trailing-* namespace prefixes.
//
// It returns once the server has confirmed the subscription, so the caller may
// safely do the thing whose events it must not miss.
func (c *Client) Events(ctx context.Context, after int64, types []string) (*EventStream, error) {
	q := url.Values{}
	if after > 0 {
		q.Set("after", strconv.FormatInt(after, 10))
	}
	if len(types) > 0 {
		q.Set("types", strings.Join(types, ","))
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/events", q, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-store")

	resp, err := c.do(c.stream, req)
	if err != nil {
		return nil, err
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("the event endpoint answered with %q rather than an event stream", ct)
	}

	s := &EventStream{body: resp.Body, reader: bufio.NewReader(resp.Body), lastSeq: after}
	if err := s.waitForReady(); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	return s, nil
}

// waitForReady consumes up to the server's `: ready` comment.
//
// Waiting for the condition rather than for a duration is the whole point: a
// client that sleeps 100ms and hopes the subscription exists is a client whose
// tests fail on a loaded CI runner, and whose `--wait` silently misses the
// event it was started to see.
func (s *EventStream) waitForReady() error {
	for {
		line, err := s.readLine()
		if err != nil {
			return fmt.Errorf("the event stream closed before it was ready: %w", err)
		}
		if strings.TrimSpace(line) == ": ready" {
			return nil
		}
		// `retry:` and other preamble lines are fine; anything else means the
		// server is not speaking the protocol we think it is, and the frame
		// loop will say so.
		if strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "data:") ||
			strings.HasPrefix(line, "id:") {
			return errors.New("the event stream sent a frame before confirming the subscription")
		}
	}
}

func (s *EventStream) readLine() (string, error) {
	line, err := s.reader.ReadString('\n')
	if len(line) > maxFrameBytes {
		return "", errors.New("the event stream sent an implausibly long line")
	}
	if err != nil {
		return line, err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// LastSeq is the highest sequence this stream has delivered. Reconnect with it
// as `after` and nothing is lost or repeated.
func (s *EventStream) LastSeq() int64 { return s.lastSeq }

// Recv returns the next frame, blocking until one arrives. It returns io.EOF
// when the server closed the stream, which is an ordinary outcome: the server
// closes after a gap notice, and the caller is expected to reconnect.
func (s *EventStream) Recv() (StreamMessage, error) {
	var (
		name string
		data strings.Builder
		size int
	)
	for {
		line, err := s.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) && strings.TrimSpace(line) == "" && data.Len() == 0 {
				return StreamMessage{}, io.EOF
			}
			if data.Len() == 0 {
				return StreamMessage{}, err
			}
			// A truncated frame is not a frame. Reporting EOF is honest;
			// delivering half an event is not.
			return StreamMessage{}, io.ErrUnexpectedEOF
		}

		switch {
		case line == "":
			if data.Len() == 0 {
				continue // a stray blank line, or the end of a comment-only frame
			}
			return s.frame(name, data.String())
		case strings.HasPrefix(line, ":"):
			continue // a comment; the heartbeat is one
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			size += len(line)
			if size > maxFrameBytes {
				return StreamMessage{}, errors.New("the event stream sent an implausibly large frame")
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default:
			// `id:` and anything else. The sequence is inside the payload too,
			// so nothing is lost by not parsing it here.
			continue
		}
	}
}

// frame decodes one completed frame.
func (s *EventStream) frame(name, data string) (StreamMessage, error) {
	if name == StreamGapType {
		var gap Gap
		if err := json.Unmarshal([]byte(data), &gap); err != nil {
			return StreamMessage{}, fmt.Errorf("the stream reported a gap this client could not read: %w", err)
		}
		if gap.ResumeAfter > s.lastSeq {
			s.lastSeq = gap.ResumeAfter
		}
		return StreamMessage{Gap: &gap}, nil
	}
	var e Event
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return StreamMessage{}, fmt.Errorf("the stream sent an event this client could not decode: %w", err)
	}
	if e.Seq > s.lastSeq {
		s.lastSeq = e.Seq
	}
	return StreamMessage{Event: &e}, nil
}

// Close ends the stream.
func (s *EventStream) Close() error { return s.body.Close() }
