package vaultplacement

// maxRequestBody bounds a pin request. It carries two short fields — a digest and
// a peer id — so a few kilobytes is already generous, and an unbounded decode on a
// request body is a memory-exhaustion primitive a write token should not hand out.
const maxRequestBody = 64 << 10
