package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// managerPluginsHandler proxies the manager's qwenpaw plugin registry.
// Managers are cluster-level resources (no TeamName), so there is no W8
// team boundary here; the route's manager authz is the scope. The port is
// the qwenpaw manager app (18799); non-qwenpaw managers are rejected
// because that port is a gateway, not a plugin API.
type managerPluginsHandler struct {
	rh       *ResourceHandler
	kubeMode string
	baseURL  func(name string) string
	p        *workerPluginsProxy
}

// GetManagerPlugins returns the read-only handler for
//
//	GET /api/v1/managers/{name}/plugins
func (h *ResourceHandler) GetManagerPlugins(kubeMode, resourcePrefix string) http.HandlerFunc {
	return (&managerPluginsHandler{
		rh:       h,
		kubeMode: kubeMode,
		baseURL: func(name string) string {
			return fmt.Sprintf("http://%s:18799", authpkg.ResourcePrefix(resourcePrefix).ManagerPodName(name))
		},
		p: newWorkerPluginsProxy(h, kubeMode, ""),
	}).serve
}

func (m *managerPluginsHandler) serve(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" || !workerNamePattern.MatchString(name) {
		httputil.WriteError(w, http.StatusBadRequest, "manager name is required and must be a valid DNS label")
		return
	}
	if m.kubeMode != "embedded" {
		httputil.WriteError(w, http.StatusServiceUnavailable, "manager plugin registry requires embedded mode")
		return
	}

	var manager v1beta1.Manager
	if err := m.rh.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: m.rh.namespace}, &manager); err != nil {
		if apierrors.IsNotFound(err) {
			httputil.WriteError(w, http.StatusNotFound, "manager not found")
			return
		}
		writeK8sError(w, "get manager plugins", err)
		return
	}
	if rt := manager.Spec.Runtime; rt != "" && rt != "qwenpaw" {
		httputil.WriteError(w, http.StatusBadRequest, "manager plugin registry is only supported for qwenpaw managers")
		return
	}

	entries, ferr := m.p.fetchProjection(r.Context(), m.baseURL(name))
	if ferr != nil {
		httputil.WriteError(w, ferr.status, ferr.message)
		return
	}
	projected, err := json.Marshal(projectPluginMetadata(entries))
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "project manager plugins metadata")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(projected)
}
