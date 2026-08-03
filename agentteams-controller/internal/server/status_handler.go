package server

import (
	"fmt"
	"net/http"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// StatusHandler handles /healthz, /api/v1/status, /api/v1/version.
type StatusHandler struct {
	k8s       client.Client
	namespace string
	kubeMode  string
}

func NewStatusHandler(k8s client.Client, namespace, kubeMode string) *StatusHandler {
	return &StatusHandler{k8s: k8s, namespace: namespace, kubeMode: kubeMode}
}

func (h *StatusHandler) Healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}

type ClusterStatusResponse struct {
	KubeMode     string `json:"kubeMode"`
	TotalWorkers int    `json:"totalWorkers"`
	TotalTeams   int    `json:"totalTeams"`
	TotalHumans  int    `json:"totalHumans"`
}

func (h *StatusHandler) ClusterStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var workers v1beta1.WorkerList
	if err := h.k8s.List(ctx, &workers, client.InNamespace(h.namespace)); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list workers: "+err.Error())
		return
	}

	var teams v1beta1.TeamList
	if err := h.k8s.List(ctx, &teams, client.InNamespace(h.namespace)); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list teams: "+err.Error())
		return
	}

	var humans v1beta1.HumanList
	if err := h.k8s.List(ctx, &humans, client.InNamespace(h.namespace)); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list humans: "+err.Error())
		return
	}

	httputil.WriteJSON(w, http.StatusOK, ClusterStatusResponse{
		KubeMode:     h.kubeMode,
		TotalWorkers: len(workers.Items),
		TotalTeams:   len(teams.Items),
		TotalHumans:  len(humans.Items),
	})
}

type VersionResponse struct {
	Controller string `json:"controller"`
	KubeMode   string `json:"kubeMode"`
}

func (h *StatusHandler) Version(w http.ResponseWriter, _ *http.Request) {
	httputil.WriteJSON(w, http.StatusOK, VersionResponse{
		Controller: "dev",
		KubeMode:   h.kubeMode,
	})
}
