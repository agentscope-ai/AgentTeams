package server

import (
	"net/http"
	"regexp"
	"strings"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/config"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

var workerEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func canManageWorkerEnv(r *http.Request) bool {
	caller := authpkg.CallerFromContext(r.Context())
	// Authentication is enforced by the router. Nil supports direct handler tests.
	return caller == nil || caller.Role == authpkg.RoleAdmin || caller.Role == authpkg.RoleManager
}

func (h *ResourceHandler) validateWorkerEnvRequest(w http.ResponseWriter, r *http.Request, env map[string]string) bool {
	if env == nil {
		return true
	}
	if !canManageWorkerEnv(r) {
		httputil.WriteError(w, http.StatusForbidden, "environment variables require admin or manager access")
		return false
	}
	builder := h.workerEnvBuilder
	if builder == nil {
		builder = service.NewWorkerEnvBuilder(config.WorkerEnvDefaults{})
	}
	system := builder.Build("", &service.WorkerProvisionResult{})
	for _, key := range []string{"AGENTTEAMS_WORKER_CR_NAME", "AGENTTEAMS_WORKER_ROLE", "AGENTTEAMS_AUTH_TOKEN", "AGENTTEAMS_AUTH_TOKEN_FILE", "AGENTTEAMS_CONTROLLER_URL"} {
		system[key] = ""
	}
	for key, value := range env {
		if _, reserved := system[key]; reserved {
			httputil.WriteError(w, http.StatusBadRequest, "environment variable is controlled by the system: "+key)
			return false
		}
		if !workerEnvName.MatchString(key) || strings.ContainsRune(value, '\x00') {
			httputil.WriteError(w, http.StatusBadRequest, "invalid environment variable name or NUL in value")
			return false
		}
	}
	return true
}
