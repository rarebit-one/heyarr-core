package events

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The vocabulary tests read the constants out of events.go rather than
// restating them, because a hand-written list of every event type is a second
// copy of the vocabulary and second copies drift. Enumerating means a type
// added tomorrow is held to these rules on the day it lands, with nobody
// remembering to add it here.

type eventTypeConst struct {
	name  string
	value string
	doc   string
	line  int
}

// eventTypeConstants parses events.go and returns every exported Type* string
// constant declared in it, with the comment written above it.
func eventTypeConstants(t *testing.T) []eventTypeConst {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "events.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing events.go: %v", err)
	}

	var out []eventTypeConst
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			name := vs.Names[0].Name
			if !strings.HasPrefix(name, "Type") {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("%s has an unparseable value %s: %v", name, lit.Value, err)
			}
			var doc string
			if vs.Doc != nil {
				doc = vs.Doc.Text()
			}
			out = append(out, eventTypeConst{
				name:  name,
				value: value,
				doc:   doc,
				line:  fset.Position(vs.Pos()).Line,
			})
		}
	}
	if len(out) < 30 {
		t.Fatalf("only found %d event type constants — the parser is probably not seeing the const block", len(out))
	}
	return out
}

func values(consts []eventTypeConst) []string {
	out := make([]string, 0, len(consts))
	for _, c := range consts {
		out = append(out, c.value)
	}
	return out
}

// No two constants may name the same event type. This is the check that a
// milestone reserving vocabulary up front exists to pass: six issues adding
// their own constants would eventually add the same string twice under two
// names, and a subscriber filtering on one would silently miss the other.
func TestNoEventTypeIsDeclaredTwice(t *testing.T) {
	seen := map[string]string{}
	for _, c := range eventTypeConstants(t) {
		if first, dup := seen[c.value]; dup {
			t.Errorf("%q is declared by both %s and %s", c.value, first, c.name)
			continue
		}
		seen[c.value] = c.name
	}
}

// The Milestone 4 reservation itself: these types exist, spelled exactly this
// way, and each carries the explanation that is this file's house style. The
// names are written out here because reserving them IS the change under test —
// everything else in this file is enumerated instead.
func TestThePeerPlaneVocabularyIsReserved(t *testing.T) {
	byName := map[string]eventTypeConst{}
	for _, c := range eventTypeConstants(t) {
		byName[c.name] = c
	}

	reserved := []struct{ name, value string }{
		// Already existed; Milestone 4 extends them rather than replacing them.
		{"TypePeerRegistered", "peer.registered"},
		{"TypeReplicaPresent", "replica.present"},
		{"TypeReplicaCorrupt", "replica.corrupt"},
		{"TypeReplicaMissing", "replica.missing"},
		// New in this change.
		{"TypePeerRemoved", "peer.removed"},
		{"TypePeerHealthChanged", "peer.health_changed"},
		{"TypeReplicationTransferChanged", "replication.transfer_changed"},
		{"TypeSyncInventoryReported", "sync.inventory_reported"},
		{"TypeSyncReconciled", "sync.reconciled"},
		{"TypeCatalogSnapshotBuilt", "catalog.snapshot_built"},
	}

	for _, want := range reserved {
		got, ok := byName[want.name]
		if !ok {
			t.Errorf("%s is not declared", want.name)
			continue
		}
		if got.value != want.value {
			t.Errorf("%s = %q, want %q", want.name, got.value, want.value)
		}
	}

	// Every type this change introduces must say why it exists — and, in the
	// house style of this file, why a neighbouring type does not. A bare
	// constant is how the next person reintroduces peer.up/peer.down.
	newlyReserved := []string{
		"TypePeerRemoved",
		"TypePeerHealthChanged",
		"TypeReplicationTransferChanged",
		"TypeSyncInventoryReported",
		"TypeSyncReconciled",
		"TypeCatalogSnapshotBuilt",
	}
	for _, name := range newlyReserved {
		c, ok := byName[name]
		if !ok {
			continue // already reported above
		}
		if len(strings.TrimSpace(c.doc)) < 120 {
			t.Errorf("%s has no explanatory comment (got %d characters); "+
				"this file's style is to say why the type exists and why a neighbouring one does not",
				name, len(strings.TrimSpace(c.doc)))
		}
	}
}

// peerPlaneNamespaces are the namespaces this milestone reserves. The rules
// below apply to these only: job.succeeded and system.started predate the
// one-type-per-machine convention and are not being relitigated here.
var peerPlaneNamespaces = []string{"peer.", "replica.", "replication.", "sync.", "catalog."}

func inPeerPlane(value string) bool {
	for _, ns := range peerPlaneNamespaces {
		if strings.HasPrefix(value, ns) {
			return true
		}
	}
	return false
}

// One event type per state machine, with the transition in the payload — the
// argument events.go makes for acquisition.phase_changed. A type named after
// one EDGE of a machine is the failure mode: N edges become N places to forget
// to emit, which is what invariant 7 exists to prevent.
//
// Enumerated rather than listed, so a peer.up added next month fails here.
func TestNoPeerPlaneTypeIsNamedAfterASingleEdge(t *testing.T) {
	edgeWords := []string{
		"up", "down", "online", "offline", "reachable", "unreachable",
		"healthy", "unhealthy", "connected", "disconnected",
		"started", "starting", "stopped", "succeeded", "failed", "completed",
		"finished", "begun", "aborted", "cancelled", "canceled", "errored",
	}

	for _, c := range eventTypeConstants(t) {
		if !inPeerPlane(c.value) {
			continue
		}
		leaf := c.value
		if i := strings.LastIndex(leaf, "."); i >= 0 {
			leaf = leaf[i+1:]
		}
		for _, word := range edgeWords {
			if leaf == word || strings.HasSuffix(leaf, "_"+word) {
				t.Errorf("%s = %q names a single edge (%q); one type per state machine, "+
					"with the transition in the payload (events.go:%d)", c.name, c.value, word, c.line)
			}
		}
	}
}

// No event fires per blob during routine replication work. There is no
// blob.verified for the same reason; a first sync of a large library must not
// produce one event per blob per state change. The one deliberate exception is
// a transfer, which is a discrete unit of queued work — see "No per-blob events
// during replication" in the package doc.
func TestNoPeerPlaneTypeFiresPerBlob(t *testing.T) {
	const transferException = "replication.transfer_changed"

	for _, c := range eventTypeConstants(t) {
		if !inPeerPlane(c.value) || c.value == transferException {
			continue
		}
		// replica.* describes one blob's replica, but it is emitted on a state
		// CHANGE (present/corrupt/missing), never per blob during a sweep.
		if strings.HasPrefix(c.value, "replica.") {
			continue
		}
		for _, perItem := range []string{"blob", "item", "each", "per_", "chunk", "file"} {
			if strings.Contains(c.value, perItem) {
				t.Errorf("%s = %q looks like a per-blob event; replication reports cycles and "+
					"transfers, and per-blob facts belong in the replicas table (events.go:%d)",
					c.name, c.value, c.line)
			}
		}
	}
}

// The wildcard filter is what a subscriber actually uses, so it is asserted
// through a real subscription per pattern rather than by inspecting matchType:
// matchType agreeing with itself proves nothing about what arrives on a
// channel. Every declared type is emitted once, and each pattern must receive
// exactly its namespace and nothing else.
//
// The trap this exists to catch is replica.* against replication.*: they share
// seven characters, and a prefix match on "replica" rather than "replica."
// would quietly hand a replica subscriber every transfer in the fabric.
func TestWildcardSubscriptionsSelectExactlyTheirNamespace(t *testing.T) {
	all := values(eventTypeConstants(t))
	patterns := []string{"peer.*", "replica.*", "replication.*", "sync.*", "catalog.*"}

	l := newLog(t)

	subs := map[string]*Subscription{}
	for _, p := range patterns {
		sub := l.Subscribe(len(all)+8, p)
		defer sub.Close()
		subs[p] = sub
	}

	for _, typ := range all {
		if _, err := l.Emit(t.Context(), typ, "subject", "s", nil); err != nil {
			t.Fatalf("Emit(%q): %v", typ, err)
		}
	}

	for _, p := range patterns {
		prefix := strings.TrimSuffix(p, "*")
		var want []string
		for _, typ := range all {
			if strings.HasPrefix(typ, prefix) {
				want = append(want, typ)
			}
		}
		if len(want) == 0 {
			t.Errorf("%s selects nothing at all — the namespace it reserves is not declared", p)
			continue
		}

		sub := subs[p]
		if n := sub.Dropped(); n != 0 {
			t.Errorf("%s dropped %d events; the buffer is too small for this test to mean anything", p, n)
		}
		var got []string
	drain:
		for {
			select {
			case e := <-sub.Events():
				got = append(got, e.Type)
			default:
				break drain
			}
		}

		sort.Strings(got)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Errorf("subscription %q received %v, want %v", p, got, want)
		}
	}
}

// The replayed history must select the same set as the live stream, or a
// client that reconnects with ?after= sees a different world than the one it
// was watching. Same enumeration, same patterns, through the SQL filter.
func TestReplayedHistorySelectsTheSameNamespacesAsTheLiveStream(t *testing.T) {
	all := values(eventTypeConstants(t))
	l := newLog(t)

	for _, typ := range all {
		if _, err := l.Emit(t.Context(), typ, "subject", "s", nil); err != nil {
			t.Fatalf("Emit(%q): %v", typ, err)
		}
	}

	for _, p := range []string{"peer.*", "replica.*", "replication.*", "sync.*", "catalog.*"} {
		prefix := strings.TrimSuffix(p, "*")
		var want []string
		for _, typ := range all {
			if strings.HasPrefix(typ, prefix) {
				want = append(want, typ)
			}
		}

		evs, err := l.Since(t.Context(), 0, []string{p}, len(all)+8)
		if err != nil {
			t.Fatalf("Since(%q): %v", p, err)
		}
		var got []string
		for _, e := range evs {
			got = append(got, e.Type)
		}

		sort.Strings(got)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Errorf("Since(%q) returned %v, want %v", p, got, want)
		}
	}
}
