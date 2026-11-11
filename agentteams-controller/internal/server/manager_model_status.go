package server

import (
	"errors"
	"net/http"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Configuration discovery is read-only and never invokes an upstream model.
// Actual model/credential validation uses the saved-identity gateway probe.
type managerModelStatus struct {
	client    client.Client
	namespace string
	gateway   gateway.Client
}

func (h *managerModelStatus) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var manager v1beta1.Manager
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: r.PathValue("name"), Namespace: h.namespace}, &manager); err != nil {
		writeK8sError(w, "get manager", err)
		return
	}
	if h.gateway == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "gateway configuration unavailable")
		return
	}
	routes, err := h.gateway.ListAIRoutes(r.Context())
	if err != nil && !errors.Is(err, gateway.ErrUnsupportedOp) {
		httputil.WriteError(w, http.StatusBadGateway, "cannot read gateway configuration")
		return
	}
	configured := manager.Spec.Model != "" && (len(routes) > 0 || manager.Spec.ModelProvider != "" || errors.Is(err, gateway.ErrUnsupportedOp))
	w.Header().Set("Cache-Control", "no-store")
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"configured": configured, "model": manager.Spec.Model})
}
