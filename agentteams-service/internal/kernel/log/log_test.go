package log

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
		ok   bool
	}{
		{"debug", slog.LevelDebug, true},
		{"info", slog.LevelInfo, true},
		{"", slog.LevelInfo, true},
		{"warn", slog.LevelWarn, true},
		{"error", slog.LevelError, true},
		{"verbose", 0, false},
	}
	for _, c := range cases {
		got, err := ParseLevel(c.in)
		if c.ok {
			if err != nil {
				t.Fatalf("ParseLevel(%q): unexpected error %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("ParseLevel(%q) = %v, want %v", c.in, got, c.want)
			}
		} else {
			if err == nil {
				t.Fatalf("ParseLevel(%q): expected error, got nil", c.in)
			}
		}
	}
}

func TestNewJSONOutput(t *testing.T) {
	var buf bytes.Buffer
	lg, err := New("info", &buf)
	if err != nil {
		t.Fatal(err)
	}
	lg.Info("hello", "request_id", "abc")

	line := strings.TrimSpace(buf.String())
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("output is not JSON: %v (%s)", err, line)
	}
	if rec["msg"] != "hello" || rec["request_id"] != "abc" {
		t.Fatalf("unexpected record: %v", rec)
	}
	if rec["level"] != "INFO" {
		t.Fatalf("level = %v, want INFO", rec["level"])
	}
}

func TestNewLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	lg, err := New("error", &buf)
	if err != nil {
		t.Fatal(err)
	}
	lg.Info("should be filtered")
	lg.Error("should appear")
	if strings.Contains(buf.String(), "should be filtered") {
		t.Fatal("info record emitted at error level")
	}
	if !strings.Contains(buf.String(), "should appear") {
		t.Fatal("error record missing")
	}
}

func TestNewInvalidLevel(t *testing.T) {
	if _, err := New("verbose", &bytes.Buffer{}); err == nil {
		t.Fatal("expected an error for an invalid level")
	}
}

func TestWithAddsFields(t *testing.T) {
	var buf bytes.Buffer
	lg, err := New("info", &buf)
	if err != nil {
		t.Fatal(err)
	}
	lg.With("component", "test").Info("scoped")
	if !strings.Contains(buf.String(), `"component":"test"`) {
		t.Fatalf("With field missing: %s", buf.String())
	}
}
