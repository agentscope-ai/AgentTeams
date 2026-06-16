// Package envcompat provides dual-prefix environment variable resolution
// during the HiClaw → AgentTeams rename (#861).
//
// All exported helpers accept the legacy "HICLAW_FOO" key. The new
// "AGENTTEAMS_FOO" key takes precedence; the legacy key is consulted as a
// fallback and emits a one-time deprecation warning per variable.
//
// After Phase 2 of the rename, this package is removed and callers switch to
// passing "AGENTTEAMS_"-prefixed keys to plain os.Getenv.
package envcompat

import (
	"os"
	"strconv"
	"strings"
	"sync"

	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	legacyPrefix = "HICLAW_"
	newPrefix    = "AGENTTEAMS_"
)

// warningSink receives one call per first observation of a deprecated key.
// It is overridable for tests; the default sends to controller-runtime's
// logger under "env-fallback".
var (
	deprecationOnce sync.Map
	warningSinkMu   sync.RWMutex
	warningSink     = defaultWarningSink
)

func defaultWarningSink(oldKey, newKey string) {
	ctrl.Log.WithName("env-fallback").Info(
		"legacy environment variable is deprecated",
		"old", oldKey, "new", newKey,
	)
}

// translateKey maps "HICLAW_FOO" → "AGENTTEAMS_FOO". Returns ("", false) when
// key has no recognized prefix.
func translateKey(key string) (string, bool) {
	if strings.HasPrefix(key, legacyPrefix) {
		return newPrefix + strings.TrimPrefix(key, legacyPrefix), true
	}
	return "", false
}

// Lookup reads an env var with AGENTTEAMS_ → HICLAW_ fallback. Keys outside
// the rename namespace pass through to os.Getenv.
func Lookup(key string) string {
	if newKey, ok := translateKey(key); ok {
		if v := os.Getenv(newKey); v != "" {
			return v
		}
		if v := os.Getenv(key); v != "" {
			warnDeprecated(key, newKey)
			return v
		}
		return ""
	}
	return os.Getenv(key)
}

func warnDeprecated(oldKey, newKey string) {
	if _, loaded := deprecationOnce.LoadOrStore(oldKey, struct{}{}); loaded {
		return
	}
	warningSinkMu.RLock()
	sink := warningSink
	warningSinkMu.RUnlock()
	sink(oldKey, newKey)
}

// OrDefault returns Lookup(key) or defaultVal when the value is empty.
func OrDefault(key, defaultVal string) string {
	if v := Lookup(key); v != "" {
		return v
	}
	return defaultVal
}

// OrDefaultInt parses Lookup(key) as int; falls back to defaultVal on error or empty.
func OrDefaultInt(key string, defaultVal int) int {
	if v := Lookup(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
}

// Bool reports whether Lookup(key) is one of 1/true/True/TRUE.
func Bool(key string) bool {
	v := Lookup(key)
	return v == "1" || v == "true" || v == "True" || v == "TRUE"
}

// BoolDefault is Bool with an explicit default for the unset case.
func BoolDefault(key string, defaultVal bool) bool {
	v := Lookup(key)
	if v == "" {
		return defaultVal
	}
	return v == "1" || v == "true" || v == "True" || v == "TRUE"
}

// resetDeprecationWarningsForTest clears the once-tracker so tests can
// observe warnings on repeat invocations.
func resetDeprecationWarningsForTest() {
	deprecationOnce.Range(func(k, _ any) bool {
		deprecationOnce.Delete(k)
		return true
	})
}

// setWarningSinkForTest replaces the deprecation warning sink and returns a
// function that restores the previous sink. Always pair with
// resetDeprecationWarningsForTest if the test relies on observing warnings
// for keys that earlier tests may have already triggered.
func setWarningSinkForTest(fn func(oldKey, newKey string)) func() {
	warningSinkMu.Lock()
	prev := warningSink
	warningSink = fn
	warningSinkMu.Unlock()
	return func() {
		warningSinkMu.Lock()
		warningSink = prev
		warningSinkMu.Unlock()
	}
}
