package logging

import (
	"bytes"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestParseLevelAcceptsCanonicalNamesCaseInsensitively(t *testing.T) {
	for _, in := range []string{"debug", "info", "warn", "error", "DEBUG", "Info", "WARN", "Error"} {
		if _, err := ParseLevel("TEST_LEVEL", in); err != nil {
			t.Errorf("ParseLevel(%q) = %v, want nil", in, err)
		}
	}
}

func TestParseLevelRejectsUnknownValues(t *testing.T) {
	for _, in := range []string{"", "verbose", "warning", "loud", "1"} {
		if _, err := ParseLevel("TEST_LEVEL", in); err == nil {
			t.Errorf("ParseLevel(%q) should fail", in)
		}
	}
}

func TestParseLevelErrorNamesTheSetting(t *testing.T) {
	_, err := ParseLevel("MCP_LOG_LEVEL", "loud")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "MCP_LOG_LEVEL") {
		t.Errorf("error should name the setting so an operator knows which knob to fix, got: %v", err)
	}
}

func TestFormatTimeRewritesOnlyTheTimeAttr(t *testing.T) {
	ts := time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.UTC)
	want := "2026-03-04T05:06:07.123456Z"

	if got := FormatTime(nil, slog.Time(slog.TimeKey, ts)); got.Value.String() != want {
		t.Errorf("FormatTime = %q, want %q", got.Value.String(), want)
	}

	// Non-time attributes pass through untouched.
	if got := FormatTime(nil, slog.Int("n", 1)); got.Value.Int64() != 1 {
		t.Errorf("FormatTime should leave non-time attrs alone, got %v", got)
	}
}

func TestNewLoggerUsesTheUniformTimestampFormat(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(NewHandler(&buf, slog.LevelDebug))
	l.Info("hello", "k", "v")

	re := regexp.MustCompile(`time=\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}(Z|[+-]\d{2}:\d{2})`)
	if !re.MatchString(buf.String()) {
		t.Errorf("log line does not carry the uniform timestamp format: %s", buf.String())
	}
}

func TestNewHandlerHonorsLevel(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(NewHandler(&buf, slog.LevelInfo))
	l.Debug("should be dropped")
	l.Info("should be kept")
	if strings.Contains(buf.String(), "should be dropped") {
		t.Errorf("debug line leaked past an info-level handler: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "should be kept") {
		t.Errorf("info line missing: %s", buf.String())
	}
}
