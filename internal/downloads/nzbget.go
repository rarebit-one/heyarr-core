package downloads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
	"github.com/rarebit-one/heyarr-core/internal/providers/transporterr"
)

// The NZBGet download client, behind the provider registry's Downloader
// contract (§58, §59, M11). The second USENET client beside SABnzbd (#379).
//
// # What it shares with the other clients, and why
//
// The safety and identity decisions are the contract's, not the protocol's, so
// they are the same as SABnzbd's: a download client is SHARED, so every
// mutating operation filters on a label first (here an NZBGet CATEGORY); identity
// is the client's own stable id (here NZBGet's integer NZBID, rendered as a
// string, never the name); and a service Heyarr does not install is
// version-reported rather than version-pinned (ADR-0025). It even shares
// SABnzbd's source discipline verbatim — isUsenetSource and ErrNotUsenetSource
// are this package's, so an .nzb routes to whichever usenet client is
// configured and a magnet still falls through to a torrent one.
//
// What differs is only the wire. Where SABnzbd is GET /api?mode=…, NZBGet is
// JSON-RPC: every call is a POST of {"method","params","id"} to /jsonrpc,
// authenticated with HTTP BASIC (username+password, AuthBasic), so a wrong
// credential is a plain 401 rather than SABnzbd's in-band {"status":false}
// envelope. A transfer lives in a QUEUE group (listgroups) while it downloads
// and moves to the HISTORY when it is done, so "what is this doing" is two reads
// merged, the same shape SABnzbd has.
//
// # Its live exercise is opt-in, never a daemon in CI (ADR-0026)
//
// NZBGet is an operator-managed service Heyarr targets by configuration; it is
// not installed and so not pinned or run on the merge path. The merge path tests
// it against a fake of its JSON-RPC API; the real exercise is TestLiveNZBGet,
// pointed at whatever instance you have and skipped when unset. As with SABnzbd,
// a daemon-in-the-loop harness is NOT provided here: a real NZBGet transfer
// needs a real Usenet news server with the article posted (a full NNTP + yEnc +
// .nzb stack), which has no clean disposable form the way qBittorrent's private
// web seed does — so that leg stays the documented follow-up (#379), not a faked
// pass.

// NZBGetOptions configure an NZBGet client. It mirrors QBOptions rather than
// SABOptions on the credential: NZBGet authenticates with a username and
// password (AuthBasic), not a single api key.
type NZBGetOptions struct {
	Name     string
	Endpoint string
	// Username and Password are NZBGet's control credential. Optional as a pair:
	// an instance on a trusted network with the control password blank is a
	// supported deployment, exactly as an auth-bypassed qBittorrent or SABnzbd
	// is.
	Username     string
	Password     string
	PathMap      PathMap
	Label        string
	Capabilities []providers.Capability
	// HTTPClient is injectable so tests drive the real transport against a fake
	// of the JSON-RPC API rather than a stub of the client.
	HTTPClient *http.Client
	Now        func() time.Time
}

// NZBGetClient drives one NZBGet instance.
type NZBGetClient struct {
	name    string
	caps    []providers.Capability
	pathMap PathMap
	label   string
	now     func() time.Time
	api     *nzbTransport
}

// NewNZBGet builds an NZBGet client, refusing a mis-wired one.
func NewNZBGet(opts NZBGetOptions) (*NZBGetClient, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return nil, errors.New("downloads: an nzbget client needs a provider name")
	}
	if strings.TrimSpace(opts.Endpoint) == "" {
		return nil, errors.New("downloads: an nzbget client needs an endpoint")
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
	return &NZBGetClient{
		name:    strings.TrimSpace(opts.Name),
		caps:    caps,
		pathMap: opts.PathMap,
		label:   label,
		now:     now,
		api: &nzbTransport{
			base: strings.TrimRight(strings.TrimSpace(opts.Endpoint), "/"),
			user: opts.Username,
			pass: opts.Password,
			http: httpc,
		},
	}, nil
}

var _ providers.Downloader = (*NZBGetClient)(nil)

// Name implements providers.Provider.
func (c *NZBGetClient) Name() string { return c.name }

// Capabilities implements providers.Provider.
func (c *NZBGetClient) Capabilities() []providers.Capability {
	return append([]providers.Capability(nil), c.caps...)
}

// Label is what this client tags (categorises) and recognises its transfers by.
func (c *NZBGetClient) Label() string { return c.label }

// Check exercises the instance and reports what it found (ADR-0025).
//
// It makes an AUTHENTICATED read (the group list) so a wrong credential is
// caught here rather than an hour later on the first grab, then reads the app
// version — a real round trip returning real facts, so a client cannot report
// itself healthy merely because it was configured. NZBGet exposes no API-version
// floor to gate on, so the version is reported for an operator to read.
func (c *NZBGetClient) Check(ctx context.Context) providers.Health {
	at := c.now()

	// The authed read first: it is what a wrong credential fails, and reporting
	// "the credential was refused" is more actionable than a version mismatch.
	if _, err := c.api.call(ctx, "listgroups", []any{0}); err != nil {
		if errors.Is(err, ErrUnauthorised) {
			return providers.Unhealthy(
				"the credential was refused — check the username and password", at)
		}
		return providers.Unhealthy(nzbShort(err), at)
	}

	res, err := c.api.call(ctx, "version", []any{})
	if err != nil {
		return providers.Unhealthy(nzbShort(err), at)
	}
	var ver string
	if err := json.Unmarshal(res, &ver); err != nil {
		return providers.Unhealthy("could not read the version", at)
	}
	return providers.Healthy(strings.TrimSpace(ver), at)
}

// Add queues a release by URL, tagged with this client's category.
//
// Idempotent by construction, which NZBGet does not give for free — appending
// the same URL enqueues a second job. So each add carries a DETERMINISTIC nzb
// name derived from the source, and a re-add first looks for a job already
// carrying that name (in the queue or the history) and returns it. A job WILL be
// re-run (invariant 9), and this makes the re-run indistinguishable from the
// first call rather than a duplicate download. It is the same discipline
// SABnzbd's Add uses, because idempotency is the contract's requirement, not a
// property of either wire.
func (c *NZBGetClient) Add(ctx context.Context, source secret.Value) (providers.Transfer, error) {
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
	// NZBGet strips a trailing ".nzb" from NZBFilename to form the NZBName it
	// then reports, so the name matched below is the bare form.
	name := "heyarr-" + sourceKey(source)

	// A job already carrying this name is this same release, added before.
	if existing, err := c.ours(ctx); err == nil {
		for _, s := range existing {
			if s.matchesName(name) {
				return c.toTransfer(s), nil
			}
		}
	}

	// append(NZBFilename, NZBContent, Category, Priority, AddToTop, AddPaused,
	// DupeKey, DupeScore, DupeMode) — the widely-supported nine-argument form.
	// NZBContent is the URL: NZBGet fetches the .nzb itself. Returns the new
	// integer NZBID, or 0 when it refused the source.
	res, err := c.api.call(ctx, "append", []any{
		name + ".nzb", // NZBFilename → the NZBName NZBGet reports back
		nzbURL,        // NZBContent — a URL is fetched by NZBGet
		c.label,       // Category
		0,             // Priority
		false,         // AddToTop
		false,         // AddPaused
		"",            // DupeKey
		0,             // DupeScore
		"SCORE",       // DupeMode
	})
	if err != nil {
		return providers.Transfer{}, err
	}
	var nzbID int64
	if err := json.Unmarshal(res, &nzbID); err != nil {
		return providers.Transfer{}, &nzbError{detail: "decoding append", err: err}
	}
	if nzbID <= 0 {
		return providers.Transfer{}, fmt.Errorf(
			"%w: NZBGet did not accept the source", ErrRPCFailure)
	}
	return providers.Transfer{ID: strconv.FormatInt(nzbID, 10), Name: name}, nil
}

// Transfers is everything this client is doing that BELONGS TO HEYARR.
//
// The category is the filter, and it is the point: a download client is shared,
// and a transfer without our category is invisible to everything above — not
// merely excluded from mutation, but absent, so no caller can act on one it was
// never shown. An NZBGet transfer lives in a queue group while it downloads and
// in the history once it is done, so this reads both and merges them.
func (c *NZBGetClient) Transfers(ctx context.Context) ([]providers.Transfer, error) {
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
// NZBID — from a stale row, a bug, anywhere — gets a refusal rather than a
// removal. It is deleted from whichever list holds it, and deleteData chooses
// the "Final" variant of the command that also deletes the downloaded files,
// because "stop tracking" and "delete the bytes" are different decisions and
// removal must never delete bytes Heyarr has not yet ingested.
func (c *NZBGetClient) Remove(ctx context.Context, id string, deleteData bool) error {
	ours, err := c.ours(ctx)
	if err != nil {
		return err
	}
	var found *nzbSlot
	for i := range ours {
		if strings.EqualFold(ours[i].id, id) {
			found = &ours[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("%w: %s is not a transfer Heyarr queued", ErrNotOurs, id)
	}
	nzbID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		// Our own ids are always NZBGet's decimal NZBID, so this is unreachable
		// through the lookup above; reported rather than panicked in case a
		// hand-built id ever reaches here.
		return fmt.Errorf("%w: %s is not an NZBGet id", ErrNotOurs, id)
	}
	var command string
	switch {
	case found.done && deleteData:
		command = "HistoryFinalDelete" // remove the history record AND the files
	case found.done:
		command = "HistoryDelete" // remove the history record, keep the files
	case deleteData:
		command = "GroupFinalDelete" // cancel the download AND delete its files
	default:
		command = "GroupDelete" // cancel the download, keep what was fetched
	}
	// editqueue(Command, Offset, Text, IDs) → bool.
	res, err := c.api.call(ctx, "editqueue", []any{command, 0, "", []int64{nzbID}})
	if err != nil {
		return err
	}
	var ok bool
	if err := json.Unmarshal(res, &ok); err != nil {
		return &nzbError{detail: "decoding editqueue", err: err}
	}
	if !ok {
		return fmt.Errorf("%w: NZBGet declined to remove %s", ErrRPCFailure, id)
	}
	return nil
}

// --- NZBGet shapes ----------------------------------------------------------

// nzbGroup is one entry from listgroups. NZBGet splits a byte size into a low
// and a high 32-bit half; only the fields the pipeline reads are named, and they
// are NZBGet's, changing when it changes.
type nzbGroup struct {
	NZBID           int64  `json:"NZBID"`
	NZBName         string `json:"NZBName"`
	Category        string `json:"Category"`
	Status          string `json:"Status"`
	FileSizeLo      uint32 `json:"FileSizeLo"`
	FileSizeHi      uint32 `json:"FileSizeHi"`
	RemainingSizeLo uint32 `json:"RemainingSizeLo"`
	RemainingSizeHi uint32 `json:"RemainingSizeHi"`
}

// nzbHistory is one entry from history.
type nzbHistory struct {
	NZBID      int64  `json:"NZBID"`
	Name       string `json:"Name"`
	Category   string `json:"Category"`
	Status     string `json:"Status"`
	DestDir    string `json:"DestDir"`
	FileSizeLo uint32 `json:"FileSizeLo"`
	FileSizeHi uint32 `json:"FileSizeHi"`
}

// nzbSlot is a queue or history entry reduced to what the client acts on, so the
// two lists can be filtered, matched and mapped by one set of code.
type nzbSlot struct {
	id         string
	name       string
	done       bool
	bytesTotal int64
	bytesDone  int64
	path       string // the completed destination directory, history only
	failure    string
}

func (s nzbSlot) matchesName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(s.name), name)
}

// ours reads the group list and the history, each filtered to this client's
// category, and returns them as one list. A defensive re-check of the category
// keeps a broken server-side filter from ever exposing a foreign transfer.
func (c *NZBGetClient) ours(ctx context.Context) ([]nzbSlot, error) {
	gBody, err := c.api.call(ctx, "listgroups", []any{0})
	if err != nil {
		return nil, err
	}
	var groups []nzbGroup
	if err := json.Unmarshal(gBody, &groups); err != nil {
		return nil, &nzbError{detail: "decoding listgroups", err: err}
	}

	hBody, err := c.api.call(ctx, "history", []any{false})
	if err != nil {
		return nil, err
	}
	var history []nzbHistory
	if err := json.Unmarshal(hBody, &history); err != nil {
		return nil, &nzbError{detail: "decoding history", err: err}
	}

	var out []nzbSlot
	seen := map[int64]struct{}{}
	// History first: it is the terminal state, so if a NZBID is somehow in both
	// (a job finishing as we read) the done view wins.
	for _, h := range history {
		if !strings.EqualFold(strings.TrimSpace(h.Category), c.label) {
			continue
		}
		total := combineSize(h.FileSizeHi, h.FileSizeLo)
		out = append(out, nzbSlot{
			id:         strconv.FormatInt(h.NZBID, 10),
			name:       h.Name,
			done:       true,
			bytesTotal: total,
			bytesDone:  total,
			path:       h.DestDir,
			failure:    nzbFailure(h.Status),
		})
		seen[h.NZBID] = struct{}{}
	}
	for _, g := range groups {
		if !strings.EqualFold(strings.TrimSpace(g.Category), c.label) {
			continue
		}
		if _, dup := seen[g.NZBID]; dup {
			continue
		}
		total := combineSize(g.FileSizeHi, g.FileSizeLo)
		left := combineSize(g.RemainingSizeHi, g.RemainingSizeLo)
		done := total - left
		if done < 0 {
			done = 0
		}
		out = append(out, nzbSlot{
			id:         strconv.FormatInt(g.NZBID, 10),
			name:       g.NZBName,
			done:       false,
			bytesTotal: total,
			bytesDone:  done,
		})
	}
	return out, nil
}

// toTransfer reduces a slot to the registry's value type. The path is resolved
// only on completion: before then NZBGet's destination names an incomplete
// directory ingest cannot open, which would read as an ingest bug rather than a
// timing one.
func (c *NZBGetClient) toTransfer(s nzbSlot) providers.Transfer {
	out := providers.Transfer{
		ID:         s.id,
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

// resolvePath translates NZBGet's completed destination into one Heyarr can
// open. An unmapped path is returned as-is, the right default for the common
// single-host deployment where the two share a filesystem.
func (c *NZBGetClient) resolvePath(dest string) string {
	full := strings.TrimSpace(dest)
	if full == "" {
		return ""
	}
	full = path.Clean(full)
	if mapped, ok := c.pathMap.Resolve(full); ok {
		return mapped
	}
	return full
}

// nzbFailure renders a history status, empty when the download succeeded.
//
// NZBGet's history Status is a "CATEGORY/DETAIL" string — "SUCCESS/ALL",
// "WARNING/HEALTH", "FAILURE/PAR", "DELETED/MANUAL". SUCCESS and WARNING both
// mean the bytes are on disk (a WARNING is a health note, not a loss), so only a
// FAILURE or a DELETE is a failure to report; anything else is treated as usable
// bytes, because invariant 1 has Heyarr hash and verify what actually arrived.
func nzbFailure(status string) string {
	category := strings.ToUpper(strings.TrimSpace(status))
	if i := strings.IndexByte(category, '/'); i >= 0 {
		category = category[:i]
	}
	switch category {
	case "SUCCESS", "WARNING", "":
		return ""
	default:
		return "NZBGet reported: " + strings.TrimSpace(status)
	}
}

// combineSize joins NZBGet's split 32-bit size halves into a byte count.
func combineSize(hi, lo uint32) int64 {
	return int64(hi)<<32 | int64(lo)
}

// --- transport --------------------------------------------------------------

// nzbTransport speaks the NZBGet JSON-RPC API: one endpoint, every call a POST
// to /jsonrpc of {"method","params","id"}, authenticated with HTTP basic.
type nzbTransport struct {
	base string
	user string
	pass string
	http *http.Client
}

// nzbError is an NZBGet call that failed in a way a caller may need to read.
type nzbError struct {
	detail string
	err    error
}

func (e *nzbError) Error() string {
	if e.err != nil {
		return "nzbget: " + e.detail + ": " + e.err.Error()
	}
	return "nzbget: " + e.detail
}

func (e *nzbError) Unwrap() error { return e.err }

// nzbShort renders an error for a health detail — a few words, never a
// credential, and never a Go error chain rendered in full.
func nzbShort(err error) string {
	var ne *nzbError
	if errors.As(err, &ne) {
		return ne.detail
	}
	return transporterr.Classify(err)
}

// nzbRequest is the JSON-RPC request envelope.
type nzbRequest struct {
	Method string `json:"method"`
	Params []any  `json:"params"`
	ID     int    `json:"id"`
}

// nzbResponse is the JSON-RPC response envelope. NZBGet answers a good call with
// a `result` and a bad one with an `error` object; a wrong credential is caught
// earlier, as a 401 on the HTTP layer.
type nzbResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// call issues one JSON-RPC request and returns the raw `result`.
func (t *nzbTransport) call(ctx context.Context, method string, params []any) (json.RawMessage, error) {
	payload, err := json.Marshal(nzbRequest{Method: method, Params: params, ID: 1})
	if err != nil {
		return nil, &nzbError{detail: "encoding the request", err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+"/jsonrpc", bytes.NewReader(payload))
	if err != nil {
		return nil, &nzbError{detail: "building the request", err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	if t.user != "" || t.pass != "" {
		req.SetBasicAuth(t.user, t.pass)
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, &nzbError{detail: "reaching nzbget", err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, &nzbError{detail: "reading the response", err: err}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrUnauthorised
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &nzbError{detail: fmt.Sprintf("nzbget returned status %d", resp.StatusCode)}
	}
	var env nzbResponse
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, &nzbError{detail: "decoding the response", err: err}
	}
	if env.Error != nil {
		msg := strings.TrimSpace(env.Error.Message)
		if msg == "" {
			msg = fmt.Sprintf("nzbget error %d", env.Error.Code)
		}
		return nil, &nzbError{detail: msg}
	}
	return env.Result, nil
}
