package personalmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/rarebit-one/heyarr-core/internal/device"
)

// protocolVersion is the MCP revision this speaks. The same revision the
// controller's server speaks, because a client should not need to know which
// of the two it dialled to know how to talk to it (§73's picture has one agent
// and two servers).
const protocolVersion = "2025-06-18"

// serverName identifies this implementation. It is deliberately NOT "heyarr":
// an agent holding two connections must be able to tell the Personal MCP from
// the controller's, and a name they share is a name that tells it nothing.
const serverName = "heyarr-personal"

// jsonRPCVersion is the only version accepted.
const jsonRPCVersion = "2.0"

// maxLine bounds one inbound message. Every call this server accepts is a
// handful of short fields; a megabyte is generous and still bounded.
const maxLine = 1 << 20

// The JSON-RPC error codes this server uses.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// The stable `reason` codes carried on a refusal.
//
// An agent branching on the message is an agent that breaks when the message
// improves, so the machine-readable half is these — the same reasoning behind
// §63's stable rule codes.
const (
	reasonDeviceExists   = "device_exists"
	reasonUnknownDevice  = "unknown_device"
	reasonNoDevice       = "no_device"
	reasonKeyPermissions = "key_permissions"
	reasonMalformedKey   = "malformed_key"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r request) isNotification() bool { return len(r.ID) == 0 }

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func newResponse(id json.RawMessage, result any) response {
	return response{JSONRPC: jsonRPCVersion, ID: id, Result: result}
}

func newError(id json.RawMessage, code int, message string, data any) response {
	return response{JSONRPC: jsonRPCVersion, ID: id, Error: &rpcError{Code: code, Message: message, Data: data}}
}

// Tool is one key-management verb.
type Tool struct {
	Name        string
	Title       string
	Description string
	// ReadOnly says this tool changes nothing.
	ReadOnly bool
	// Destructive says this tool can lose something that cannot be recovered.
	// An MCP client surfaces it to decide whether to ask a person first, and
	// removing the only key on a machine is exactly such a moment.
	Destructive bool
	InputSchema map[string]any
	Handler     func(args json.RawMessage) (any, error)
}

type descriptor struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations *annotations   `json:"annotations,omitempty"`
}

type annotations struct {
	ReadOnlyHint    bool `json:"readOnlyHint"`
	DestructiveHint bool `json:"destructiveHint"`
}

// Options configure the server.
type Options struct {
	// Store is the device store this server manages. Required.
	Store *device.Store
	// Version is reported at initialize.
	Version string
	// Stdin and Stdout are the transport. Defaults are the process's, which is
	// what `heyarr device mcp` wants; tests inject pipes.
	Stdin  io.Reader
	Stdout io.Writer
}

// Server is the Personal MCP.
type Server struct {
	store   *device.Store
	version string
	in      io.Reader
	out     io.Writer

	byName map[string]Tool
	names  []string
}

// New constructs the server and registers the tool surface.
func New(opts Options) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("personalmcp: a device store is required")
	}
	version := opts.Version
	if version == "" {
		version = "dev"
	}
	s := &Server{
		store:   opts.Store,
		version: version,
		in:      opts.Stdin,
		out:     opts.Stdout,
		byName:  map[string]Tool{},
	}
	s.registerTools()
	return s, nil
}

// register adds a tool, panicking on anything incoherent. This runs once at
// construction, and a tool silently missing — or registered twice with the
// second winning — is far worse than a crash at startup.
func (s *Server) register(t Tool) {
	if t.Name == "" {
		panic("personalmcp: a tool needs a name")
	}
	if _, exists := s.byName[t.Name]; exists {
		panic("personalmcp: " + t.Name + " is registered twice")
	}
	if t.Handler == nil {
		panic("personalmcp: " + t.Name + " has no handler")
	}
	if t.InputSchema == nil {
		panic("personalmcp: " + t.Name + " has no input schema")
	}
	if t.ReadOnly && t.Destructive {
		panic("personalmcp: " + t.Name + " is both read-only and destructive")
	}
	s.byName[t.Name] = t
	s.names = append(s.names, t.Name)
	sort.Strings(s.names)
}

// Names returns the registered tool names, in a stable order.
//
// Exported so a test can enumerate the whole surface rather than inspect it —
// which is how the §72/§73 boundary is asserted here, exactly as it is on the
// controller's server.
func (s *Server) Names() []string { return append([]string(nil), s.names...) }

// Serve reads newline-delimited JSON-RPC from stdin and writes replies to
// stdout, which is MCP's stdio transport. It returns when the input ends.
//
// Nothing is written to stdout but protocol messages: on this transport a
// stray print is a protocol error. Diagnostics belong on stderr.
func (s *Server) Serve(ctx context.Context) error {
	in := s.in
	if in == nil {
		return errors.New("personalmcp: no input stream")
	}
	out := s.out
	if out == nil {
		return errors.New("personalmcp: no output stream")
	}

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)
	enc := json.NewEncoder(out)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		resp, answer := s.Handle(line)
		if !answer {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return fmt.Errorf("personalmcp: writing a reply: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("personalmcp: reading stdin: %w", err)
	}
	return nil
}

// Handle answers one message, reporting whether it should be answered at all.
//
// Exported so a test can drive the protocol without a pipe, and so the reply to
// every message is produced by exactly the code Serve runs.
func (s *Server) Handle(raw []byte) (any, bool) {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return newError(nil, codeParseError, "the request is not valid JSON", nil), true
	}
	if req.JSONRPC != jsonRPCVersion {
		return newError(req.ID, codeInvalidRequest, "this server speaks JSON-RPC "+jsonRPCVersion, nil), true
	}
	if req.isNotification() {
		// JSON-RPC says a notification MUST NOT be answered, and some clients
		// treat a reply to one as fatal.
		return nil, false
	}

	switch req.Method {
	case "initialize":
		return s.initialize(req), true
	case "tools/list":
		return newResponse(req.ID, map[string]any{"tools": s.descriptors()}), true
	case "tools/call":
		return s.callTool(req), true
	case "ping":
		return newResponse(req.ID, map[string]any{}), true
	default:
		return newError(req.ID, codeMethodNotFound, "no such method: "+req.Method, nil), true
	}
}

func (s *Server) initialize(req request) response {
	return newResponse(req.ID, map[string]any{
		"protocolVersion": protocolVersion,
		"serverInfo":      map[string]any{"name": serverName, "version": s.version},
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"instructions":    instructions,
	})
}

func (s *Server) descriptors() []descriptor {
	out := make([]descriptor, 0, len(s.names))
	for _, name := range s.names {
		t := s.byName[name]
		out = append(out, descriptor{
			Name:        t.Name,
			Title:       t.Title,
			Description: t.Description,
			InputSchema: t.InputSchema,
			Annotations: &annotations{ReadOnlyHint: t.ReadOnly, DestructiveHint: t.Destructive},
		})
	}
	return out
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) callTool(req request) response {
	var params callParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return newError(req.ID, codeInvalidParams, "params is not an object", nil)
		}
	}
	if params.Name == "" {
		return newError(req.ID, codeInvalidParams, "name is required", nil)
	}
	tool, ok := s.byName[params.Name]
	if !ok {
		return newError(req.ID, codeMethodNotFound, "no such tool: "+params.Name,
			map[string]any{"available": s.Names()})
	}
	result, err := tool.Handler(params.Arguments)
	if err != nil {
		return s.toolFailure(req, params.Name, err)
	}
	return newResponse(req.ID, wrapResult(result))
}

// toolFailure maps a refusal onto a JSON-RPC error with a stable reason.
//
// Every refusal this store can produce is the caller's to fix — a chmod, a
// typo, a decision to overwrite — so each is reported as such, with the reason
// code an agent can branch on. Anything else is ours and the agent is told
// nothing but that it failed.
func (s *Server) toolFailure(req request, name string, err error) response {
	for _, m := range []struct {
		sentinel error
		reason   string
	}{
		{device.ErrDeviceExists, reasonDeviceExists},
		{device.ErrUnknownDevice, reasonUnknownDevice},
		{device.ErrNoDevice, reasonNoDevice},
		{device.ErrKeyPermissions, reasonKeyPermissions},
		{device.ErrMalformedKey, reasonMalformedKey},
	} {
		if errors.Is(err, m.sentinel) {
			return newError(req.ID, codeInvalidParams, err.Error(),
				map[string]any{"tool": name, "reason": m.reason})
		}
	}
	return newError(req.ID, codeInternalError, "the tool failed", map[string]any{"tool": name})
}

// wrapResult renders a result in MCP's content envelope: prose a client can
// show, and the same value as JSON for one that can branch on it.
func wrapResult(v any) map[string]any {
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "the result could not be encoded"}},
			"isError": true,
		}
	}
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(encoded)}},
		"structuredContent": v,
	}
}
