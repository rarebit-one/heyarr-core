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
//
// # The personal-state read verbs (§72, §73, M9/M11)
//
// When a controller is configured, this server also exposes the READ verbs over
// the user's encrypted personal state — personal_playlist, personal_starred,
// personal_history and personal_reading_position (#372, gated on the M9 plane and
// the CRDT types of #386). Each fetches the opaque ciphertext from the
// controller, unwraps the space key with THIS device's key, and materialises the
// matching CRDT locally: the controller stores ciphertext and can read none of
// it. Which CRDT a space holds is decided by the verb, never carried on the wire
// (a space holds one CRDT — see internal/personalstate/statesync). These verbs
// appear only when a reader is wired, so the bare device-key shell advertises
// nothing it cannot serve.
package personalmcp
