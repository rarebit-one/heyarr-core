package downloads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The SABnzbd download client, behind the provider registry's Downloader
// contract (§58, §59, M11). The first USENET client beside the torrent ones.
//
// # What it shares with the torrent clients, and why
//
// The safety and identity decisions are the contract's, not the protocol's, so
// they are the same as Transmission's and qBittorrent's: a download client is
// SHARED, so every mutating operation filters on a label first (here a SABnzbd
// CATEGORY, its native version of the same idea); identity is the client's own
// stable id (here SABnzbd's nzo_id, never the name); and a service Heyarr does
// not install is version-reported rather than version-pinned (ADR-0025). What
// differs is only the wire: SABnzbd is a single HTTP API keyed by an api_key
// query parameter, a release is added by URL with `mode=addurl`, and a transfer
// lives in the QUEUE while it downloads and moves to the HISTORY when it is
// done — so "what is this doing" is two reads merged, not one.
//
// # It refuses what is not its to take
//
// Add refuses a source that is not an .nzb, which is what lets it compose with
// the torrent and plain-HTTP clients through registry.Grab's fall-through — the
// same discipline qBittorrent applies from the other side (ErrNotTorrentSource).
// A usenet client that accepted a magnet would silently swallow a transfer that
// belonged to a torrent client.
//
// # Its live exercise is opt-in, never a daemon in CI (ADR-0026)
//
// SABnzbd is an operator-managed service Heyarr targets by configuration; it is
// not installed and so not pinned or run on the merge path. The merge path tests
// it against a fake of its HTTP API; the real exercise is TestLiveSABnzbd,
// pointed at whatever instance you have and skipped when unset. Unlike
// qBittorrent, a daemon-in-the-loop harness is NOT provided: a real SABnzbd
// transfer requires a real Usenet news server with the article actually posted
// (a full NNTP + yEnc + .nzb stack), which has no clean disposable form the way
// qBittorrent's private web seed does — so that leg is a documented follow-up
// (#379), not a faked pass.

// SABOptions configure a SABnzbd client. It mirrors QBOptions so the download
// clients are configured the same way by construct.go — the difference between
// them is the wire, not the config.
type SABOptions struct {
	Name     string
	Endpoint string
	// APIKey is SABnzbd's credential — one opaque secret sent as a query
	// parameter (AuthToken), not a username/password pair. Optional: an instance
	// on a trusted network with the key requirement relaxed is a supported
	// deployment, exactly as an auth-bypassed qBittorrent is.
	APIKey       string
	PathMap      PathMap
	Label        string
	Capabilities []providers.Capability
	// HTTPClient is injectable so tests drive the real transport against a fake
	// of the HTTP API rather than a stub of the client.
	HTTPClient *http.Client
	Now        func() time.Time
}

// SABClient drives one SABnzbd instance.
type SABClient struct {
	name    string
	caps    []providers.Capability
	pathMap PathMap
	label   string
	now     func() time.Time
	api     *sabTransport
}

// NewSABnzbd builds a SABnzbd client, refusing a mis-wired one.
func NewSABnzbd(opts SABOptions) (*SABClient, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return nil, errors.New("downloads: a sabnzbd client needs a provider name")
	}
	if strings.TrimSpace(opts.Endpoint) == "" {
		return nil, errors.New("downloads: a sabnzbd client needs an endpoint")
	}
	httpc := opts.HTTPClient
	if httpc == nil {
		httpc = &http.Client{Timeout: defaultTimeout}
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	caps := opts.Capabilities
	if len(caps) == 0 {
		caps = []providers.Capability{providers.CapabilityDownload}
	}
	label := strings.TrimSpace(opts.Label)
	if label == "" {
		label = DefaultLabel
	}
	return &SABClient{
		name:    strings.TrimSpace(opts.Name),
		caps:    caps,
		pathMap: opts.PathMap,
		label:   label,
		now:     now,
		api: &sabTransport{
			base:   strings.TrimRight(strings.TrimSpace(opts.Endpoint), "/"),
			apiKey: opts.APIKey,
			http:   httpc,
		},
	}, nil
}

var _ providers.Downloader = (*SABClient)(nil)

// Name implements providers.Provider.
func (c *SABClient) Name() string { return c.name }

// Capabilities implements providers.Provider.
func (c *SABClient) Capabilities() []providers.Capability {
	return append([]providers.Capability(nil), c.caps...)
}

// Label is what this client tags (categorises) and recognises its transfers by.
func (c *SABClient) Label() string { return c.label }

// Check exercises the instance and reports what it found (ADR-0025).
//
// It makes an AUTHENTICATED read (the queue) so a wrong api_key is caught here
// rather than an hour later on the first grab, then reads the app version — a
// real round trip returning real facts, so a client cannot report itself healthy
// merely because it was configured. SABnzbd exposes no separate "API version"
// to gate on the way qBittorrent's Web API does, so the version is reported for
// an operator to read rather than compared against a floor.
func (c *SABClient) Check(ctx context.Context) providers.Health {
	at := c.now()

	// The authed read first: it is what a wrong credential fails, and reporting
	// "the credential was refused" is more actionable than a version mismatch.
	if _, err := c.api.call(ctx, url.Values{"mode": {"queue"}, "limit": {"0"}}); err != nil {
		if errors.Is(err, ErrUnauthorised) {
			return providers.Unhealthy(
				"the credential was refused — check the api key", at)
		}
		return providers.Unhealthy(sabShort(err), at)
	}

	body, err := c.api.call(ctx, url.Values{"mode": {"version"}})
	if err != nil {
		return providers.Unhealthy(sabShort(err), at)
	}
	var ver struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &ver); err != nil {
		return providers.Unhealthy("could not read the version", at)
	}
	return providers.Healthy(strings.TrimSpace(ver.Version), at)
}

// ErrNotUsenetSource is the refusal that lets this client compose with the
// torrent and http clients: a source it will not take falls through to whichever
// client will. It mirrors qBittorrent's ErrNotTorrentSource from the other side.
var ErrNotUsenetSource = errors.New("downloads: not a usenet source (.nzb)")

// usenetSourcePrefixes are the optional scheme tags a feed adapter may put in
// front of an .nzb URL to route it here regardless of the URL's own shape — the
// same seam followed.YtDlpSourceScheme uses for a video whose address is an
// http URL. They are stripped before the URL reaches the wire.
var usenetSourcePrefixes = []string{"usenet:", "nzb:"}

// isUsenetSource reports whether this client should take a source, and returns
// the bare .nzb URL with any routing tag stripped. An .nzb URL — by an explicit
// usenet tag, or by a path ending in .nzb — is usenet; everything else (a
// magnet, a direct http file) belongs to another client.
func isUsenetSource(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	lower := strings.ToLower(trimmed)
	for _, p := range usenetSourcePrefixes {
		if strings.HasPrefix(lower, p) {
			return strings.TrimSpace(trimmed[len(p):]), true
		}
	}
	// Strip a query so `https://host/x.nzb?apikey=…` still reads as one.
	if i := strings.IndexByte(lower, '?'); i >= 0 {
		lower = lower[:i]
	}
	if strings.HasSuffix(lower, ".nzb") {
		return trimmed, true
	}
	return "", false
}

// Add queues a release by URL, tagged with this client's category.
//
// Idempotent by construction, which SABnzbd does not give for free — re-adding
// the same URL enqueues a second job. So each add carries a DETERMINISTIC
// nzbname derived from the source, and a re-add first looks for a job already
// carrying that name (in the queue or the history) and returns it. A job WILL be
// re-run (invariant 9), and this makes the re-run indistinguishable from the
// first call rather than a duplicate download.
func (c *SABClient) Add(ctx context.Context, source secret.Value) (providers.Transfer, error) {
	// Reveal() only here and nowhere else in this method: an .nzb URL on a
	// private indexer carries an api key identifying a person, and it must not
	// reach a log line or the error below.
	raw := strings.TrimSpace(source.Reveal())
	if raw == "" {
		return providers.Transfer{}, errors.New("downloads: nothing to add")
	}
	nzbURL, ok := isUsenetSource(raw)
	if !ok {
		return providers.Transfer{}, ErrNotUsenetSource
	}

	// The stable name that makes a re-add idempotent. It is derived from the
	// source, so the same release always maps to the same name, and it never
	// contains the source itself (sourceKey is a digest), so it is safe to log.
	name := "heyarr-" + sourceKey(source)

	// A job already carrying this name is this same release, added before.
	if existing, err := c.ours(ctx); err == nil {
		for _, s := range existing {
			if s.matchesName(name) {
				return c.toTransfer(s), nil
			}
		}
	}

	body, err := c.api.call(ctx, url.Values{
		"mode":    {"addurl"},
		"name":    {nzbURL},
		"nzbname": {name},
		"cat":     {c.label},
	})
	if err != nil {
		return providers.Transfer{}, err
	}
	var added struct {
		Status bool     `json:"status"`
		NZOIDs []string `json:"nzo_ids"`
	}
	if err := json.Unmarshal(body, &added); err != nil {
		return providers.Transfer{}, &sabError{detail: "decoding addurl", err: err}
	}
	if !added.Status || len(added.NZOIDs) == 0 {
		return providers.Transfer{}, fmt.Errorf(
			"%w: SABnzbd did not accept the source", ErrRPCFailure)
	}
	return providers.Transfer{ID: added.NZOIDs[0], Name: name}, nil
}

// Transfers is everything this client is doing that BELONGS TO HEYARR.
//
// The category is the filter, and it is the point: a download client is shared,
// and a transfer without our category is invisible to everything above — not
// merely excluded from mutation, but absent, so no caller can act on one it was
// never shown. A SABnzbd transfer lives in the queue while it downloads and in
// the history once it is done, so this reads both and merges them.
func (c *SABClient) Transfers(ctx context.Context) ([]providers.Transfer, error) {
	ours, err := c.ours(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]providers.Transfer, 0, len(ours))
	for _, s := range ours {
		out = append(out, c.toTransfer(s))
	}
	return out, nil
}

// Remove takes a transfer out of the client, refusing anything that is not ours.
//
// The id is looked up in our filtered view first: a caller handing a foreign
// nzo_id — from a stale row, a bug, anywhere — gets a refusal rather than a
// removal. It is deleted from whichever list holds it (a queued job from the
// queue, a finished one from the history). deleteData is separate because "stop
// tracking" and "delete the bytes" are different decisions, and removal must
// never delete bytes Heyarr has not yet ingested.
func (c *SABClient) Remove(ctx context.Context, id string, deleteData bool) error {
	ours, err := c.ours(ctx)
	if err != nil {
		return err
	}
	var found *sabSlot
	for i := range ours {
		if strings.EqualFold(ours[i].nzoID, id) {
			found = &ours[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("%w: %s is not a transfer Heyarr queued", ErrNotOurs, id)
	}
	mode := "queue"
	if found.done {
		mode = "history"
	}
	_, err = c.api.call(ctx, url.Values{
		"mode":      {mode},
		"name":      {"delete"},
		"value":     {id},
		"del_files": {sabBool(deleteData)},
	})
	return err
}

// --- SABnzbd shapes ---------------------------------------------------------

// sabQueueSlot is one entry from mode=queue. Only the fields the pipeline reads
// are named; they are SABnzbd's and change when it changes. SABnzbd reports the
// sizes as decimal-MB STRINGS, which is why they are parsed rather than typed.
type sabQueueSlot struct {
	NZOID    string `json:"nzo_id"`
	Filename string `json:"filename"`
	Cat      string `json:"cat"`
	Status   string `json:"status"`
	MB       string `json:"mb"`
	MBLeft   string `json:"mbleft"`
}

// sabHistorySlot is one entry from mode=history.
type sabHistorySlot struct {
	NZOID       string `json:"nzo_id"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	Status      string `json:"status"`
	Bytes       int64  `json:"bytes"`
	Storage     string `json:"storage"`
	FailMessage string `json:"fail_message"`
}

// sabSlot is a queue or history entry reduced to what the client acts on, so the
// two lists can be filtered, matched and mapped by one set of code.
type sabSlot struct {
	nzoID      string
	name       string
	done       bool
	bytesTotal int64
	bytesDone  int64
	path       string // the completed storage path, history only
	failure    string
}

func (s sabSlot) matchesName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(s.name), name)
}

// ours reads the queue and the history, each filtered to this client's
// category, and returns them as one list. A defensive re-check of the category
// keeps a broken server-side filter from ever exposing a foreign transfer.
func (c *SABClient) ours(ctx context.Context) ([]sabSlot, error) {
	qBody, err := c.api.call(ctx, url.Values{"mode": {"queue"}})
	if err != nil {
		return nil, err
	}
	var queue struct {
		Queue struct {
			Slots []sabQueueSlot `json:"slots"`
		} `json:"queue"`
	}
	if err := json.Unmarshal(qBody, &queue); err != nil {
		return nil, &sabError{detail: "decoding queue", err: err}
	}

	hBody, err := c.api.call(ctx, url.Values{"mode": {"history"}})
	if err != nil {
		return nil, err
	}
	var history struct {
		History struct {
			Slots []sabHistorySlot `json:"slots"`
		} `json:"history"`
	}
	if err := json.Unmarshal(hBody, &history); err != nil {
		return nil, &sabError{detail: "decoding history", err: err}
	}

	var out []sabSlot
	seen := map[string]struct{}{}
	// History first: it is the terminal state, so if a nzo_id is somehow in both
	// (a job finishing as we read) the done view wins.
	for _, h := range history.History.Slots {
		if !strings.EqualFold(strings.TrimSpace(h.Category), c.label) {
			continue
		}
		out = append(out, sabSlot{
			nzoID:      h.NZOID,
			name:       h.Name,
			done:       strings.EqualFold(h.Status, "Completed"),
			bytesTotal: h.Bytes,
			bytesDone:  h.Bytes,
			path:       h.Storage,
			failure:    sabFailure(h),
		})
		seen[strings.ToLower(h.NZOID)] = struct{}{}
	}
	for _, q := range queue.Queue.Slots {
		if !strings.EqualFold(strings.TrimSpace(q.Cat), c.label) {
			continue
		}
		if _, dup := seen[strings.ToLower(q.NZOID)]; dup {
			continue
		}
		total := mbToBytes(q.MB)
		left := mbToBytes(q.MBLeft)
		done := total - left
		if done < 0 {
			done = 0
		}
		out = append(out, sabSlot{
			nzoID:      q.NZOID,
			name:       q.Filename,
			done:       false,
			bytesTotal: total,
			bytesDone:  done,
		})
	}
	return out, nil
}

// toTransfer reduces a slot to the registry's value type. The path is resolved
// only on completion: before then SABnzbd's storage names an incomplete
// directory ingest cannot open, which would read as an ingest bug rather than a
// timing one.
func (c *SABClient) toTransfer(s sabSlot) providers.Transfer {
	out := providers.Transfer{
		ID:         s.nzoID,
		Name:       s.name,
		Done:       s.done,
		BytesTotal: s.bytesTotal,
		BytesDone:  s.bytesDone,
		Error:      s.failure,
	}
	if s.done && s.failure == "" {
		out.Path = c.resolvePath(s.path)
	}
	return out
}

// resolvePath translates SABnzbd's completed path into one Heyarr can open. An
// unmapped path is returned as-is, the right default for the common single-host
// deployment where the two share a filesystem.
func (c *SABClient) resolvePath(storage string) string {
	full := strings.TrimSpace(storage)
	if full == "" {
		return ""
	}
	full = path.Clean(full)
	if mapped, ok := c.pathMap.Resolve(full); ok {
		return mapped
	}
	return full
}

// sabFailure renders a history slot's failure, empty when it succeeded.
func sabFailure(h sabHistorySlot) string {
	if strings.EqualFold(h.Status, "Failed") {
		if msg := strings.TrimSpace(h.FailMessage); msg != "" {
			return "SABnzbd reported: " + msg
		}
		return "SABnzbd reported the download failed"
	}
	return ""
}

// mbToBytes parses SABnzbd's decimal-MB string into bytes. A malformed value is
// read as zero rather than an error: progress is advisory (invariant 1 has
// Heyarr hash what arrived), and a queue read must not fail because one size
// field was unexpected.
func mbToBytes(mb string) int64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(mb), 64)
	if err != nil || f < 0 {
		return 0
	}
	return int64(f * 1024 * 1024)
}

func sabBool(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// --- transport --------------------------------------------------------------

// sabTransport speaks the SABnzbd HTTP API: one endpoint, every call a GET to
// /api with a `mode` and, when configured, an `apikey`. Output is always JSON.
type sabTransport struct {
	base   string
	apiKey string
	http   *http.Client
}

// sabError is a SABnzbd call that failed in a way a caller may need to read.
type sabError struct {
	detail string
	err    error
}

func (e *sabError) Error() string {
	if e.err != nil {
		return "sabnzbd: " + e.detail + ": " + e.err.Error()
	}
	return "sabnzbd: " + e.detail
}

func (e *sabError) Unwrap() error { return e.err }

// sabShort renders an error for a health detail — a few words, never a
// credential, and never a Go error chain rendered in full.
func sabShort(err error) string {
	var se *sabError
	if errors.As(err, &se) {
		return se.detail
	}
	return "unreachable"
}

// call issues one API request and returns the raw body, having first turned
// SABnzbd's in-band error envelope into a Go error — a wrong api key is a 200
// carrying `{"status": false, "error": "API Key Incorrect"}`, so the status code
// alone would read as success.
func (t *sabTransport) call(ctx context.Context, params url.Values) ([]byte, error) {
	q := url.Values{}
	for k, v := range params {
		q[k] = v
	}
	q.Set("output", "json")
	if t.apiKey != "" {
		q.Set("apikey", t.apiKey)
	}
	endpoint := t.base + "/api?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, &sabError{detail: "building the request", err: err}
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, &sabError{detail: "reaching sabnzbd", err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, &sabError{detail: "reading the response", err: err}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrUnauthorised
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &sabError{detail: fmt.Sprintf("sabnzbd returned status %d", resp.StatusCode)}
	}
	if err := sabAPIError(body); err != nil {
		return nil, err
	}
	return body, nil
}

// sabAPIError reads SABnzbd's in-band error envelope. A successful call to a
// read endpoint has no top-level `error`, so this returns nil for it; a failed
// one carries `{"status": false, "error": "…"}`, and an api-key error becomes
// ErrUnauthorised so Check can name the credential.
func sabAPIError(body []byte) error {
	var env struct {
		Status *bool  `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		// A non-object body (an array, a bare value) is not an error envelope.
		return nil
	}
	msg := strings.TrimSpace(env.Error)
	if msg == "" {
		return nil
	}
	low := strings.ToLower(msg)
	if strings.Contains(low, "api key") || strings.Contains(low, "apikey") ||
		strings.Contains(low, "not logged in") {
		return ErrUnauthorised
	}
	return &sabError{detail: msg}
}
