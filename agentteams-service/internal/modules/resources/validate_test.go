package resources

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/httputil"
)

func TestValidateName(t *testing.T) {
	longest := strings.Repeat("a", 253)
	tooLong := strings.Repeat("a", 254)
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"", true},
		{"w", false},
		{"worker-1", false},
		{"a0-9z", false},
		{"worker-", false}, // the frozen regexp allows a trailing hyphen
		{"Worker", true},
		{"WORKER", true},
		{"-worker", true},
		{"worker_1", true},
		{"worker.1", true},
		{"worker 1", true},
		{longest, false},
		{tooLong, true},
	}
	for _, tt := range tests {
		err := ValidateName(tt.name)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("ValidateName(%q) = nil, want error", tt.name)
			}
			status, code, _ := ErrorResponse(err)
			if status != http.StatusBadRequest || code != httputil.CodeInvalidName {
				t.Fatalf("ValidateName(%q) = %d/%s, want 400/%s", tt.name, status, code, httputil.CodeInvalidName)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ValidateName(%q) = %v, want nil", tt.name, err)
		}
	}

	if err := ValidateName(""); !errors.Is(err, ErrNameRequired) {
		t.Fatalf("empty name must be ErrNameRequired, got %v", err)
	}
	if _, _, msg := ErrorResponse(ErrNameRequired); msg != "worker name is required" {
		t.Fatalf("empty-name message = %q", msg)
	}
	if _, _, msg := ErrorResponse(ValidateName("Bad_Name")); msg != `invalid worker name: "Bad_Name"` {
		t.Fatalf("invalid-name message = %q", msg)
	}
}

func TestValidateRuntime(t *testing.T) {
	for _, ok := range []string{"", "qwenpaw", "openclaw", "hermes"} {
		if err := ValidateRuntime(ok); err != nil {
			t.Fatalf("ValidateRuntime(%q) = %v, want nil", ok, err)
		}
	}
	for _, copaw := range []string{"copaw", "CoPaw", "COPAW"} {
		err := ValidateRuntime(copaw)
		if err == nil {
			t.Fatalf("ValidateRuntime(%q) = nil, want error", copaw)
		}
		status, code, msg := ErrorResponse(err)
		if status != http.StatusBadRequest || code != httputil.CodeDeprecatedRuntime {
			t.Fatalf("ValidateRuntime(%q) = %d/%s, want 400/%s", copaw, status, code, httputil.CodeDeprecatedRuntime)
		}
		if !strings.Contains(msg, "migrate to qwenpaw") {
			t.Fatalf("ValidateRuntime(%q) message = %q", copaw, msg)
		}
	}
}

func TestValidateEnv(t *testing.T) {
	// nil env never needs the admin/manager gate (controller
	// worker_env.go:23-25).
	if err := ValidateEnv(nil, false); err != nil {
		t.Fatalf("nil env for non-manager = %v", err)
	}

	// A present map is gated: even an empty one is 403 for non-managers,
	// matching the controller's env == nil check.
	if err := ValidateEnv(map[string]string{}, false); !errors.Is(err, ErrEnvForbidden) {
		t.Fatalf("empty env for non-manager = %v, want ErrEnvForbidden", err)
	}
	status, code, msg := ErrorResponse(ErrEnvForbidden)
	if status != http.StatusForbidden || code != httputil.CodeForbidden ||
		msg != "environment variables require admin or manager access" {
		t.Fatalf("ErrEnvForbidden maps to %d/%s/%q", status, code, msg)
	}

	for _, env := range []map[string]string{
		{"FOO": "bar"},
		{"_PRIVATE": "v"},
		{"A1_b": "v"},
		{"EMPTY_VALUE": ""},
		{"WITH_SPACES": "a b c"},
	} {
		if err := ValidateEnv(env, true); err != nil {
			t.Fatalf("ValidateEnv(%v) = %v, want nil", env, err)
		}
	}

	for key := range reservedEnvKeys {
		err := ValidateEnv(map[string]string{key: "v"}, true)
		if err == nil {
			t.Fatalf("reserved key %q accepted", key)
		}
		status, code, msg := ErrorResponse(err)
		if status != http.StatusBadRequest || code != httputil.CodeInvalidEnv {
			t.Fatalf("reserved key %q = %d/%s, want 400/%s", key, status, code, httputil.CodeInvalidEnv)
		}
		if msg != "environment variable is controlled by the system: "+key {
			t.Fatalf("reserved key %q message = %q", key, msg)
		}
	}

	for _, bad := range []map[string]string{
		{"1BAD": "v"},
		{"BAD-KEY": "v"},
		{"BAD KEY": "v"},
		{"": "v"},
		{"GOOD": "nul\x00inside"},
	} {
		err := ValidateEnv(bad, true)
		if err == nil {
			t.Fatalf("ValidateEnv(%v) = nil, want error", bad)
		}
		status, code, msg := ErrorResponse(err)
		if status != http.StatusBadRequest || code != httputil.CodeInvalidEnv {
			t.Fatalf("ValidateEnv(%v) = %d/%s, want 400/%s", bad, status, code, httputil.CodeInvalidEnv)
		}
		if msg != "invalid environment variable name or NUL in value" {
			t.Fatalf("ValidateEnv(%v) message = %q", bad, msg)
		}
	}
}

func TestReservedEnvKeysCoverControllerContract(t *testing.T) {
	// 5 explicit system keys (worker_env.go:35) + WorkerEnvBuilder.Build
	// keys (service/worker_env.go:33-44); see reservedEnvKeys.
	for _, key := range []string{
		"AGENTTEAMS_WORKER_CR_NAME",
		"AGENTTEAMS_WORKER_ROLE",
		"AGENTTEAMS_AUTH_TOKEN",
		"AGENTTEAMS_AUTH_TOKEN_FILE",
		"AGENTTEAMS_CONTROLLER_URL",
		"AGENTTEAMS_WORKER_NAME",
		"AGENTTEAMS_WORKER_GATEWAY_KEY",
		"AGENTTEAMS_WORKER_MATRIX_TOKEN",
		"AGENTTEAMS_WORKER_ROOM_ID",
		"AGENTTEAMS_FS_ACCESS_KEY",
		"AGENTTEAMS_FS_SECRET_KEY",
		"OPENCLAW_DISABLE_BONJOUR",
		"OPENCLAW_MDNS_HOSTNAME",
		"AGENTTEAMS_CONSOLE_PORT",
		"HOME",
	} {
		if _, ok := reservedEnvKeys[key]; !ok {
			t.Fatalf("%q missing from reservedEnvKeys", key)
		}
	}
	if len(reservedEnvKeys) != 15 {
		t.Fatalf("reservedEnvKeys has %d entries, want 15", len(reservedEnvKeys))
	}
}

func TestErrorResponse(t *testing.T) {
	custom := &HTTPError{Status: 409, Code: "already_exists", Message: "boom"}
	status, code, msg := ErrorResponse(custom)
	if status != 409 || code != "already_exists" || msg != "boom" {
		t.Fatalf("custom = %d/%s/%q", status, code, msg)
	}

	status, code, msg = ErrorResponse(errors.New("plain"))
	if status != 400 || code != httputil.CodeInvalidRequest || msg != "plain" {
		t.Fatalf("generic = %d/%s/%q", status, code, msg)
	}

	status, code, msg = ErrorResponse(nil)
	if status != 500 || code != httputil.CodeInternal || msg != "internal error" {
		t.Fatalf("nil = %d/%s/%q", status, code, msg)
	}
}
