package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// Tool is one semantic action (§71).
//
// The Scope field is the security contract and it is on the DESCRIPTOR rather
// than inside the handler, so that the whole authorisation surface can be read
// off one list. A handler that checked its own scope would put the contract in
// as many places as there are tools, and the one that forgot would be
// indistinguishable from one that deliberately needed no scope.
type Tool struct {
	// Name is the published vocabulary. Changing one breaks every agent built
	// against it, so these are chosen from §71 rather than invented.
	Name string
	// Title is a short human label for a tool picker.
	Title string
	// Description tells an agent WHEN to reach for this, not merely what it
	// does. An agent choosing between tools is reading this.
	Description string
	// Scope is what a caller must hold. Checked on every call.
	Scope auth.Scope
	// ReadOnly says this tool changes nothing. It is advisory to the agent —
	// MCP clients surface it to decide whether to ask a human first — and it
	// is asserted against Scope in the registry, because a read-only tool
	// needing `write` is a contradiction that would mislead exactly the
	// confirmation prompt it exists to inform.
	ReadOnly bool
	// InputSchema is hand-written JSON Schema. A tool schema is an interface
	// contract with the same permanence as an endpoint, so it is authored and
	// golden-tested rather than reflected out of a struct (ADR-0015's
	// reasoning, applied to a second interface).
	InputSchema map[string]any
	// Handler runs the tool. It returns a value to be JSON-encoded as the
	// result, or an error the server maps onto a JSON-RPC code.
	Handler func(ctx context.Context, args json.RawMessage) (any, error)
}

// descriptor is the wire shape of a tool in tools/list.
type descriptor struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations *annotations   `json:"annotations,omitempty"`
}

// annotations are MCP's advisory hints about a tool's behaviour.
type annotations struct {
	ReadOnlyHint bool `json:"readOnlyHint"`
	// RequiredScope is not part of MCP. It is here because an agent that can
	// see a verb it may not call is better served by being told why than by
	// discovering it at call time — and because a human reading tools/list is
	// reading the authorisation contract.
	RequiredScope string `json:"heyarr/requiredScope"`
}

// registry holds the tool surface.
//
// It is a map plus a sorted name list rather than a slice, because tools/list
// must be STABLE: an agent diffing the surface between calls should see a
// change only when the surface changed, and Go randomises map iteration.
type registry struct {
	byName map[string]Tool
	names  []string
}

func newRegistry() *registry {
	return &registry{byName: map[string]Tool{}}
}

// register adds a tool, refusing anything incoherent at wiring time.
//
// Panics rather than returning an error, matching the job registry's stance:
// this runs once at construction, and a tool silently missing from the surface
// — or worse, silently registered twice with the second winning — is far worse
// than a startup crash.
func (r *registry) register(t Tool) {
	if t.Name == "" {
		panic("mcp: a tool needs a name")
	}
	if _, exists := r.byName[t.Name]; exists {
		panic("mcp: " + t.Name + " is registered twice")
	}
	if t.Handler == nil {
		panic("mcp: " + t.Name + " has no handler — an absent tool is better than a stub")
	}
	if t.InputSchema == nil {
		panic("mcp: " + t.Name + " has no input schema")
	}
	if _, err := auth.ParseScope(string(t.Scope)); err != nil {
		panic("mcp: " + t.Name + " declares no valid scope: " + err.Error())
	}
	// A read-only tool that demands `write` would make an MCP client's
	// confirmation prompt lie in the safe direction, and a mutating tool
	// marked read-only would make it lie in the dangerous one.
	if t.ReadOnly && t.Scope != auth.ScopeRead {
		panic("mcp: " + t.Name + " is read-only but demands " + string(t.Scope))
	}
	if !t.ReadOnly && t.Scope == auth.ScopeRead {
		panic("mcp: " + t.Name + " mutates but demands only read")
	}

	r.byName[t.Name] = t
	r.names = append(r.names, t.Name)
	sort.Strings(r.names)
}

// lookup finds a tool by name.
func (r *registry) lookup(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// all returns every tool, in a stable order.
func (r *registry) all() []Tool {
	out := make([]Tool, 0, len(r.names))
	for _, n := range r.names {
		out = append(out, r.byName[n])
	}
	return out
}

// Names returns the registered tool names, in a stable order.
//
// Exported so a test can enumerate the whole surface rather than inspect it —
// which is how §72's boundary is asserted.
func (s *Server) Names() []string {
	return append([]string(nil), s.tools.names...)
}

// Tools returns every registered tool, in a stable order. Exported for the
// same reason as Names.
func (s *Server) Tools() []Tool { return s.tools.all() }

// toolError is a failure the agent caused, carrying the JSON-RPC code it maps
// onto. Anything else that comes out of a handler is ours and becomes an
// internal error, because handing an agent our internals is neither useful to
// it nor safe for us.
type toolError struct {
	code int
	err  error
}

func (e *toolError) Error() string { return e.err.Error() }
func (e *toolError) Unwrap() error { return e.err }

func invalidParams(format string, args ...any) error {
	return &toolError{code: codeInvalidParams, err: fmt.Errorf(format, args...)}
}
