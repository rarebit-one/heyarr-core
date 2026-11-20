package crdt

// composite_snapshot.go is ADR-0113 §6's composite snapshot: the one snapshot a
// space that holds more than one kind may carry. The server keeps one latest
// snapshot per space and compacts against it (§44), so a snapshot holding one kind
// would let compaction strand the others. A composite therefore carries
//
//   - the snapshot of every kind its writer folds, verbatim (each kind's own
//     deterministic Snapshot bytes), keyed by the kind's `k` value; and
//   - verbatim the plaintext of every change its writer could not fold (another
//     kind, or an op its build does not know);
//
// and it is taken at all the space's heads. A reader takes its own kind's state and
// replays the verbatim changes of kinds it knows; nothing is lost whichever client
// wrote it. The wire shape is pinned by testdata/vectors/composite.json:
//
//	{"k":"composite","v":1,"states":{"<kind>":<that kind's snapshot>,…},"verbatim":["<base64 plaintext>",…]}
//
// `states` is ordered by kind (encoding/json sorts map keys), and `verbatim` by the
// plaintext bytes, so one writer folding one log always produces the same bytes.
// Both are always present, empty rather than null.
//
// A composite is NOT any single kind's snapshot: each kind's FromSnapshot refuses
// one ([ErrCompositeSnapshot]) instead of reading its unknown fields as an empty
// state, which would hide everything compaction removed from the log.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// KindComposite is the `k` tag a composite snapshot carries.
const KindComposite = "composite"

// compositeVersion is the composite's wire version; a reader refuses another.
const compositeVersion = 1

// ErrCompositeSnapshot is a single-kind snapshot reader handed a composite.
var ErrCompositeSnapshot = errors.New("crdt: a composite snapshot is not a single kind's snapshot (ADR-0113)")

// Composite is a decoded composite snapshot.
type Composite struct {
	// States holds each folded kind's own snapshot bytes, keyed by its `k`.
	States map[string][]byte
	// Verbatim holds the plaintext of every change the writer could not fold.
	Verbatim [][]byte
}

type compositeWire struct {
	Kind     string                     `json:"k"`
	Version  int                        `json:"v"`
	States   map[string]json.RawMessage `json:"states"`
	Verbatim [][]byte                   `json:"verbatim"`
}

// Snapshot serialises the composite deterministically.
func (c Composite) Snapshot() ([]byte, error) {
	w := compositeWire{
		Kind:     KindComposite,
		Version:  compositeVersion,
		States:   make(map[string]json.RawMessage, len(c.States)),
		Verbatim: make([][]byte, 0, len(c.Verbatim)),
	}
	for k, s := range c.States {
		if k == "" || k == KindComposite {
			return nil, fmt.Errorf("crdt: a composite cannot hold a state of kind %q", k)
		}
		if !json.Valid(s) {
			return nil, fmt.Errorf("crdt: the %s state in a composite is not JSON", k)
		}
		w.States[k] = json.RawMessage(s)
	}
	w.Verbatim = append(w.Verbatim, c.Verbatim...)
	sort.Slice(w.Verbatim, func(i, j int) bool { return bytes.Compare(w.Verbatim[i], w.Verbatim[j]) < 0 })
	return json.Marshal(w)
}

// IsCompositeSnapshot reports whether b is tagged as a composite snapshot.
func IsCompositeSnapshot(b []byte) bool {
	var probe struct {
		Kind string `json:"k"`
	}
	return json.Unmarshal(b, &probe) == nil && probe.Kind == KindComposite
}

// CompositeFromSnapshot decodes a composite snapshot, refusing one of another
// version or a body that is not a composite.
func CompositeFromSnapshot(b []byte) (Composite, error) {
	var w compositeWire
	if err := json.Unmarshal(b, &w); err != nil {
		return Composite{}, fmt.Errorf("crdt: decoding a composite snapshot: %w", err)
	}
	if w.Kind != KindComposite {
		return Composite{}, fmt.Errorf("crdt: not a composite snapshot (k=%q)", w.Kind)
	}
	if w.Version != compositeVersion {
		return Composite{}, fmt.Errorf("crdt: composite snapshot version %d is not %d", w.Version, compositeVersion)
	}
	c := Composite{States: make(map[string][]byte, len(w.States)), Verbatim: w.Verbatim}
	for k, s := range w.States {
		c.States[k] = []byte(s)
	}
	return c, nil
}

// refuseComposite is the guard each single-kind FromSnapshot runs first.
func refuseComposite(b []byte) error {
	if IsCompositeSnapshot(b) {
		return ErrCompositeSnapshot
	}
	return nil
}
