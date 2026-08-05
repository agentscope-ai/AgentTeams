package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	managedAgentIdentityRequestMaxBytes = 4 * 1024
	matrixUserIDMaxBytes                = 255
)

// ManagedAgentIdentityHandler answers one exact, controller-scoped identity
// membership question for a self-authenticated Worker runtime.
type ManagedAgentIdentityHandler struct {
	client         client.Client
	namespace      string
	controllerName string
}

type ManagedAgentIdentityRequest struct {
	MatrixUserID string `json:"matrixUserId"`
}

type ManagedAgentIdentityResponse struct {
	Managed bool `json:"managed"`
}

func NewManagedAgentIdentityHandler(k8s client.Client, namespace, controllerName string) *ManagedAgentIdentityHandler {
	return &ManagedAgentIdentityHandler{client: k8s, namespace: namespace, controllerName: controllerName}
}

func (h *ManagedAgentIdentityHandler) Lookup(w http.ResponseWriter, r *http.Request) {
	caller := authpkg.CallerFromContext(r.Context())
	workerName := r.PathValue("name")
	if caller == nil || (caller.Role != authpkg.RoleWorker && caller.Role != authpkg.RoleTeamLeader) || caller.Username != workerName {
		httputil.WriteError(w, http.StatusForbidden, "managed-Agent identity lookup is restricted to the calling Worker")
		return
	}

	var callerWorker v1beta1.Worker
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: workerName, Namespace: h.namespace}, &callerWorker); err != nil {
		writeK8sError(w, "get calling worker", err)
		return
	}
	if h.controllerName != "" && callerWorker.Labels[v1beta1.LabelController] != h.controllerName {
		httputil.WriteError(w, http.StatusForbidden, "calling Worker is outside this controller scope")
		return
	}

	request, err := decodeManagedAgentIdentityRequest(w, r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid managed-Agent identity request: "+err.Error())
		return
	}

	options := []client.ListOption{client.InNamespace(h.namespace)}
	if h.controllerName != "" {
		options = append(options, client.MatchingLabels{v1beta1.LabelController: h.controllerName})
	}
	var workers v1beta1.WorkerList
	if err := h.client.List(r.Context(), &workers, options...); err != nil {
		writeK8sError(w, "list managed Workers", err)
		return
	}
	for i := range workers.Items {
		if workers.Items[i].Status.MatrixUserID == request.MatrixUserID {
			httputil.WriteJSON(w, http.StatusOK, ManagedAgentIdentityResponse{Managed: true})
			return
		}
	}

	var managers v1beta1.ManagerList
	if err := h.client.List(r.Context(), &managers, options...); err != nil {
		writeK8sError(w, "list managed Managers", err)
		return
	}
	for i := range managers.Items {
		if managers.Items[i].Status.MatrixUserID == request.MatrixUserID {
			httputil.WriteJSON(w, http.StatusOK, ManagedAgentIdentityResponse{Managed: true})
			return
		}
	}
	// This endpoint intentionally returns only the answer to the caller's one
	// bounded question; it never exposes the managed identity set.
	httputil.WriteJSON(w, http.StatusOK, ManagedAgentIdentityResponse{Managed: false})
}

func decodeManagedAgentIdentityRequest(w http.ResponseWriter, r *http.Request) (ManagedAgentIdentityRequest, error) {
	var request ManagedAgentIdentityRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, managedAgentIdentityRequestMaxBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return request, errors.New("request must contain exactly one JSON object")
		}
		return request, err
	}
	if err := validateExactMatrixUserID(request.MatrixUserID); err != nil {
		return request, err
	}
	return request, nil
}

func validateExactMatrixUserID(value string) error {
	if value == "" || len([]byte(value)) > matrixUserIDMaxBytes {
		return errors.New("matrixUserId must be between 1 and 255 bytes")
	}
	if value != strings.TrimSpace(value) || value[0] != '@' {
		return errors.New("matrixUserId must be one exact Matrix user ID")
	}
	separator := strings.IndexByte(value, ':')
	if separator < 2 || separator == len(value)-1 {
		return errors.New("matrixUserId must be one exact Matrix user ID")
	}
	for _, char := range value {
		if unicode.IsSpace(char) || unicode.IsControl(char) {
			return errors.New("matrixUserId must be one exact Matrix user ID")
		}
	}
	return nil
}
