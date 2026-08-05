package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/sandboxpolicy"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var executionSandboxSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type ExecutionSandboxHandler struct {
	client           client.Client
	namespace        string
	controllerName   string
	defaultRuntime   string
	ephemeralStorage sandboxpolicy.Policy
	egressCeilings   []v1beta1.DeepAgentsEgressRule
}

type ExecutionSandboxEnsureRequest struct {
	SessionID string `json:"sessionId"`
}

type ExecutionSandboxResponse struct {
	Name     string `json:"name"`
	Phase    string `json:"phase"`
	Endpoint string `json:"endpoint,omitempty"`
	Token    string `json:"token,omitempty"`
}

func NewExecutionSandboxHandler(
	k8s client.Client,
	namespace string,
	controllerName string,
	defaultRuntime string,
	ephemeralStorage sandboxpolicy.Policy,
	egressCeilings []v1beta1.DeepAgentsEgressRule,
) *ExecutionSandboxHandler {
	return &ExecutionSandboxHandler{
		client: k8s, namespace: namespace, controllerName: controllerName, defaultRuntime: defaultRuntime,
		ephemeralStorage: ephemeralStorage, egressCeilings: deepCopyEgressRules(egressCeilings),
	}
}

func (h *ExecutionSandboxHandler) Ensure(w http.ResponseWriter, r *http.Request) {
	worker, ok := h.deepAgentsWorker(w, r)
	if !ok {
		return
	}
	var request ExecutionSandboxEnsureRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid execution sandbox request: "+err.Error())
		return
	}
	if !executionSandboxSessionIDPattern.MatchString(request.SessionID) {
		httputil.WriteError(w, http.StatusBadRequest, "sessionId must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
		return
	}

	name := executionSandboxName(worker.Name, request.SessionID)
	key := client.ObjectKey{Name: name, Namespace: h.namespace}
	var sandbox v1beta1.ExecutionSandbox
	err := h.client.Get(r.Context(), key, &sandbox)
	if apierrors.IsNotFound(err) {
		desiredSpec, err := h.desiredExecutionSandboxSpec(worker, request.SessionID)
		if err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "invalid execution sandbox policy: "+err.Error())
			return
		}
		controller := true
		blockDeletion := true
		labels := map[string]string{
			v1beta1.LabelWorker:           worker.Name,
			v1beta1.LabelExecutionSandbox: name,
		}
		if h.controllerName != "" {
			labels[v1beta1.LabelController] = h.controllerName
		}
		sandbox = v1beta1.ExecutionSandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: h.namespace,
				Labels:    labels,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion:         v1beta1.SchemeGroupVersion.String(),
					Kind:               "Worker",
					Name:               worker.Name,
					UID:                worker.UID,
					Controller:         &controller,
					BlockOwnerDeletion: &blockDeletion,
				}},
			},
			Spec: desiredSpec,
		}
		if err := h.client.Create(r.Context(), &sandbox); err != nil {
			writeK8sError(w, "create execution sandbox", err)
			return
		}
		httputil.WriteJSON(w, http.StatusAccepted, ExecutionSandboxResponse{Name: name, Phase: "Pending"})
		return
	}
	if err != nil {
		writeK8sError(w, "get execution sandbox", err)
		return
	}
	if !h.ownsControllerLabel(sandbox.Labels) {
		httputil.WriteError(w, http.StatusConflict, "execution sandbox belongs to another controller")
		return
	}
	if sandbox.Spec.WorkerRef.Name != worker.Name || sandbox.Spec.WorkerRef.UID != string(worker.UID) ||
		sandbox.Spec.SessionID != request.SessionID {
		httputil.WriteError(w, http.StatusConflict, "execution sandbox identity collision")
		return
	}
	desiredSpec, err := h.desiredExecutionSandboxSpec(worker, request.SessionID)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid execution sandbox policy: "+err.Error())
		return
	}
	if !executionSandboxPolicyMatches(sandbox.Spec, desiredSpec) {
		sandbox.Spec.WorkerRef = desiredSpec.WorkerRef
		sandbox.Spec.SessionID = desiredSpec.SessionID
		sandbox.Spec.IdleTimeout = desiredSpec.IdleTimeout
		sandbox.Spec.MaxLifetime = desiredSpec.MaxLifetime
		sandbox.Spec.Resources = desiredSpec.Resources
		sandbox.Spec.Egress = desiredSpec.Egress
		if err := h.client.Update(r.Context(), &sandbox); err != nil {
			writeK8sError(w, "update execution sandbox policy", err)
			return
		}
		httputil.WriteJSON(w, http.StatusAccepted, ExecutionSandboxResponse{Name: sandbox.Name, Phase: "Pending"})
		return
	}

	response := ExecutionSandboxResponse{
		Name:     sandbox.Name,
		Phase:    sandbox.Status.Phase,
		Endpoint: sandbox.Status.Endpoint,
	}
	if response.Phase == "" {
		response.Phase = "Pending"
	}
	status := http.StatusAccepted
	if response.Phase == "Ready" && sandbox.Status.ObservedGeneration == sandbox.Generation {
		var secret corev1.Secret
		if err := h.client.Get(r.Context(), key, &secret); err != nil {
			writeK8sError(w, "get execution sandbox runner token", err)
			return
		}
		token := secret.Data["token"]
		if len(token) < 32 {
			httputil.WriteError(w, http.StatusServiceUnavailable, "execution sandbox runner token is not ready")
			return
		}
		response.Token = string(token)
		status = http.StatusOK
	} else if response.Phase == "Ready" {
		response.Phase = "Pending"
		response.Endpoint = ""
	}
	httputil.WriteJSON(w, status, response)
}

func (h *ExecutionSandboxHandler) desiredExecutionSandboxSpec(
	worker *v1beta1.Worker,
	sessionID string,
) (v1beta1.ExecutionSandboxSpec, error) {
	execution := worker.Spec.RuntimeConfig.DeepAgents.Execution.DeepCopy()
	if err := sandboxpolicy.ValidateExecutionDurations(*execution); err != nil {
		return v1beta1.ExecutionSandboxSpec{}, err
	}
	if _, err := sandboxpolicy.IntersectEgress(execution.Egress, h.egressCeilings); err != nil {
		return v1beta1.ExecutionSandboxSpec{}, err
	}
	effectiveResources, _, _, err := h.ephemeralStorage.Resolve(execution.Resources)
	if err != nil {
		return v1beta1.ExecutionSandboxSpec{}, err
	}
	return v1beta1.ExecutionSandboxSpec{
		WorkerRef:   v1beta1.ExecutionSandboxWorkerRef{Name: worker.Name, UID: string(worker.UID)},
		SessionID:   sessionID,
		IdleTimeout: execution.IdleTimeout,
		MaxLifetime: execution.MaxLifetime,
		Resources:   effectiveResources,
		Egress:      deepCopyEgressRules(execution.Egress),
	}, nil
}

func executionSandboxPolicyMatches(
	existing v1beta1.ExecutionSandboxSpec,
	desired v1beta1.ExecutionSandboxSpec,
) bool {
	return existing.WorkerRef == desired.WorkerRef &&
		existing.SessionID == desired.SessionID &&
		existing.IdleTimeout == desired.IdleTimeout &&
		existing.MaxLifetime == desired.MaxLifetime &&
		apiequality.Semantic.DeepEqual(existing.Resources, desired.Resources) &&
		apiequality.Semantic.DeepEqual(existing.Egress, desired.Egress)
}

func (h *ExecutionSandboxHandler) Heartbeat(w http.ResponseWriter, r *http.Request) {
	sandbox, ok := h.workerSandboxFromPath(w, r)
	if !ok {
		return
	}
	base := sandbox.DeepCopy()
	now := metav1.NewTime(time.Now().UTC())
	sandbox.Status.LastHeartbeat = &now
	if err := h.client.Status().Patch(r.Context(), sandbox, client.MergeFrom(base)); err != nil {
		writeK8sError(w, "update execution sandbox heartbeat", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *ExecutionSandboxHandler) Delete(w http.ResponseWriter, r *http.Request) {
	sandbox, ok := h.workerSandboxFromPath(w, r)
	if !ok {
		return
	}
	if err := h.client.Delete(r.Context(), sandbox); err != nil && !apierrors.IsNotFound(err) {
		writeK8sError(w, "delete execution sandbox", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *ExecutionSandboxHandler) deepAgentsWorker(w http.ResponseWriter, r *http.Request) (*v1beta1.Worker, bool) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required")
		return nil, false
	}
	var worker v1beta1.Worker
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
		writeK8sError(w, "get worker", err)
		return nil, false
	}
	if !h.ownsControllerLabel(worker.Labels) {
		httputil.WriteError(w, http.StatusConflict, "worker belongs to another controller")
		return nil, false
	}
	if backend.ResolveRuntime(worker.Spec.Runtime, h.defaultRuntime) != backend.RuntimeDeepAgents || worker.Spec.RuntimeConfig == nil ||
		worker.Spec.RuntimeConfig.DeepAgents == nil || worker.Spec.RuntimeConfig.DeepAgents.Execution.Mode != "sandbox" {
		httputil.WriteError(w, http.StatusConflict, "worker is not a deepagents runtime with sandbox execution enabled")
		return nil, false
	}
	return &worker, true
}

func (h *ExecutionSandboxHandler) workerSandboxFromPath(
	w http.ResponseWriter,
	r *http.Request,
) (*v1beta1.ExecutionSandbox, bool) {
	worker, ok := h.deepAgentsWorker(w, r)
	if !ok {
		return nil, false
	}
	sessionID := strings.TrimSpace(r.PathValue("sessionId"))
	if !executionSandboxSessionIDPattern.MatchString(sessionID) {
		httputil.WriteError(w, http.StatusBadRequest, "invalid sessionId")
		return nil, false
	}
	var sandbox v1beta1.ExecutionSandbox
	key := client.ObjectKey{Name: executionSandboxName(worker.Name, sessionID), Namespace: h.namespace}
	if err := h.client.Get(r.Context(), key, &sandbox); err != nil {
		writeK8sError(w, "get execution sandbox", err)
		return nil, false
	}
	if !h.ownsControllerLabel(sandbox.Labels) {
		httputil.WriteError(w, http.StatusConflict, "execution sandbox belongs to another controller")
		return nil, false
	}
	if sandbox.Spec.WorkerRef.Name != worker.Name || sandbox.Spec.WorkerRef.UID != string(worker.UID) || sandbox.Spec.SessionID != sessionID {
		httputil.WriteError(w, http.StatusConflict, "execution sandbox identity collision")
		return nil, false
	}
	return &sandbox, true
}

// ownsControllerLabel mirrors the platform's embedded ownership convention:
// an empty controller name owns only unlabelled objects, while a named
// in-cluster controller requires an exact label match.
func (h *ExecutionSandboxHandler) ownsControllerLabel(labels map[string]string) bool {
	return labels[v1beta1.LabelController] == h.controllerName
}

func executionSandboxName(workerName, sessionID string) string {
	digest := sha256.Sum256([]byte(workerName + "\x00" + sessionID))
	suffix := hex.EncodeToString(digest[:8])
	prefix := strings.ToLower(workerName)
	prefix = regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(prefix, "-")
	prefix = strings.Trim(prefix, "-")
	if prefix == "" {
		prefix = "worker"
	}
	if len(prefix) > 38 {
		prefix = strings.TrimRight(prefix[:38], "-")
	}
	return fmt.Sprintf("exec-%s-%s", prefix, suffix)
}

func deepCopyEgressRules(in []v1beta1.DeepAgentsEgressRule) []v1beta1.DeepAgentsEgressRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]v1beta1.DeepAgentsEgressRule, len(in))
	for i := range in {
		in[i].DeepCopyInto(&out[i])
	}
	return out
}
