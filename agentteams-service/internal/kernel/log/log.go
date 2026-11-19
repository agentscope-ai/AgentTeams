// Package log builds the service logger: standard-library log/slog with a
// JSON handler (machine-collectable; the service runs as an independent
// process). No third-party logging library.
package log

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// New returns a slog.Logger writing JSON to w at the configured level
// (debug|info|warn|error). The returned logger's With(fields...) adds
// request-scope fields for downstream handlers.
func New(level string, w io.Writer) (*slog.Logger, error) {
	lvl, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})), nil
}

// ParseLevel maps the config level strings to slog levels. An empty
// string is treated as info.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q (want debug|info|warn|error)", s)
	}
}
