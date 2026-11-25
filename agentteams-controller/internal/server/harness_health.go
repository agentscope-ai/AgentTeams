package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/modelhealth"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Harness health travels through the existing private AgentFS member scope.
// No container exec, inbound port or vendor-specific model request is required.
type harnessHealth struct {
	client    client.Client
	namespace string
	store     modelhealth.Store
	manager   bool
}

func (h *harnessHealth) identity(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	name := r.PathValue("name")
	if h.manager {
		var m v1beta1.Manager
		if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &m); err != nil {
			writeK8sError(w, "get manager", err)
			return "", "", false
		}
		return "manager", m.Spec.Runtime, true
	}
	var worker v1beta1.Worker
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
		writeK8sError(w, "get worker", err)
		return "", "", false
	}
	return worker.Spec.EffectiveWorkerName(worker.Name), worker.Spec.Runtime, true
}

func (h *harnessHealth) Get(w http.ResponseWriter, r *http.Request) {
	member, runtime, ok := h.identity(w, r)
	if !ok {
		return
	}
	if h.store == nil {
		httputil.WriteError(w, 503, "Harness health storage unavailable")
		return
	}
	report := modelhealth.Unknown("missing_report")
	b, err := h.store.GetObject(r.Context(), "agents/"+member+"/runtime/health.json")
	if err == nil {
		report = modelhealth.Parse(b, runtime, member, time.Now().UTC())
	} else if !os.IsNotExist(err) {
		httputil.WriteError(w, 503, "Harness health report unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httputil.WriteJSON(w, 200, report)
}

func (h *harnessHealth) Probe(w http.ResponseWriter, r *http.Request) {
	if !canManageWorkerEnv(r) {
		httputil.WriteError(w, 403, "Harness probes require admin or manager access")
		return
	}
	member, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	if h.store == nil {
		httputil.WriteError(w, 503, "Harness health storage unavailable")
		return
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		httputil.WriteError(w, 503, "Harness probe identity unavailable")
		return
	}
	request := modelhealth.Request{Version: 1, ID: hex.EncodeToString(id[:]), ExpiresAt: time.Now().UTC().Add(2 * time.Minute)}
	data, _ := json.Marshal(request)
	if h.store.PutObject(r.Context(), "agents/"+member+"/runtime/health-request.json", data) != nil {
		httputil.WriteError(w, 503, "Harness probe request unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httputil.WriteJSON(w, http.StatusAccepted, map[string]any{"status": "pending", "queued": true, "request_id": request.ID, "expires_at": request.ExpiresAt, "message": "Harness model check queued; read the member health result"})
}
