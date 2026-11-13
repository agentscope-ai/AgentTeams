package resources

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/httputil"
)

// Runtime names used by the frozen R1 contract (58 §1.1/§2, aligned with
// agentteams-controller internal/backend/interface.go:35-39).
const (
	runtimeOpenClaw = "openclaw"
	runtimeCopaw    = "copaw"
)

// workerNameRE is the create-name constraint frozen in 58 §1.1
// (cmd/agt/create.go:433-439): lowercase letter/digit first, then
// lowercase letters, digits and hyphens. Length is capped at 253 (the
// res.workers.name VARCHAR(253) / K8s DNS-1035 label limit) by ValidateName.
var workerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// workerEnvNameRE mirrors agentteams-controller
// internal/server/worker_env.go:14.
var workerEnvNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedEnvKeys is the frozen reserved-key set of 58 §2 / worker_env.go:34-36:
// the five explicit AGENTTEAMS_* system keys plus every key that
// WorkerEnvBuilder.Build("", &WorkerProvisionResult{}) installs with the
// production defaults (controller internal/service/worker_env.go:32-48).
//
// Note (R1 deviation): WorkerEnvBuilder.applyClusterDefaults only adds the
// AGENTTEAMS_YOLO / MATRIX_DEBUG / CMS_* / SKILLS_API_URL / NACOS_AUTH_TYPE /
// matrix-domain style keys when the controller's cluster config sets a
// non-empty value; the service has no such cluster config in R1, so those
// conditionally-present keys are intentionally NOT reserved. A deployment
// that turns one on should extend this set (R2 wires the same defaults in).
var reservedEnvKeys = map[string]struct{}{
	// Explicit system keys (worker_env.go:35).
	"AGENTTEAMS_WORKER_CR_NAME":  {},
	"AGENTTEAMS_WORKER_ROLE":     {},
	"AGENTTEAMS_AUTH_TOKEN":      {},
	"AGENTTEAMS_AUTH_TOKEN_FILE": {},
	"AGENTTEAMS_CONTROLLER_URL":  {},
	// WorkerEnvBuilder.Build keys (service/worker_env.go:33-44).
	"AGENTTEAMS_WORKER_NAME":         {},
	"AGENTTEAMS_WORKER_GATEWAY_KEY":  {},
	"AGENTTEAMS_WORKER_MATRIX_TOKEN": {},
	"AGENTTEAMS_WORKER_ROOM_ID":      {},
	"AGENTTEAMS_FS_ACCESS_KEY":       {},
	"AGENTTEAMS_FS_SECRET_KEY":       {},
	"OPENCLAW_DISABLE_BONJOUR":       {},
	"OPENCLAW_MDNS_HOSTNAME":         {},
	"AGENTTEAMS_CONSOLE_PORT":        {},
	"HOME":                           {},
}

// HTTPError is a classified handler error carrying the frozen 58 §2 envelope
// fields, so handlers never scatter status/code/message decisions.
type HTTPError struct {
	Status  int
	Code    string
	Message string
}

// Error implements the error interface.
func (e *HTTPError) Error() string { return e.Message }

// Frozen validation/authz errors shared by the handlers and their tests.
var (
	// ErrEnvForbidden is returned when a non-admin/manager caller supplies
	// env (worker_env.go:26-28; 58 §2 forbidden/403).
	ErrEnvForbidden = &HTTPError{
		Status:  http.StatusForbidden,
		Code:    httputil.CodeForbidden,
		Message: "environment variables require admin or manager access",
	}
	// ErrNameRequired is the empty-name create error (58 §2 invalid_name).
	ErrNameRequired = &HTTPError{
		Status:  http.StatusBadRequest,
		Code:    httputil.CodeInvalidName,
		Message: "worker name is required",
	}
	// ErrWorkerNameRequired is the empty {name} path error
	// (resource_handler.go:235).
	ErrWorkerNameRequired = &HTTPError{
		Status:  http.StatusBadRequest,
		Code:    httputil.CodeInvalidName,
		Message: "worker name is required",
	}
)

// ValidateName validates a create name against the frozen constraint.
//
// Messages follow 58 §2 invalid_name: "worker name is required" for an
// empty name and "invalid worker name: ..." for a name that violates the
// regexp or the 253-byte limit.
func ValidateName(name string) error {
	if name == "" {
		return ErrNameRequired
	}
	if len(name) > 253 || !workerNameRE.MatchString(name) {
		return &HTTPError{
			Status:  http.StatusBadRequest,
			Code:    httputil.CodeInvalidName,
			Message: fmt.Sprintf("invalid worker name: %q", name),
		}
	}
	return nil
}

// ValidateRuntime rejects the deprecated CoPaw runtime for new workers
// (58 §2 deprecated_runtime; controller resource_handler.go:160-166).
// The comparison is case-insensitive, matching strings.EqualFold in the
// controller.
func ValidateRuntime(runtime string) error {
	if strings.EqualFold(runtime, runtimeCopaw) {
		return &HTTPError{
			Status:  http.StatusBadRequest,
			Code:    httputil.CodeDeprecatedRuntime,
			Message: fmt.Sprintf("runtime %q is deprecated; migrate to qwenpaw", runtime),
		}
	}
	return nil
}

// ValidateEnv validates the create/update env map (58 §1.1/§2).
//
//   - nil env is always allowed (nothing to validate, nothing written);
//   - a non-admin/manager caller gets 403 forbidden before key inspection;
//   - reserved keys win over shape validation, exactly like the controller
//     (worker_env.go:38-46) → 400 invalid_env "environment variable is
//     controlled by the system: <key>";
//   - keys must match ^[A-Za-z_][A-Za-z0-9_]*$ and values must not contain
//     NUL → 400 invalid_env.
//
// Map iteration order is unspecified; the returned error is whichever
// offending entry is visited first, which is fine because every violation
// is a 400 invalid_env.
func ValidateEnv(env map[string]string, canManage bool) error {
	if env == nil {
		return nil
	}
	if !canManage {
		return ErrEnvForbidden
	}
	for key, value := range env {
		if _, reserved := reservedEnvKeys[key]; reserved {
			return &HTTPError{
				Status:  http.StatusBadRequest,
				Code:    httputil.CodeInvalidEnv,
				Message: "environment variable is controlled by the system: " + key,
			}
		}
		if !workerEnvNameRE.MatchString(key) || strings.ContainsRune(value, '\x00') {
			return &HTTPError{
				Status:  http.StatusBadRequest,
				Code:    httputil.CodeInvalidEnv,
				Message: "invalid environment variable name or NUL in value",
			}
		}
	}
	return nil
}

// ErrorResponse maps any error to (status, code, message) for
// httputil.WriteError. A classified *HTTPError keeps its frozen fields; every
// other error falls back to 400 invalid_request with its message (the
// controller's bare invalid-JSON/parse errors).
func ErrorResponse(err error) (status int, code, message string) {
	var e *HTTPError
	if errors.As(err, &e) {
		return e.Status, e.Code, e.Message
	}
	if err == nil {
		return http.StatusInternalServerError, httputil.CodeInternal, "internal error"
	}
	return http.StatusBadRequest, httputil.CodeInvalidRequest, err.Error()
}
