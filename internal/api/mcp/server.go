package mcp

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/api/resources"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// protocolVersion is the MCP revision this speaks.
const protocolVersion = "2025-06-18"

// serverName and serverVersion identify this implementation to a client.
const serverName = "heyarr"

// maxRequestBody bounds an inbound JSON-RPC document.
//
// Every call this server accepts is a handful of short fields, except
// explain_release, which carries a candidate set. A megabyte is generous for
// that and still far below what an unbounded json.Decode on a request body
// costs — which is a memory exhaustion primitive needing nothing but a read
// token.
const maxRequestBody = 1 << 20

// Options configure the MCP server.
type Options struct {
	// DB is the controller database, read through the reader pool.
	DB *sqlite.DB
	// Resources is the resource API, called for the WRITE intents.
	//
	// Not for reads: a tool is not an endpoint with a different envelope, and
	// wrapping the read handlers would make this the second read API ADR-0019
	// says it must not be. It is here so want_content and monitor_content are
	// the SAME implementation the HTTP door uses rather than a second one that
	// drifts.
	Resources *resources.API
	// Jobs enqueues the work verify_blob asks for.
	Jobs   *jobs.Queue
	Logger *slog.Logger
	// Version is the build version reported at initialize.
	Version string
}

// Server is the MCP endpoint.
type Server struct {
	db        *sqlite.DB
	reader    *sql.DB
	resources *resources.API
	jobs      *jobs.Queue
	log       *slog.Logger
	version   string

	tools *registry
}

// New constructs the MCP server and registers the tool surface.
func New(opts Options) (*Server, error) {
	if opts.DB == nil {
		return nil, errors.New("mcp: a database is required")
	}
	if opts.Resources == nil {
		return nil, errors.New("mcp: the resource API is required — " +
			"the write intents are shared with it rather than reimplemented")
	}
	if opts.Jobs == nil {
		return nil, errors.New("mcp: a job queue is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	version := opts.Version
	if version == "" {
		version = "dev"
	}

	s := &Server{
		db:        opts.DB,
		reader:    opts.DB.Reader(),
		resources: opts.Resources,
		jobs:      opts.Jobs,
		log:       log.With("component", "mcp"),
		version:   version,
		tools:     newRegistry(),
	}
	s.registerTools()
	return s, nil
}

// Mount registers the MCP endpoint on the authenticated /api/v1 router.
//
// One route. MCP multiplexes over JSON-RPC rather than over paths, and mounting
// it here rather than standing up a second server means it inherits the whole
// middleware chain — request correlation, the access log, panic recovery,
// metrics — and the `read` scope floor, without any of that being remembered
// again. The per-tool scope check is what turns that floor into a contract.
func (s *Server) Mount(r chi.Router) {
	r.Post("/mcp", s.handle)
}

// handle serves one JSON-RPC request.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			httpapi.Fail(w, r, problem.BadRequest("the request body is too large"))
			return
		}
		httpapi.Fail(w, r, problem.BadRequest("could not read the request body"))
		return
	}

	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		s.write(w, r, newError(nil, codeParseError, "the request is not valid JSON", nil))
		return
	}
	if req.JSONRPC != jsonRPCVersion {
		s.write(w, r, newError(req.ID, codeInvalidRequest,
			"this endpoint speaks JSON-RPC "+jsonRPCVersion, nil))
		return
	}

	resp, answer := s.dispatch(r, req)
	if !answer {
		// A notification. JSON-RPC says it MUST NOT be answered, and some
		// clients treat a reply to one as a protocol violation.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	s.write(w, r, resp)
}

// dispatch routes one request, returning whether it should be answered at all.
func (s *Server) dispatch(r *http.Request, req request) (response, bool) {
	if req.isNotification() {
		// Lifecycle notifications are accepted and ignored. There is nothing
		// this server needs to do when a client announces it has initialised,
		// and refusing an unknown one would make us stricter than the protocol.
		return response{}, false
	}

	switch req.Method {
	case "initialize":
		return s.initialize(req), true
	case "tools/list":
		return newResponse(req.ID, map[string]any{"tools": s.descriptors()}), true
	case "tools/call":
		return s.callTool(r, req), true
	case "ping":
		// MCP's keepalive. An empty result is the whole protocol.
		return newResponse(req.ID, map[string]any{}), true
	default:
		return newError(req.ID, codeMethodNotFound,
			"no such method: "+req.Method, nil), true
	}
}

// initialize answers the handshake.
func (s *Server) initialize(req request) response {
	return newResponse(req.ID, map[string]any{
		"protocolVersion": protocolVersion,
		"serverInfo": map[string]any{
			"name":    serverName,
			"version": s.version,
		},
		// Tools and nothing else. Resources and prompts are MCP features this
		// server deliberately does not implement: a resource list would be the
		// second read API ADR-0019 exists to prevent.
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"instructions": instructions,
	})
}

// descriptors renders the tool surface for tools/list.
//
// Every tool is listed, whatever the caller holds. A verb that vanished for a
// read token would make the vocabulary depend on the credential, so an agent
// could not learn what exists — and the required scope is on the descriptor so
// it can tell the difference between "no such verb" and "not with this token".
func (s *Server) descriptors() []descriptor {
	tools := s.tools.all()
	out := make([]descriptor, 0, len(tools))
	for _, t := range tools {
		out = append(out, descriptor{
			Name:        t.Name,
			Title:       t.Title,
			Description: t.Description,
			InputSchema: t.InputSchema,
			Annotations: &annotations{
				ReadOnlyHint:  t.ReadOnly,
				RequiredScope: string(t.Scope),
			},
		})
	}
	return out
}

// callParams is the tools/call params object.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// callTool runs one tool, enforcing its scope first.
func (s *Server) callTool(r *http.Request, req request) response {
	var params callParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return newError(req.ID, codeInvalidParams, "params is not an object", nil)
		}
	}
	if params.Name == "" {
		return newError(req.ID, codeInvalidParams, "name is required", nil)
	}

	tool, ok := s.tools.lookup(params.Name)
	if !ok {
		// Deliberately specific. §71 lists verbs this milestone does not
		// carry, and an agent that asked for one deserves to know it is
		// coming rather than that it was mistyped.
		return newError(req.ID, codeMethodNotFound,
			"no such tool: "+params.Name, deferralFor(params.Name))
	}

	// The scope check, on EVERY call, before anything else runs.
	//
	// An MCP session is not a new trust domain (ADR-0011). The route already
	// requires `read` from the router, so a caller reaching here holds at
	// least that; this is what stops a read token calling a mutating verb.
	id, authenticated := httpapi.IdentityFrom(r.Context())
	if !authenticated {
		return newError(req.ID, codeForbidden, "this endpoint requires a bearer token", nil)
	}
	if !id.Allows(tool.Scope) {
		return newError(req.ID, codeForbidden,
			params.Name+" requires the "+string(tool.Scope)+" scope",
			map[string]any{
				"tool":           params.Name,
				"required_scope": string(tool.Scope),
			})
	}

	result, err := tool.Handler(r.Context(), params.Arguments)
	if err != nil {
		return s.toolFailure(r, req, params.Name, err)
	}
	return newResponse(req.ID, wrapResult(result))
}

// toolFailure maps a handler error onto a JSON-RPC error.
//
// Only errors this package classified reach the client with their detail. An
// unclassified error is ours — a broken query, a closed database — and the
// agent is told that something failed and nothing else, because handing it our
// internals is neither useful to it nor safe for us. The detail goes to the
// log, correlated with the request, which is where an operator will look.
func (s *Server) toolFailure(r *http.Request, req request, name string, err error) response {
	var te *toolError
	if errors.As(err, &te) {
		return newError(req.ID, te.code, te.Error(), map[string]any{"tool": name})
	}
	if errors.Is(err, sql.ErrNoRows) {
		return newError(req.ID, codeInvalidParams,
			"nothing here matches that identifier", map[string]any{"tool": name})
	}
	s.log.Error("an mcp tool failed",
		"request_id", httpapi.RequestIDFrom(r.Context()),
		"tool", name, "error", err)
	return newError(req.ID, codeInternalError, "the tool failed", map[string]any{"tool": name})
}

// wrapResult renders a tool's return value in MCP's content envelope.
//
// MCP requires `content`, an array of typed blocks, and every client can render
// a text block. `structuredContent` carries the same value as JSON for a client
// that can use it — so an agent gets prose it can quote AND a shape it can
// branch on, rather than being made to parse the prose.
func wrapResult(v any) map[string]any {
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		// The tool results are plain structs; marshalling cannot fail. If it
		// somehow does, saying so beats a panic behind a JSON-RPC envelope.
		return map[string]any{
			"content": []any{map[string]any{
				"type": "text",
				"text": "the result could not be encoded",
			}},
			"isError": true,
		}
	}
	return map[string]any{
		"content": []any{map[string]any{
			"type": "text",
			"text": string(encoded),
		}},
		"structuredContent": v,
	}
}

func (s *Server) write(w http.ResponseWriter, r *http.Request, resp response) {
	buf, err := json.Marshal(resp)
	if err != nil {
		s.log.Error("encoding an mcp response failed",
			"request_id", httpapi.RequestIDFrom(r.Context()), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
}

var _ = auth.ScopeRead
