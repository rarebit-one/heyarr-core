package personalstate

// maxRequestBody bounds a push. A wrapped key is ~100 bytes and a change's
// ciphertext is a small CRDT delta, so a few megabytes is already generous, and
// an unbounded decode on a request body is a memory-exhaustion primitive a write
// token should not hand out. It is larger than resources' 1 MiB because a change
// carries opaque ciphertext rather than a handful of short fields.
const maxRequestBody = 4 << 20
