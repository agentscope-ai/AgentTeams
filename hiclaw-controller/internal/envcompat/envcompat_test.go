package envcompat

import (
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// warnObserver captures deprecation events from the injectable sink.
type warnObserver struct {
	mu    sync.Mutex
	calls []warnCall
}

type warnCall struct {
	oldKey, newKey string
}

func (w *warnObserver) sink(old, new string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, warnCall{old, new})
}

func (w *warnObserver) snapshot() []warnCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]warnCall, len(w.calls))
	copy(out, w.calls)
	return out
}

// withTestObserver installs a fresh observer and clears any prior warnings.
// Returns the observer plus a cleanup that restores the previous sink.
func withTestObserver(t *testing.T) *warnObserver {
	t.Helper()
	resetDeprecationWarningsForTest()
	obs := &warnObserver{}
	restore := setWarningSinkForTest(obs.sink)
	t.Cleanup(func() {
		restore()
		resetDeprecationWarningsForTest()
	})
	return obs
}

func TestLookup_NewKeyPrecedence(t *testing.T) {
	obs := withTestObserver(t)
	t.Setenv("AGENTTEAMS_FOO_BAR", "new")
	t.Setenv("HICLAW_FOO_BAR", "old")
	if got := Lookup("HICLAW_FOO_BAR"); got != "new" {
		t.Fatalf("Lookup = %q, want %q", got, "new")
	}
	if calls := obs.snapshot(); len(calls) != 0 {
		t.Fatalf("expected no deprecation warning when new key wins, got %+v", calls)
	}
}

func TestLookup_FallsBackToLegacy(t *testing.T) {
	obs := withTestObserver(t)
	t.Setenv("HICLAW_LEGACY_ONLY", "value")
	if got := Lookup("HICLAW_LEGACY_ONLY"); got != "value" {
		t.Fatalf("Lookup = %q, want %q", got, "value")
	}
	calls := obs.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 deprecation warning, got %d (%+v)", len(calls), calls)
	}
	if calls[0].oldKey != "HICLAW_LEGACY_ONLY" || calls[0].newKey != "AGENTTEAMS_LEGACY_ONLY" {
		t.Fatalf("warning payload = %+v, want HICLAW_LEGACY_ONLY → AGENTTEAMS_LEGACY_ONLY", calls[0])
	}
}

func TestLookup_DeprecationWarningEmittedOnce(t *testing.T) {
	obs := withTestObserver(t)
	t.Setenv("HICLAW_REPEAT_KEY", "value")
	for i := 0; i < 50; i++ {
		_ = Lookup("HICLAW_REPEAT_KEY")
	}
	if calls := obs.snapshot(); len(calls) != 1 {
		t.Fatalf("expected exactly 1 warning across 50 calls, got %d", len(calls))
	}
}

func TestLookup_NoWarningWhenUnset(t *testing.T) {
	obs := withTestObserver(t)
	if got := Lookup("HICLAW_NEVER_SET_XYZ"); got != "" {
		t.Fatalf("Lookup = %q, want empty", got)
	}
	if calls := obs.snapshot(); len(calls) != 0 {
		t.Fatalf("expected no warning for unset key, got %+v", calls)
	}
}

func TestLookup_NoWarningWhenNewKeyWins(t *testing.T) {
	obs := withTestObserver(t)
	t.Setenv("AGENTTEAMS_PRIMARY", "new")
	if got := Lookup("HICLAW_PRIMARY"); got != "new" {
		t.Fatalf("Lookup = %q, want %q", got, "new")
	}
	if calls := obs.snapshot(); len(calls) != 0 {
		t.Fatalf("expected no warning when new key is set, got %+v", calls)
	}
}

func TestLookup_NewEmptyFallsBackToLegacy(t *testing.T) {
	obs := withTestObserver(t)
	// AGENTTEAMS_ set but empty — current behavior treats empty as "not set",
	// falling back to HICLAW_. This mirrors the shell resolve_env contract.
	t.Setenv("AGENTTEAMS_EMPTY_NEW", "")
	t.Setenv("HICLAW_EMPTY_NEW", "legacy")
	if got := Lookup("HICLAW_EMPTY_NEW"); got != "legacy" {
		t.Fatalf("Lookup = %q, want %q (empty new should fall back)", got, "legacy")
	}
	if calls := obs.snapshot(); len(calls) != 1 {
		t.Fatalf("expected 1 warning when falling back, got %d", len(calls))
	}
}

func TestLookup_NonRenamedKeyPassesThrough(t *testing.T) {
	obs := withTestObserver(t)
	t.Setenv("SOME_OTHER_VAR", "plain")
	if got := Lookup("SOME_OTHER_VAR"); got != "plain" {
		t.Fatalf("Lookup = %q, want %q", got, "plain")
	}
	if calls := obs.snapshot(); len(calls) != 0 {
		t.Fatalf("expected no warning for non-renamed key, got %+v", calls)
	}
}

func TestOrDefault(t *testing.T) {
	withTestObserver(t)
	if got := OrDefault("HICLAW_MISSING_KEY", "fallback"); got != "fallback" {
		t.Fatalf("OrDefault = %q, want %q", got, "fallback")
	}
	t.Setenv("AGENTTEAMS_PRESENT", "set")
	if got := OrDefault("HICLAW_PRESENT", "fallback"); got != "set" {
		t.Fatalf("OrDefault = %q, want %q", got, "set")
	}
}

func TestOrDefaultInt(t *testing.T) {
	withTestObserver(t)
	if got := OrDefaultInt("HICLAW_NUM_MISSING", 42); got != 42 {
		t.Fatalf("OrDefaultInt = %d, want 42", got)
	}
	t.Setenv("AGENTTEAMS_NUM", "7")
	if got := OrDefaultInt("HICLAW_NUM", 42); got != 7 {
		t.Fatalf("OrDefaultInt = %d, want 7", got)
	}
	t.Setenv("AGENTTEAMS_NUM_BAD", "not-a-number")
	if got := OrDefaultInt("HICLAW_NUM_BAD", 99); got != 99 {
		t.Fatalf("OrDefaultInt = %d, want 99 (fallback on parse error)", got)
	}
}

func TestOrDefaultInt_NegativeAndZero(t *testing.T) {
	withTestObserver(t)
	t.Setenv("AGENTTEAMS_NEG", "-5")
	if got := OrDefaultInt("HICLAW_NEG", 1); got != -5 {
		t.Fatalf("OrDefaultInt(-5) = %d, want -5", got)
	}
	t.Setenv("AGENTTEAMS_ZERO", "0")
	if got := OrDefaultInt("HICLAW_ZERO", 99); got != 0 {
		t.Fatalf("OrDefaultInt(0) = %d, want 0", got)
	}
}

func TestBool(t *testing.T) {
	withTestObserver(t)
	cases := []struct {
		val  string
		want bool
	}{
		{"1", true}, {"true", true}, {"True", true}, {"TRUE", true},
		{"0", false}, {"false", false}, {"", false}, {"yes", false}, {"FALSE", false},
	}
	for _, c := range cases {
		t.Setenv("AGENTTEAMS_FLAG", c.val)
		if got := Bool("HICLAW_FLAG"); got != c.want {
			t.Errorf("Bool(%q) = %v, want %v", c.val, got, c.want)
		}
	}
}

func TestBoolDefault(t *testing.T) {
	withTestObserver(t)
	if got := BoolDefault("HICLAW_FLAG_UNSET", true); !got {
		t.Fatalf("BoolDefault unset = %v, want true (default)", got)
	}
	t.Setenv("AGENTTEAMS_FLAG_OVERRIDE", "false")
	if got := BoolDefault("HICLAW_FLAG_OVERRIDE", true); got {
		t.Fatalf("BoolDefault = %v, want false (env override)", got)
	}
	// Empty string should use default, mirroring the original semantics.
	t.Setenv("AGENTTEAMS_FLAG_EMPTY", "")
	if got := BoolDefault("HICLAW_FLAG_EMPTY", true); !got {
		t.Fatalf("BoolDefault empty = %v, want true (default kept)", got)
	}
}

func TestTranslateKey(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"HICLAW_FOO", "AGENTTEAMS_FOO", true},
		{"HICLAW_", "AGENTTEAMS_", true},
		{"SOMETHING_ELSE", "", false},
		{"", "", false},
		{"hiclaw_FOO", "", false}, // case-sensitive
	}
	for _, c := range cases {
		got, ok := translateKey(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("translateKey(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestLookup_ConcurrentSafe ensures that Lookup is safe under concurrent
// access and that the once-per-key warning guarantee holds in the face of
// races.
func TestLookup_ConcurrentSafe(t *testing.T) {
	obs := withTestObserver(t)
	const goroutines = 64
	const callsPerG = 100

	// Set a few legacy-only keys.
	for i := 0; i < 8; i++ {
		t.Setenv("HICLAW_RACE_"+strconv.Itoa(i), "v"+strconv.Itoa(i))
	}

	var wg sync.WaitGroup
	var hits int64
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < callsPerG; i++ {
				key := "HICLAW_RACE_" + strconv.Itoa((seed+i)%8)
				if v := Lookup(key); v == "" {
					t.Errorf("unexpected empty value for %s", key)
					return
				}
				atomic.AddInt64(&hits, 1)
			}
		}(g)
	}
	wg.Wait()

	if got := atomic.LoadInt64(&hits); got != int64(goroutines*callsPerG) {
		t.Fatalf("hits = %d, want %d", got, goroutines*callsPerG)
	}
	calls := obs.snapshot()
	if len(calls) != 8 {
		t.Fatalf("expected exactly 8 warnings (one per unique legacy key), got %d (%+v)", len(calls), calls)
	}
	// Each call must reference a distinct key.
	seen := make(map[string]bool)
	for _, c := range calls {
		if seen[c.oldKey] {
			t.Fatalf("duplicate warning for %s", c.oldKey)
		}
		seen[c.oldKey] = true
	}
}

// TestLookup_RestoreSinkRestoresDefault verifies that setWarningSinkForTest's
// restore function actually puts the default sink back.
func TestLookup_RestoreSinkRestoresDefault(t *testing.T) {
	resetDeprecationWarningsForTest()
	t.Cleanup(resetDeprecationWarningsForTest)

	var observed bool
	restore := setWarningSinkForTest(func(_, _ string) { observed = true })

	t.Setenv("HICLAW_RESTORE_CHECK_A", "v")
	_ = Lookup("HICLAW_RESTORE_CHECK_A")
	if !observed {
		t.Fatal("custom sink was not invoked while installed")
	}

	restore()

	// After restore, the default sink should be active. We cannot easily
	// capture controller-runtime's log output here, but we can at least
	// ensure no panic and that the custom sink no longer fires.
	observed = false
	t.Setenv("HICLAW_RESTORE_CHECK_B", "v")
	_ = Lookup("HICLAW_RESTORE_CHECK_B")
	if observed {
		t.Fatal("custom sink still observed after restore")
	}
}

// TestLookup_OSGetenvBehaviorParity sanity-checks that for non-renamed keys
// we exactly mirror os.Getenv.
func TestLookup_OSGetenvBehaviorParity(t *testing.T) {
	withTestObserver(t)
	keys := []string{"PATH", "HOME", "DOES_NOT_EXIST_XYZ"}
	for _, k := range keys {
		if Lookup(k) != os.Getenv(k) {
			t.Errorf("Lookup(%q) != os.Getenv(%q): %q vs %q", k, k, Lookup(k), os.Getenv(k))
		}
	}
}
