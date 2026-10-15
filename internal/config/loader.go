package config

import "context"

// Loader resolves a Config from a path: Load for the heyarr binary,
// LoadMnemosyne for the mnemosyne binary (ADR-0107).
type Loader func(path string) (Config, error)

type loaderKey struct{}

// WithLoader returns a context that carries the Loader every shared CLI
// subcommand should use. The two binaries share one command tree, so a
// command cannot know which service it belongs to from its own position; the
// root that built the tree knows, and says so here. Nothing global: a test
// may build both roots in one process.
func WithLoader(ctx context.Context, l Loader) context.Context {
	return context.WithValue(ctx, loaderKey{}, l)
}

// LoadFromContext loads configuration with the Loader the context carries,
// or Load (heyarr defaults) when it carries none. Shared CLI code reaches
// config through this, never through Load directly, so a command run under
// the mnemosyne root derives mnemosyne.db / mnemosyne.sock and reads the
// MNEMOSYNE_ environment — the same file the mnemosyne server opens.
func LoadFromContext(ctx context.Context, path string) (Config, error) {
	if ctx != nil {
		if l, ok := ctx.Value(loaderKey{}).(Loader); ok && l != nil {
			return l(path)
		}
	}
	return Load(path)
}
