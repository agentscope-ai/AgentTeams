package sandboxpolicy

import (
	"testing"
	"time"
)

func TestResolveDurationMatchesDeepAgentsWholeSecondGrammar(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "whole seconds", raw: "45s", want: 45 * time.Second},
		{name: "combined parts", raw: "6h30m", want: 6*time.Hour + 30*time.Minute},
		{name: "fractional part resolving to seconds", raw: "0.5m", want: 30 * time.Second},
		{name: "fractional parts with whole total", raw: "0.5s0.5s", want: time.Second},
		{name: "Go millisecond syntax", raw: "500ms", wantErr: true},
		{name: "Go millisecond syntax resolving to whole second", raw: "1000ms", wantErr: true},
		{name: "Go microsecond syntax", raw: "1000000us", wantErr: true},
		{name: "Go nanosecond syntax", raw: "1000000000ns", wantErr: true},
		{name: "fractional second", raw: "1.5s", wantErr: true},
		{name: "missing leading digit", raw: ".5m", wantErr: true},
		{name: "uppercase unit", raw: "1H", wantErr: true},
		{name: "zero total", raw: "0s", wantErr: true},
		{name: "negative duration", raw: "-1s", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveDuration(tt.raw, 30*time.Minute, "idleTimeout")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ResolveDuration(%q)=%s, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveDuration(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("ResolveDuration(%q)=%s, want %s", tt.raw, got, tt.want)
			}
		})
	}
}

func TestResolveDurationUsesFallbackOnlyWhenOmitted(t *testing.T) {
	fallback := 30 * time.Minute
	got, err := ResolveDuration("", fallback, "idleTimeout")
	if err != nil || got != fallback {
		t.Fatalf("ResolveDuration(empty)=(%s, %v), want (%s, nil)", got, err, fallback)
	}
	for _, raw := range []string{" ", "\t", " 1s", "1s "} {
		if got, err := ResolveDuration(raw, fallback, "idleTimeout"); err == nil {
			t.Fatalf("ResolveDuration(%q)=%s, want explicit whitespace rejected", raw, got)
		}
	}
}
