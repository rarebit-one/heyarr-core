// Package personalmcp is the Personal MCP (§73): a LOCAL, stdio JSON-RPC server
// an agent on the user's own machine talks to.
//
// # Why this is not a tool on the controller's MCP
//
// §72 is not a caveat, it is a boundary: controller-side MCP cannot decrypt
// user artifacts. §73 answers that by putting private state behind a SEPARATE
// Personal MCP, exposed by a client the user runs, not by the server.
//
// Adding these verbs to internal/api/mcp would move a private key onto the
// controller — reachable by anyone holding a bearer token, logged by the
// controller's middleware, backed up with the catalog. That is the vulnerability
// §38 and SECURITY.md exist to prevent, and no amount of scope checking makes it
// safe, because the server would hold the material either way. See ADR-0032.
//
// The two servers therefore share no code and no transport. This one:
//
//   - speaks over stdin/stdout, so it is reachable only by a process the user
//     started on the machine that holds the key;
//   - has no bearer tokens and no scopes, because there is no remote caller to
//     authorise — containment is the OS's (§74), which is also why the key file
//     is 0600 and the directory 0700;
//   - never returns key material, only public keys and paths.
//
// # Why the tool list is short and asserted
//
// The same rule internal/api/mcp follows: ship only the verbs whose underlying
// capability exists. This device can generate, list, show and remove ONE key.
// It cannot wrap a space key, delegate, grant a capability, pair or recover —
// those are Milestone 8 and 9 — so those verbs are absent rather than stubbed.
// A test enumerates the surface exactly, so a verb added later cannot arrive
// unnoticed.
package personalmcp
