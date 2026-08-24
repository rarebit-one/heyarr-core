package downloads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Fake is a download client that moves no bytes over a network but DOES write
// real ones to disk.
//
// # Why this is production code rather than a test fixture
//
// The same argument as providers.Fake: the acceptance demo has to prove the
// whole arc — decide, search, select, acquire, verify, ingest — on a machine
// with no download client, and the only way to do that is something that
// behaves exactly like one and needs no daemon.
//
// Putting it in a _test.go file would mean the demo could not reach it, and the
// demo is the thing that proves the claim on a real machine over a real socket.
// Here, the demo exercises the same registration, routing, health, label and
// path-mapping paths as production.
//
// # It writes real bytes, and that is the point
//
// A fake that reported "done" without producing a file would let ingest be
// tested against a fiction — and ingest's whole job is to bring bytes under
// management. So Complete() writes the content it was given into the download
// directory, and everything downstream is dealing with a real file on a real
// filesystem, hardlinkable and hashable like any other.
type Fake struct {
	name string
	// dir is where completed transfers are written. Real, and the caller's to
	// clean up.
	dir   string
	label string
	now   func() time.Time

	mu        sync.Mutex
	transfers map[string]*fakeTransfer
	// offers is what each source produces when grabbed — see Offer.
	offers map[string]fakeOffer
}

type fakeOffer struct {
	name    string
	content []byte
}

type fakeTransfer struct {
	transfer providers.Transfer
	content  []byte
}

// NewFake builds a fake download client writing into dir.
func NewFake(name, dir string) *Fake {
	return &Fake{
		name:      name,
		dir:       dir,
		label:     DefaultLabel,
		now:       func() time.Time { return time.Now().UTC() },
		transfers: map[string]*fakeTransfer{},
		offers:    map[string]fakeOffer{},
	}
}

var _ providers.Downloader = (*Fake)(nil)

// Name implements providers.Provider.
func (f *Fake) Name() string { return f.name }

// Capabilities implements providers.Provider.
func (f *Fake) Capabilities() []providers.Capability {
	return []providers.Capability{providers.CapabilityDownload}
}

// Check is always healthy: there is nothing that could be unwell.
//
// It reports a version so that the health shape a caller sees is the same one a
// real client produces — a fake that left it empty would let a bug in the
// version reporting hide behind "the fake does not have one".
func (f *Fake) Check(_ context.Context) providers.Health {
	return providers.Healthy("fake", f.now())
}

// Transfers implements providers.Downloader.
func (f *Fake) Transfers(_ context.Context) ([]providers.Transfer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]providers.Transfer, 0, len(f.transfers))
	for _, t := range f.transfers {
		out = append(out, t.transfer)
	}
	sortTransfers(out)
	return out, nil
}

// Queue adds a transfer that has not started.
//
// content is what Complete will write. Passing it up front rather than at
// completion keeps the fake's API shaped like the real one — a caller adds a
// release and later observes it finish, rather than telling the client what the
// bytes should be at the moment it wants them to exist.
func (f *Fake) Queue(id, name string, content []byte) providers.Transfer {
	f.mu.Lock()
	defer f.mu.Unlock()

	t := providers.Transfer{
		ID: id, Name: name,
		BytesTotal: int64(len(content)),
	}
	f.transfers[id] = &fakeTransfer{transfer: t, content: content}
	return t
}

// Offer tells the fake what a given source produces when it is grabbed.
//
// # Why a grab needs seeding at all
//
// A real download client is handed a magnet and discovers the content from the
// swarm. A fake has no swarm, so the bytes have to come from somewhere, and the
// choice is between inventing them at Add (which makes every grab produce the
// same meaningless file) and being told in advance.
//
// Being told in advance is what keeps the demo honest: the content a want ends
// up holding is content the script chose and can assert against, rather than
// whatever the fake happened to generate.
//
// Offer is keyed on the SOURCE rather than on a transfer id because the source
// is all the grab has. That is the whole point of #225 — the id the search
// produced is a hash, and the source is the only thing that can be acted on.
func (f *Fake) Offer(source secret.Value, name string, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offers[sourceKey(source)] = fakeOffer{name: name, content: content}
}

// Add implements providers.Downloader — the fake's half of §64's
// SELECTED → QUEUED edge.
//
// # Idempotent, deliberately, like the real one
//
// downloads.Client.Add returns Transmission's existing transfer when it answers
// `torrent-duplicate`. A job that calls this WILL be re-run (invariant 9), so
// the fake has to have the same property or the demo would prove the grab
// works only on a queue that never retries. A second Add of the same source
// returns the transfer the first one created, unchanged.
//
// The transfer id is derived from the source, which is what makes that work and
// also mirrors reality: a real client's id for a torrent is its infohash, a
// function of the release rather than of when it was asked for.
func (f *Fake) Add(_ context.Context, source secret.Value) (providers.Transfer, error) {
	key := sourceKey(source)
	if key == "" {
		return providers.Transfer{}, errors.New("downloads: nothing to add")
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	id := "fake:" + key
	if existing, ok := f.transfers[id]; ok {
		return existing.transfer, nil
	}
	offer, ok := f.offers[key]
	if !ok {
		// Not echoed. The source is a credential (secret.Value), and an error
		// string is exactly the sort of place one ends up in a log. The id is
		// a digest and safe to name, which is enough to find the seeding bug.
		return providers.Transfer{}, fmt.Errorf(
			"downloads: this fake was not told what %s produces; call Offer first", id)
	}

	t := providers.Transfer{
		ID: id, Name: offer.name,
		BytesTotal: int64(len(offer.content)),
	}
	f.transfers[id] = &fakeTransfer{transfer: t, content: offer.content}
	return t, nil
}

// sourceKey is a stable, non-revealing identifier for a source.
//
// A digest rather than the value: this becomes a transfer ID, which reaches
// logs, the API and the demo's output, and the source may carry a passkey.
func sourceKey(source secret.Value) string {
	raw := strings.TrimSpace(source.Reveal())
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:16]
}

// Progress moves a transfer partway, without completing it.
//
// It leaves Path EMPTY, matching a real client with incomplete-dir enabled: the
// bytes are not where they will end up until the transfer finishes, so a
// mid-transfer path would be a lie.
func (f *Fake) Progress(id string, bytesDone int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	t, ok := f.transfers[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotOurs, id)
	}
	t.transfer.BytesDone = bytesDone
	return nil
}

// Complete finishes a transfer and WRITES THE BYTES.
//
// The file is real, so ingest hardlinks and hashes it exactly as it would a
// real acquisition. A fake that reported completion without producing a file
// would let the whole ingest path be tested against something that is not
// there.
func (f *Fake) Complete(id string) (providers.Transfer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	t, ok := f.transfers[id]
	if !ok {
		return providers.Transfer{}, fmt.Errorf("%w: %s", ErrNotOurs, id)
	}
	if err := os.MkdirAll(f.dir, 0o750); err != nil {
		return providers.Transfer{}, err
	}
	path := filepath.Join(f.dir, t.transfer.Name)
	if err := os.WriteFile(path, t.content, 0o600); err != nil {
		return providers.Transfer{}, err
	}

	t.transfer.Done = true
	t.transfer.BytesDone = t.transfer.BytesTotal
	t.transfer.Path = path
	return t.transfer, nil
}

// Fail marks a transfer as troubled.
//
// The reason is prefixed the way the real client's is, so a caller cannot come
// to depend on a shape only the fake produces.
func (f *Fake) Fail(id string, reason TroubleReason, detail string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	t, ok := f.transfers[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotOurs, id)
	}
	t.transfer.Error = string(reason) + ": " + detail
	return nil
}

// Remove drops a transfer, refusing one it does not hold.
//
// The same refusal the real client makes, for the same reason: a caller must
// not be able to reach past what this client queued.
func (f *Fake) Remove(_ context.Context, id string, deleteData bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	t, ok := f.transfers[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotOurs, id)
	}
	if deleteData && t.transfer.Path != "" {
		if err := os.Remove(t.transfer.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	delete(f.transfers, id)
	return nil
}

// Label reports what this client tags its transfers with.
func (f *Fake) Label() string { return f.label }

// Dir is where completed transfers are written.
func (f *Fake) Dir() string { return f.dir }

func sortTransfers(in []providers.Transfer) {
	// Stable and by id, so a caller reading the queue twice sees the same
	// order — the same reasoning as everywhere else a set reaches an operator.
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j].ID < in[j-1].ID; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}
