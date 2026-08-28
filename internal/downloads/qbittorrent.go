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
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The qBittorrent download client, behind the provider registry's Downloader
// contract (§58, §59, M11). A second real torrent client beside Transmission.
//
// # What it shares with the Transmission client, and why
//
// The safety and identity decisions are the same because they are the
// contract's, not the protocol's: a download client is SHARED, so every
// mutating operation filters on a label first (here a qBittorrent CATEGORY,
// which is that client's native version of the same idea); identity is the
// INFOHASH, never the name; and a version outside the supported range is
// reported unhealthy naming both numbers rather than as a startup failure
// (ADR-0025). What differs is only the wire: qBittorrent authenticates with a
// login that mints a session cookie, and its `torrents/add` answers "Ok."
// without naming the transfer, so identity comes from the magnet's infohash (or
// a category diff for a .torrent) rather than from the add response.
//
// # It refuses what is not its to take
//
// Add refuses a source that is not a magnet or a .torrent, which is what lets it
// compose with the plain-HTTP and usenet clients through registry.Grab's
// fall-through — the same discipline the HTTP client applies from the other
// side. A torrent client that accepted an .nzb would silently swallow a transfer
// that belonged to a usenet client.
//
// # Its live exercise is opt-in, never a daemon in CI (ADR-0026)
//
// qBittorrent is an operator-managed service Heyarr targets by configuration; it
// is not installed and so not pinned or run in CI. The merge path tests it
// against a fake of its Web API; the real exercise is TestLiveQBittorrent,
// pointed at whatever instance you have and skipped when unset.

// qbMinAPIVersion is the oldest qBittorrent Web API this client will drive.
//
// 2.0 is qBittorrent 4.1, where the v2 API this client speaks was introduced.
// Below it the endpoints here do not exist in the shape expected, and a version
// below this is reported UNHEALTHY NAMING BOTH NUMBERS, never a startup failure.
const qbMinAPIVersion = "2.0"

// QBOptions configure a qBittorrent client. It mirrors Options so the two
// clients are configured the same way by construct.go.
type QBOptions struct {
	Name         string
	Endpoint     string
	Username     string
	Password     string
	PathMap      PathMap
	Label        string
	Capabilities []providers.Capability
	// HTTPClient is injectable so tests drive the real transport against a fake
	// of the Web API rather than a stub of the client.
	HTTPClient *http.Client
	Now        func() time.Time
}

// QBClient drives one qBittorrent instance.
type QBClient struct {
	name    string
	caps    []providers.Capability
	pathMap PathMap
	label   string
	now     func() time.Time
	api     *qbTransport

	version string // the app version the last Check learnt, for Session-style reads.
}

// NewQBittorrent builds a qBittorrent client, refusing a mis-wired one.
func NewQBittorrent(opts QBOptions) (*QBClient, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return nil, errors.New("downloads: a qbittorrent client needs a provider name")
	}
	if strings.TrimSpace(opts.Endpoint) == "" {
		return nil, errors.New("downloads: a qbittorrent client needs an endpoint")
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
	return &QBClient{
		name:    strings.TrimSpace(opts.Name),
		caps:    caps,
		pathMap: opts.PathMap,
		label:   label,
		now:     now,
		api: &qbTransport{
			base: strings.TrimRight(strings.TrimSpace(opts.Endpoint), "/"),
			user: opts.Username,
			pass: opts.Password,
			http: httpc,
		},
	}, nil
}

var _ providers.Downloader = (*QBClient)(nil)

// Name implements providers.Provider.
func (c *QBClient) Name() string { return c.name }

// Capabilities implements providers.Provider.
func (c *QBClient) Capabilities() []providers.Capability {
	return append([]providers.Capability(nil), c.caps...)
}

// Label is what this client tags (categorises) and recognises its transfers by.
func (c *QBClient) Label() string { return c.label }

// Check exercises the instance and reports what it found (ADR-0025).
//
// It logs in and reads the app and Web-API versions — a real round trip that
// returns real facts, so a client cannot report itself healthy merely because it
// was configured. An unsupported Web-API version is reported unhealthy naming
// both numbers, the analogue of ADR-0023's version probe for a service Heyarr
// does not install.
func (c *QBClient) Check(ctx context.Context) providers.Health {
	at := c.now()

	apiVer, err := c.api.text(ctx, "/api/v2/app/webapiVersion")
	if err != nil {
		if errors.Is(err, ErrUnauthorised) {
			return providers.Unhealthy(
				"the credential was refused — check the username and password", at)
		}
		return providers.Unhealthy(qbShort(err), at)
	}
	appVer, err := c.api.text(ctx, "/api/v2/app/version")
	if err != nil {
		return providers.Unhealthy(qbShort(err), at)
	}
	c.version = appVer

	if compareDotted(apiVer, qbMinAPIVersion) < 0 {
		return providers.Unhealthy(fmt.Sprintf(
			"Web API version %s is below the minimum this client supports (%s); "+
				"transfers will not be driven until qBittorrent is upgraded",
			apiVer, qbMinAPIVersion), at)
	}
	return providers.Healthy(appVer, at)
}

// ErrNotTorrentSource is the refusal that lets this client compose with the
// HTTP and usenet clients: a source it will not take falls through to whichever
// client will. It mirrors the HTTP client's ErrNotHTTPSource from the other side.
var ErrNotTorrentSource = errors.New("downloads: not a torrent source (magnet or .torrent)")

// isTorrentSource reports whether this client should take a source. A magnet URI
// or a reference ending in .torrent is a torrent; everything else — a direct
// http(s) file, an .nzb — belongs to another client.
func isTorrentSource(raw string) bool {
	lower := strings.ToLower(strings.TrimSpace(raw))
	if strings.HasPrefix(lower, "magnet:") {
		return true
	}
	// Strip a query so `https://host/x.torrent?passkey=…` still reads as one.
	if i := strings.IndexByte(lower, '?'); i >= 0 {
		lower = lower[:i]
	}
	return strings.HasSuffix(lower, ".torrent")
}

// Add queues a release, tagged with this client's category.
//
// qBittorrent's add answers only "Ok.", so the transfer is resolved afterwards:
// by the magnet's infohash when there is one, and otherwise by the single new
// hash that appeared in the category. Idempotent by construction — re-adding a
// magnet qBittorrent already holds is a no-op, and the resolve returns the same
// transfer either way, so a re-run (invariant 9) is indistinguishable from the
// first call.
func (c *QBClient) Add(ctx context.Context, source secret.Value) (providers.Transfer, error) {
	// Reveal() only here and nowhere else in this method: on a private tracker a
	// magnet carries a passkey identifying a person, and it must not reach a log
	// line or the error below.
	raw := strings.TrimSpace(source.Reveal())
	if raw == "" {
		return providers.Transfer{}, errors.New("downloads: nothing to add")
	}
	if !isTorrentSource(raw) {
		return providers.Transfer{}, ErrNotTorrentSource
	}

	hash := magnetInfoHash(raw)

	// For a .torrent (no infohash in hand), snapshot the category first so the
	// one that appears after the add can be told from what was already there.
	var before map[string]struct{}
	if hash == "" {
		existing, err := c.ours(ctx)
		if err != nil {
			return providers.Transfer{}, err
		}
		before = make(map[string]struct{}, len(existing))
		for _, t := range existing {
			before[strings.ToLower(t.Hash)] = struct{}{}
		}
	}

	if err := c.addTorrent(ctx, raw); err != nil {
		return providers.Transfer{}, err
	}

	if hash != "" {
		if t, ok := c.byHash(ctx, hash); ok {
			return c.toTransfer(t), nil
		}
		// Accepted but not yet registered in the queue. The caller gets a
		// transfer keyed by the infohash it will carry, and the next poll fills
		// in the rest — the hash is stable, so nothing is lost.
		return providers.Transfer{ID: hash, Name: magnetName(raw)}, nil
	}

	// The .torrent path: return the new arrival in our category.
	after, err := c.ours(ctx)
	if err != nil {
		return providers.Transfer{}, err
	}
	for _, t := range after {
		if _, seen := before[strings.ToLower(t.Hash)]; !seen {
			return c.toTransfer(t), nil
		}
	}
	return providers.Transfer{}, fmt.Errorf(
		"%w: qBittorrent accepted the torrent but it did not appear in the queue", ErrRPCFailure)
}

// Transfers is everything this client is doing that BELONGS TO HEYARR.
//
// The category is the filter, and it is the point: a download client is shared,
// and a transfer without our category is invisible to everything above — not
// merely excluded from mutation, but absent, so no caller can act on one it was
// never shown.
func (c *QBClient) Transfers(ctx context.Context) ([]providers.Transfer, error) {
	ours, err := c.ours(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]providers.Transfer, 0, len(ours))
	for _, t := range ours {
		out = append(out, c.toTransfer(t))
	}
	return out, nil
}

// Remove takes a transfer out of the client, refusing anything that is not ours.
//
// The id is looked up in our filtered view first: a caller handing a foreign
// infohash — from a stale row, a bug, anywhere — gets a refusal rather than a
// removal. deleteData is separate because "stop tracking" and "delete the bytes"
// are different decisions, and removal must never delete bytes Heyarr has not
// yet ingested.
func (c *QBClient) Remove(ctx context.Context, id string, deleteData bool) error {
	ours, err := c.ours(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, t := range ours {
		if strings.EqualFold(t.Hash, id) {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: %s is not a transfer Heyarr queued", ErrNotOurs, id)
	}
	form := url.Values{
		"hashes":      {strings.ToLower(id)},
		"deleteFiles": {fmt.Sprintf("%t", deleteData)},
	}
	_, err = c.api.post(ctx, "/api/v2/torrents/delete", form)
	return err
}

// qbTorrent is one entry from torrents/info. Only the fields the pipeline reads
// are named; they are qBittorrent's and change when it changes.
type qbTorrent struct {
	Hash        string  `json:"hash"`
	Name        string  `json:"name"`
	State       string  `json:"state"`
	Progress    float64 `json:"progress"`
	Size        int64   `json:"size"`
	Completed   int64   `json:"completed"`
	Downloaded  int64   `json:"downloaded"`
	SavePath    string  `json:"save_path"`
	ContentPath string  `json:"content_path"`
	Category    string  `json:"category"`
}

// ours reads the queue filtered to this client's category.
func (c *QBClient) ours(ctx context.Context) ([]qbTorrent, error) {
	body, err := c.api.get(ctx, "/api/v2/torrents/info", url.Values{"category": {c.label}})
	if err != nil {
		return nil, err
	}
	var torrents []qbTorrent
	if err := json.Unmarshal(body, &torrents); err != nil {
		return nil, &qbError{detail: "decoding torrents/info", err: err}
	}
	// qBittorrent honours the category filter server-side, but a defensive
	// re-check keeps a broken filter from ever exposing a foreign transfer.
	out := torrents[:0]
	for _, t := range torrents {
		if strings.EqualFold(strings.TrimSpace(t.Category), c.label) {
			out = append(out, t)
		}
	}
	return out, nil
}

// byHash returns the transfer with the given infohash if it is ours yet.
func (c *QBClient) byHash(ctx context.Context, hash string) (qbTorrent, bool) {
	ours, err := c.ours(ctx)
	if err != nil {
		return qbTorrent{}, false
	}
	for _, t := range ours {
		if strings.EqualFold(t.Hash, hash) {
			return t, true
		}
	}
	return qbTorrent{}, false
}

// addTorrent posts one source to torrents/add, tagged with our category.
func (c *QBClient) addTorrent(ctx context.Context, source string) error {
	form := url.Values{
		"urls":     {source},
		"category": {c.label},
	}
	body, err := c.api.post(ctx, "/api/v2/torrents/add", form)
	if err != nil {
		return err
	}
	// qBittorrent answers "Ok." on success and "Fails." when it could not parse
	// the source. The status was 200 either way, so the body is the real result.
	if strings.EqualFold(strings.TrimSpace(string(body)), "fails.") {
		return fmt.Errorf("%w: qBittorrent could not accept the source", ErrRPCFailure)
	}
	return nil
}

// qbDone reports a finished transfer from qBittorrent's state vocabulary. The
// *UP states are seeding (the download is complete); progress is the fallback.
func qbDone(t qbTorrent) bool {
	if t.Progress >= 1 {
		return true
	}
	switch t.State {
	case "uploading", "stalledUP", "forcedUP", "queuedUP", "pausedUP", "checkingUP":
		return true
	}
	return false
}

// toTransfer reduces qBittorrent's shape to the registry's value type.
func (c *QBClient) toTransfer(t qbTorrent) providers.Transfer {
	done := qbDone(t)
	out := providers.Transfer{
		ID:         strings.ToLower(t.Hash),
		Name:       t.Name,
		Done:       done,
		BytesTotal: t.Size,
		BytesDone:  t.Completed,
	}
	if out.BytesDone == 0 && t.Downloaded > 0 {
		out.BytesDone = t.Downloaded
	}
	switch t.State {
	case "error", "missingFiles":
		out.Error = "qBittorrent reported state " + t.State
	}
	// The path is resolved only on completion: before then content_path may name
	// an incomplete file that ingest cannot open, which would read as an ingest
	// bug rather than a timing one.
	if done {
		out.Path = c.resolvePath(t)
	}
	return out
}

// resolvePath translates qBittorrent's path into one Heyarr can open. An
// unmapped path is returned as-is, the right default for the common single-host
// deployment where the two share a filesystem.
func (c *QBClient) resolvePath(t qbTorrent) string {
	full := strings.TrimSpace(t.ContentPath)
	if full == "" {
		full = path.Join(path.Clean(t.SavePath), t.Name)
	}
	if mapped, ok := c.pathMap.Resolve(full); ok {
		return mapped
	}
	return full
}

// magnetInfoHash extracts the btih infohash from a magnet URI, lower-cased, or
// "" when the source is not a magnet or carries no v1 infohash.
func magnetInfoHash(raw string) string {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "magnet:") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	for _, xt := range u.Query()["xt"] {
		const prefix = "urn:btih:"
		if strings.HasPrefix(strings.ToLower(xt), prefix) {
			h := strings.ToLower(strings.TrimSpace(xt[len(prefix):]))
			// A 40-char hex (v1) infohash is what qBittorrent keys on. A base32
			// or v2 hash is left unresolved rather than guessed.
			if len(h) == 40 && isHex(h) {
				return h
			}
		}
	}
	return ""
}

// magnetName reads the display name from a magnet URI, for a human reading a
// queue before the client has reported the real name.
func magnetName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Query().Get("dn")
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return len(s) > 0
}

// compareDotted compares two dotted version strings numerically, component by
// component. It returns -1, 0 or 1. A non-numeric component compares as 0, which
// keeps a "2.9.1beta"-shaped tail from being read as older than "2.9.1".
func compareDotted(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var an, bn int
		if i < len(as) {
			an = atoiPrefix(as[i])
		}
		if i < len(bs) {
			bn = atoiPrefix(bs[i])
		}
		if an != bn {
			if an < bn {
				return -1
			}
			return 1
		}
	}
	return 0
}

// atoiPrefix reads the leading run of digits as an int, ignoring any suffix.
func atoiPrefix(s string) int {
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// --- transport --------------------------------------------------------------

// qbTransport speaks the qBittorrent Web API: a login that mints a session
// cookie, then requests carrying it. It re-logs in once on a 403 so an expired
// session recovers without the caller knowing.
type qbTransport struct {
	base string
	user string
	pass string
	http *http.Client
	sid  string
}

// qbError is a qBittorrent call that failed in a way a caller may need to read.
type qbError struct {
	detail string
	err    error
}

func (e *qbError) Error() string {
	if e.err != nil {
		return "qbittorrent: " + e.detail + ": " + e.err.Error()
	}
	return "qbittorrent: " + e.detail
}

func (e *qbError) Unwrap() error { return e.err }

// qbShort renders an error for a health detail — a few words, never a
// credential, and never a Go error chain rendered in full.
func qbShort(err error) string {
	var qe *qbError
	if errors.As(err, &qe) {
		return qe.detail
	}
	return "unreachable"
}

// login mints a session cookie from the operator's credential.
func (t *qbTransport) login(ctx context.Context) error {
	form := url.Values{"username": {t.user}, "password": {t.pass}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.base+"/api/v2/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return &qbError{detail: "building the login request", err: err}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// qBittorrent checks Referer against its own host to blunt CSRF; without it
	// some builds answer 403 to an otherwise valid login.
	req.Header.Set("Referer", t.base)

	resp, err := t.http.Do(req)
	if err != nil {
		return &qbError{detail: "reaching the login endpoint", err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode == http.StatusForbidden:
		return ErrUnauthorised
	case resp.StatusCode != http.StatusOK:
		return &qbError{detail: fmt.Sprintf("login returned status %d", resp.StatusCode)}
	case strings.EqualFold(strings.TrimSpace(string(body)), "fails."):
		return ErrUnauthorised
	}
	// Capture the SID cookie. An auth-bypassed instance (a localhost whitelist)
	// answers "Ok." with no cookie, which is fine: the requests below then carry
	// none and succeed anyway.
	for _, ck := range resp.Cookies() {
		if strings.EqualFold(ck.Name, "SID") {
			t.sid = ck.Value
		}
	}
	return nil
}

// do issues one request, logging in first if a credential is configured and no
// session is held, and re-logging in once on a 403.
func (t *qbTransport) do(ctx context.Context, method, endpoint string, query, form url.Values) ([]byte, error) {
	if t.sid == "" && (t.user != "" || t.pass != "") {
		if err := t.login(ctx); err != nil {
			return nil, err
		}
	}
	body, status, err := t.roundTrip(ctx, method, endpoint, query, form)
	if err != nil {
		return nil, err
	}
	if status == http.StatusForbidden {
		// Session expired, or an auth-bypassed instance that nonetheless refused
		// this route. Re-login once and retry; a second 403 is a real refusal.
		if err := t.login(ctx); err != nil {
			return nil, err
		}
		body, status, err = t.roundTrip(ctx, method, endpoint, query, form)
		if err != nil {
			return nil, err
		}
		if status == http.StatusForbidden {
			return nil, ErrUnauthorised
		}
	}
	if status != http.StatusOK {
		return nil, &qbError{detail: fmt.Sprintf("%s returned status %d", endpoint, status)}
	}
	return body, nil
}

func (t *qbTransport) roundTrip(ctx context.Context, method, endpoint string, query, form url.Values) ([]byte, int, error) {
	u := t.base + endpoint
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var bodyReader io.Reader
	if form != nil {
		bodyReader = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
	if err != nil {
		return nil, 0, &qbError{detail: "building the request", err: err}
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Referer", t.base)
	if t.sid != "" {
		// The Secure/HttpOnly/SameSite attributes gosec wants are for cookies a
		// SERVER sets in a response; this is a request cookie the client sends to
		// qBittorrent, where they have no meaning and Go would not transmit them.
		//nolint:gosec // G124: request cookie, not a Set-Cookie — those attributes do not apply
		req.AddCookie(&http.Cookie{Name: "SID", Value: t.sid})
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, 0, &qbError{detail: "reaching " + endpoint, err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, &qbError{detail: "reading " + endpoint, err: err}
	}
	return body, resp.StatusCode, nil
}

func (t *qbTransport) get(ctx context.Context, endpoint string, query url.Values) ([]byte, error) {
	return t.do(ctx, http.MethodGet, endpoint, query, nil)
}

func (t *qbTransport) post(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	return t.do(ctx, http.MethodPost, endpoint, nil, form)
}

// text reads a plain-text endpoint (the version routes), trimmed.
func (t *qbTransport) text(ctx context.Context, endpoint string) (string, error) {
	body, err := t.get(ctx, endpoint, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}
