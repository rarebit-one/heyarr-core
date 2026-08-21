package mcp

import "encoding/json"

// JSON-RPC 2.0, the subset MCP speaks.
//
// Hand-written rather than imported: the protocol surface this package needs
// is three methods, and the envelope is a dozen fields. See the package doc for
// why that is a deliberate choice rather than an omission.

// jsonRPCVersion is the only version accepted. A request claiming another is
// rejected rather than assumed compatible.
const jsonRPCVersion = "2.0"

// request is an inbound JSON-RPC call.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether this request wants no reply.
//
// JSON-RPC says a request with no id is a notification and MUST NOT be
// answered. MCP uses them for lifecycle signals like notifications/initialized,
// and answering one is a protocol error that some clients treat as fatal.
func (r request) isNotification() bool { return len(r.ID) == 0 }

// response is an outbound JSON-RPC reply.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	// Data carries the actionable half. A code tells a client how to behave;
	// this tells a person what happened, and the two are different audiences.
	Data any `json:"data,omitempty"`
}

// The JSON-RPC 2.0 error codes, plus the one MCP adds.
//
// These are the numbers clients branch on, so they are the contract. A client
// branching on the message is a client that breaks when the message improves —
// the same reasoning behind the stable rule codes in §63's reasons.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603

	// codeForbidden is outside JSON-RPC's reserved range, which stops at
	// -32000. A refused scope is not a malformed request and must not be
	// reported as one: an agent that cannot tell "you may not do this" from
	// "you asked wrongly" will retry the wrong one forever.
	codeForbidden = -32001
)

func newResponse(id json.RawMessage, result any) response {
	return response{JSONRPC: jsonRPCVersion, ID: id, Result: result}
}

func newError(id json.RawMessage, code int, message string, data any) response {
	return response{
		JSONRPC: jsonRPCVersion,
		ID:      id,
		Error:   &rpcError{Code: code, Message: message, Data: data},
	}
}
