// Package logging provides uniform slog logging setup with microsecond RFC3339 timestamps.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"
)

// TimeFormat is RFC3339 with microsecond precision and local timezone offset.
const TimeFormat = "2006-01-02T15:04:05.000000Z07:00"

// FormatTime formats slog.TimeKey attributes using TimeFormat.
func FormatTime(groups []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey {
		if a.Value.Kind() == slog.KindTime {
			a.Value = slog.StringValue(a.Value.Time().Format(TimeFormat))
		} else if t, ok := a.Value.Any().(time.Time); ok {
			a.Value = slog.StringValue(t.Format(TimeFormat))
		}
	}
	return a
}

// NewHandler returns a slog.TextHandler writing to w at level with TimeFormat timestamps.
func NewHandler(w io.Writer, level slog.Level) slog.Handler {
	if w == nil {
		w = os.Stderr
	}
	return slog.NewTextHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: FormatTime,
	})
}

// NewLogger returns a logger writing to stderr at level with TimeFormat timestamps.
func NewLogger(level slog.Level) *slog.Logger {
	return slog.New(NewHandler(os.Stderr, level))
}

// ParseLevel parses slog log level names (debug, info, warn, error) case-insensitively.
func ParseLevel(name, value string) (slog.Level, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(value)); err != nil {
		return 0, fmt.Errorf("%s must be one of debug, info, warn or error, got %q", name, value)
	}
	return lvl, nil
}
