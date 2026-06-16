package envcompat

import (
	"testing"
)

func TestLookup_NewKeyPrecedence(t *testing.T) {
	resetDeprecationWarningsForTest()
	t.Setenv("AGENTTEAMS_FOO_BAR", "new")
	t.Setenv("HICLAW_FOO_BAR", "old")
	if got := Lookup("HICLAW_FOO_BAR"); got != "new" {
		t.Fatalf("Lookup = %q, want %q", got, "new")
	}
}

func TestLookup_FallsBackToLegacy(t *testing.T) {
	resetDeprecationWarningsForTest()
	t.Setenv("HICLAW_LEGACY_ONLY", "value")
	if got := Lookup("HICLAW_LEGACY_ONLY"); got != "value" {
		t.Fatalf("Lookup = %q, want %q", got, "value")
	}
}

func TestLookup_EmptyWhenUnset(t *testing.T) {
	resetDeprecationWarningsForTest()
	if got := Lookup("HICLAW_NEVER_SET_XYZ"); got != "" {
		t.Fatalf("Lookup = %q, want empty", got)
	}
}

func TestLookup_NonRenamedKeyPassesThrough(t *testing.T) {
	t.Setenv("SOME_OTHER_VAR", "plain")
	if got := Lookup("SOME_OTHER_VAR"); got != "plain" {
		t.Fatalf("Lookup = %q, want %q", got, "plain")
	}
}

func TestOrDefault(t *testing.T) {
	resetDeprecationWarningsForTest()
	if got := OrDefault("HICLAW_MISSING_KEY", "fallback"); got != "fallback" {
		t.Fatalf("OrDefault = %q, want %q", got, "fallback")
	}
	t.Setenv("AGENTTEAMS_PRESENT", "set")
	if got := OrDefault("HICLAW_PRESENT", "fallback"); got != "set" {
		t.Fatalf("OrDefault = %q, want %q", got, "set")
	}
}

func TestOrDefaultInt(t *testing.T) {
	resetDeprecationWarningsForTest()
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

func TestBool(t *testing.T) {
	resetDeprecationWarningsForTest()
	cases := []struct {
		val  string
		want bool
	}{
		{"1", true}, {"true", true}, {"True", true}, {"TRUE", true},
		{"0", false}, {"false", false}, {"", false}, {"yes", false},
	}
	for _, c := range cases {
		t.Setenv("AGENTTEAMS_FLAG", c.val)
		if got := Bool("HICLAW_FLAG"); got != c.want {
			t.Errorf("Bool(%q) = %v, want %v", c.val, got, c.want)
		}
	}
}

func TestBoolDefault(t *testing.T) {
	resetDeprecationWarningsForTest()
	if got := BoolDefault("HICLAW_FLAG_UNSET", true); !got {
		t.Fatalf("BoolDefault unset = %v, want true (default)", got)
	}
	t.Setenv("AGENTTEAMS_FLAG_OVERRIDE", "false")
	if got := BoolDefault("HICLAW_FLAG_OVERRIDE", true); got {
		t.Fatalf("BoolDefault = %v, want false (env override)", got)
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
	}
	for _, c := range cases {
		got, ok := translateKey(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("translateKey(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
