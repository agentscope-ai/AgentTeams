package server

import (
	"net/http"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/config"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

func (h *ResourceHandler) validateManagerEnvRequest(w http.ResponseWriter, r *http.Request, env map[string]string) bool {
	if env == nil {
		return true
	}
	if !h.validateWorkerEnvRequest(w, r, env) {
		return false
	}
	builder := h.workerEnvBuilder
	if builder == nil {
		builder = service.NewWorkerEnvBuilder(config.WorkerEnvDefaults{})
	}
	// Include conditional model/runtime keys in the protected system environment.
	system := builder.BuildManager("", &service.ManagerProvisionResult{}, v1beta1.ManagerSpec{
		Model: "reserved", Runtime: "qwenpaw",
		Config: v1beta1.ManagerConfig{HeartbeatInterval: "reserved", WorkerIdleTimeout: "reserved", NotifyChannel: "reserved"},
	})
	for key := range env {
		if _, reserved := system[key]; reserved {
			httputil.WriteError(w, http.StatusBadRequest, "environment variable is controlled by the system: "+key)
			return false
		}
	}
	return true
}
