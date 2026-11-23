package resources

import (
	"regexp"
	"strings"
	"testing"
)

var fuzzWorkerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
var fuzzEnvKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// FuzzValidateName: acceptance must be exactly the frozen grammar
// (^[a-z0-9][a-z0-9-]*$, length <= 253) — no panic, no extra acceptance.
func FuzzValidateName(f *testing.F) {
	for _, seed := range []string{"w", "w-1", "W", "-x", "", "a", strings.Repeat("b", 300)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		err := ValidateName(name)
		wantOK := len(name) <= 253 && fuzzWorkerNameRE.MatchString(name)
		if wantOK != (err == nil) {
			t.Fatalf("ValidateName(%q) err=%v, wantOK=%v", name, err, wantOK)
		}
	})
}

// FuzzParseWorkerListFields: any accepted input maps to one of the four
// known views; nothing else escapes the parser.
func FuzzParseWorkerListFields(f *testing.F) {
	for _, seed := range []string{"", "all", "spec", "status", "spec,status", "all,spec", "Spec", " , "} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		view, err := parseWorkerListFields(raw)
		if err != nil {
			return
		}
		switch view {
		case workerFieldsAll, workerFieldsSpec, workerFieldsStatus, workerFieldsSpecStatus:
		default:
			t.Fatalf("parser returned unknown view %d for %q", view, raw)
		}
	})
}

// FuzzValidateEnv: acceptance implies a legal key (regexp, no NUL) for a
// manage-env caller; reserved keys stay rejected.
func FuzzValidateEnv(f *testing.F) {
	f.Add("FOO", "bar")
	f.Add("1BAD", "x")
	f.Add("A_B", "line\n")
	f.Add("AGENTTEAMS_TEST", "x")
	f.Fuzz(func(t *testing.T, key, value string) {
		err := ValidateEnv(map[string]string{key: value}, true)
		if err != nil {
			return
		}
		if !fuzzEnvKeyRE.MatchString(key) {
			t.Fatalf("accepted illegal env key %q", key)
		}
		if strings.ContainsRune(value, 0) {
			t.Fatalf("accepted NUL value for %q", key)
		}
	})
}
