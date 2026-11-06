package server

import (
	"encoding/json"
	"net/http"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// --- ModelAccessTable API (PR-A1a §7) ---
//
// Tables are the shared (provider, model) grant sets referenced from
// Human.spec.modelAccess.scopes[].table. Authorization is handled by the
// RequireAuthz middleware on the "model-access-table" resource kind:
// admin/manager have full access and humans are denied by the authorizer's
// default-deny branch (403) — no authorizer change is needed.

func (h *ResourceHandler) ListModelAccessTables(w http.ResponseWriter, r *http.Request) {
	var list v1beta1.ModelAccessTableList
	if err := h.client.List(r.Context(), &list, client.InNamespace(h.namespace)); err != nil {
		writeK8sError(w, "list model access tables", err)
		return
	}

	tables := make([]ModelAccessTableResponse, 0, len(list.Items))
	for i := range list.Items {
		tables = append(tables, modelAccessTableToResponse(&list.Items[i]))
	}

	httputil.WriteJSON(w, http.StatusOK, ModelAccessTableListResponse{Tables: tables, Total: len(tables)})
}

func (h *ResourceHandler) GetModelAccessTable(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "table name is required")
		return
	}

	var table v1beta1.ModelAccessTable
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &table); err != nil {
		writeK8sError(w, "get model access table", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, modelAccessTableToResponse(&table))
}

func (h *ResourceHandler) CreateModelAccessTable(w http.ResponseWriter, r *http.Request) {
	var req ModelAccessTableRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "name is required")
		return
	}
	spec := v1beta1.ModelAccessTableSpec{
		Main:      req.Main,
		Spawn:     req.Spawn,
		CodingCli: req.CodingCli,
	}
	if err := validateModelAccessTableSpec(&spec); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	table := &v1beta1.ModelAccessTable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: h.namespace,
		},
		Spec: spec,
	}
	h.stampControllerLabel(&table.ObjectMeta)

	if err := h.client.Create(r.Context(), table); err != nil {
		writeK8sError(w, "create model access table", err)
		return
	}

	httputil.WriteJSON(w, http.StatusCreated, modelAccessTableToResponse(table))
}

func (h *ResourceHandler) UpdateModelAccessTable(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "table name is required")
		return
	}

	var req ModelAccessTableRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	spec := v1beta1.ModelAccessTableSpec{
		Main:      req.Main,
		Spawn:     req.Spawn,
		CodingCli: req.CodingCli,
	}
	if err := validateModelAccessTableSpec(&spec); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	for attempt := 0; attempt < k8sUpdateMaxRetries; attempt++ {
		var table v1beta1.ModelAccessTable
		if err := h.client.Get(ctx, client.ObjectKey{Name: name, Namespace: h.namespace}, &table); err != nil {
			writeK8sError(w, "get model access table for update", err)
			return
		}
		// PUT is a full replace of the spec: an empty body clears every
		// surface (same "present = replace" semantics as Human.modelAccess).
		table.Spec = spec
		if err := h.client.Update(ctx, &table); err != nil {
			if apierrors.IsConflict(err) && attempt+1 < k8sUpdateMaxRetries {
				time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
				continue
			}
			writeK8sError(w, "update model access table", err)
			return
		}
		httputil.WriteJSON(w, http.StatusOK, modelAccessTableToResponse(&table))
		return
	}
}

func (h *ResourceHandler) DeleteModelAccessTable(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "table name is required")
		return
	}

	table := &v1beta1.ModelAccessTable{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.namespace},
	}
	if err := h.client.Delete(r.Context(), table); err != nil {
		writeK8sError(w, "delete model access table", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// modelAccessTableToResponse projects a table CR onto its read shape.
func modelAccessTableToResponse(t *v1beta1.ModelAccessTable) ModelAccessTableResponse {
	return ModelAccessTableResponse{
		Name:      t.Name,
		Main:      t.Spec.Main,
		Spawn:     t.Spec.Spawn,
		CodingCli: t.Spec.CodingCli,
	}
}
