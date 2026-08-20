package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/media"
)

// JobType is the queue's name for a probe (§75). The ingest pipeline enqueues
// it; nothing else may spell it.
const JobType = "probe_blob"

// Capability is what a worker must advertise to claim one (ADR-0023,
// ADR-0008). A worker that resolved no ffprobe advertises nothing, never
// claims this type, and the jobs stay pending and visible rather than failing.
//
// It is an alias for the toolchain's constant rather than a second spelling of
// "ffprobe". Two constants holding one string is a drift waiting to happen,
// and the failure it produces is the worst shape available: a job that
// requires a capability nobody advertises, waiting forever, with both sides
// looking correct in isolation.
const Capability = media.CapabilityFFprobe

// Payload is the probe_blob job's payload — the wire contract between ingest
// and the handler.
type Payload struct {
	// BlobHash is what to probe. A probe describes bytes, so the job names
	// bytes rather than an asset: two assets sharing a blob need one probe.
	BlobHash string `json:"blob_hash"`
	// Size is the blob's size, carried rather than looked up.
	//
	// The prober needs it for the fallback threshold, and ingest already knows
	// it — the job would otherwise re-read a row that was written moments ago
	// in the same process. It is advisory: a wrong size costs a fallback
	// decision, never correctness, because the threshold only ever decides
	// which of two routes to the same answer to take.
	Size int64 `json:"size"`
}

// DedupeKey is the queue's idempotency key. Ingesting the same blob twice
// while its first probe is still live yields one job, not two (ADR-0008).
func DedupeKey(blobHash string) string { return "probe:" + blobHash }

// EndpointClient builds an HTTP client and a base URL for a peer endpoint.
//
// The unix:// case exists because that is how Heyarr is actually deployed and
// how the acceptance demo runs: the API listens on a unix socket and nothing
// binds TCP at all. A prober that could only speak to a TCP address would be a
// prober that works in tests and not in production — and finding that out
// would take until someone tried it.
//
// ffprobe is unaffected either way: it talks to the loopback proxy over TCP
// and never learns how the proxy reaches the peer.
func EndpointClient(endpoint string, timeout time.Duration) (*http.Client, string, error) {
	if endpoint == "" {
		return nil, "", errors.New("probe: this node has no reachable endpoint configured; " +
			"set peer.endpoint, or bind http.addr to a concrete address")
	}
	if socket, ok := strings.CutPrefix(endpoint, "unix://"); ok {
		dialer := &net.Dialer{Timeout: timeout}
		client := &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socket)
			},
		}}
		// The host is a placeholder that never resolves — the transport
		// ignores it entirely — but it has to be syntactically present, and it
		// has to be stable so nothing downstream treats two probes as two
		// different origins.
		return client, "http://heyarr", nil
	}
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		return nil, "", fmt.Errorf("probe: peer endpoint %q must be http://, https:// or unix://", endpoint)
	}
	return &http.Client{}, strings.TrimSuffix(endpoint, "/"), nil
}

// BlobURL is where a blob's bytes are, on a given base (ADR-0013).
//
// It is assembled here rather than by each caller so that the one endpoint
// ADR-0013 describes has one spelling. A second spelling is how a contract
// with four consumers acquires a fifth that is subtly different.
//
// base is an ORIGIN — "http://peer:7777", or "" for a URL relative to this
// API. It is not the API prefix: passing httpapi.APIPrefix produces
// "/api/v1/api/v1/blobs/...", which is a 404 for every caller and was caught
// by a test rather than by review.
func BlobURL(base, hash string) string {
	return base + "/api/v1/blobs/" + hash + "/content"
}

// probableExts are the extensions worth handing to ffprobe.
//
// It is an allowlist rather than a denylist because the cost of being wrong
// runs one way: a missed probe is metadata Heyarr does not have, and a spurious
// one is a subprocess, a job slot and a row describing a JPEG as a one-frame
// video — which is true, useless, and then has to be reasoned about by the
// planner.
var probableExts = map[string]bool{
	// Video containers.
	".mkv": true, ".mp4": true, ".m4v": true, ".mov": true, ".avi": true,
	".webm": true, ".ts": true, ".m2ts": true, ".mpg": true, ".mpeg": true,
	".wmv": true, ".flv": true, ".ogv": true,
	// Audio.
	".flac": true, ".mp3": true, ".m4a": true, ".m4b": true, ".ogg": true,
	".opus": true, ".wav": true, ".aac": true, ".wma": true, ".alac": true,
	".aiff": true, ".ape": true, ".wv": true, ".dsf": true,
}

// IsProbable reports whether an extension names something worth probing.
func IsProbable(ext string) bool { return probableExts[strings.ToLower(ext)] }
