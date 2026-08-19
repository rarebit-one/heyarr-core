package cli

import (
	"io"
	"log/slog"
	"os"

	"github.com/lmittmann/tint"
	"github.com/rarebit-one/heyarr-core/internal/config"
)

// newLogger builds the process logger from configuration. JSON in production,
// human-readable when stderr is a terminal.
func newLogger(cfg config.Log, w io.Writer) *slog.Logger {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	format := cfg.Format
	if format == "auto" {
		if isTerminal(w) {
			format = "text"
		} else {
			format = "json"
		}
	}

	if format == "text" {
		return slog.New(tint.NewTextHandler(w, &tint.Options{Level: level}))
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}

// isTerminal reports whether w is an interactive terminal. Anything that is not
// an *os.File — a buffer in a test, a pipe — is not.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
