package downloads

import (
	"sync"

	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
)

// Programs posing as releases (#819).
//
// A fake release is named like the episode it claims to be and carries one
// Windows executable. Its title passes every profile gate, so nothing before
// the download notices; the file list is the first place the truth shows. Each
// client reads a transfer's file list ONCE, when it is known (a magnet's only
// arrives with its metadata), and remembers the verdict, so a poll over a
// queue of thousands asks only about the transfers it has not judged yet.

// maxFileListLookups bounds how many transfers one poll asks about, so a
// freshly started process with a long queue spreads the reads over a few
// passes instead of stalling the first one.
const maxFileListLookups = 25

// payloadVerdicts remembers, per transfer, whether every file it carries is a
// program. Safe for concurrent use; a nil value knows nothing.
type payloadVerdicts struct {
	mu    sync.Mutex
	known map[string]bool
}

func newPayloadVerdicts() *payloadVerdicts {
	return &payloadVerdicts{known: map[string]bool{}}
}

// verdict returns whether id is known to be all programs, and whether it has
// been judged at all.
func (p *payloadVerdicts) verdict(id string) (executable, judged bool) {
	if p == nil {
		return false, true // nothing to ask: never look a file list up
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	executable, judged = p.known[id]
	return executable, judged
}

// record stores id's verdict from its file names. An empty list is not a
// verdict (the client does not know the files yet), so nothing is stored.
func (p *payloadVerdicts) record(id string, names []string) {
	if p == nil || len(names) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.known[id] = allExecutable(names)
}

// keepOnly forgets every transfer not in ids: removed from the client, it
// will not be asked about again.
func (p *payloadVerdicts) keepOnly(ids map[string]bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for id := range p.known {
		if !ids[id] {
			delete(p.known, id)
		}
	}
}

// allExecutable reports whether every name is a program. A release that holds
// one real file beside an installer is not a fake by this measure, and ingest
// takes only its primary files anyway.
func allExecutable(names []string) bool {
	if len(names) == 0 {
		return false
	}
	for _, n := range names {
		if !identification.IsExecutableName(n) {
			return false
		}
	}
	return true
}
