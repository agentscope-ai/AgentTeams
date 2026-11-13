package server

// Internal service API (R1-2e, 58 §5). These routes are the receiving half
// of the agentteams-service adapter contract:
//
//	POST /internal/v1/workers/{name}/provision    service → controller
//	POST /internal/v1/workers/{name}/deprovision  service → controller
//	GET  /internal/v1/workers/{name}/execution    service → controller
//	POST /internal/v1/token-review                service → controller
//
// The surface is opt-in: it is registered only when AGENTTEAMS_INTERNAL_TOKEN
// is set, authenticated with that shared bearer plus an RFC3339 timestamp
// inside a ±5min window and an X-AT-Request-Id — never with the public claims
// chain, and never routed by Higress. Adding it does not change any existing
// public route (G1 closed-mode zero change).

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
)

const (
	// provisionGenerationAnnotation records the last service generation the
	// controller applied, so a redelivered (name, generation) is a no-op and
	// an older generation can never overwrite a newer spec.
	provisionGenerationAnnotation = "agentteams.io/provision-generation"
	// provisionIncarnationAnnotation identifies one life of a worker name.
	// A soft-deleted row frees the name, so delete→create is a new life; a
	// delayed deprovision from the old life must not delete the new CR.
	provisionIncarnationAnnotation = "agentteams.io/provision-incarnation"

	internalReplayWindow    = 5 * time.Minute
	internalTimestampHeader = "X-AT-Timestamp"
	internalRequestIDHeader = "X-AT-Request-Id"
	internalMaxBodyBytes    = 4 << 20
)

// internalAPI owns the /internal/v1 handlers. It deliberately does not
// depend on the public ResourceHandler: the internal contract is a small,
// separately testable surface.
type internalAPI struct {
	client         client.Client
	namespace      string
	auth           *authpkg.Middleware
	token          string
	controllerName string
	defaultRuntime string
	defaultModel   string
	defaultManaged bool
	now            func() time.Time
}

func registerInternalRoutes(mux *http.ServeMux, deps ServerDeps) {
	if deps.InternalToken == "" {
		return
	}
	api := &internalAPI{
		client:         deps.Client,
		namespace:      deps.Namespace,
		auth:           deps.AuthMw,
		token:          deps.InternalToken,
		controllerName: deps.ControllerName,
		defaultRuntime: deps.DefaultWorkerRuntime,
		defaultModel:   deps.DefaultModel,
		defaultManaged: true,
		now:            time.Now,
	}
	mux.Handle("POST /internal/v1/token-review", api.guard(http.HandlerFunc(api.tokenReview)))
	mux.Handle("POST /internal/v1/workers/{name}/provision", api.guard(http.HandlerFunc(api.provision)))
	mux.Handle("POST /internal/v1/workers/{name}/deprovision", api.guard(http.HandlerFunc(api.deprovision)))
	mux.Handle("GET /internal/v1/workers/{name}/execution", api.guard(http.HandlerFunc(api.execution)))
}

// guard enforces the internal bearer + replay window + request id. The
// timestamp is a coarse clock-skew/replay guard, not a nonce store: the
// routes are idempotent on (name, generation) and status writes are
// naturally idempotent, matching the service-side guard (58 §5).
func (a *internalAPI) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+a.token)) != 1 {
			httputil.WriteError(w, http.StatusUnauthorized, "internal bearer token required")
			return
		}
		ts, err := time.Parse(time.RFC3339, r.Header.Get(internalTimestampHeader))
		if err != nil {
			httputil.WriteError(w, http.StatusUnauthorized, "X-AT-Timestamp is required (RFC3339)")
			return
		}
		skew := a.now().Sub(ts)
		if skew < 0 {
			skew = -skew
		}
		if skew > internalReplayWindow {
			httputil.WriteError(w, http.StatusUnauthorized, "X-AT-Timestamp outside the replay window")
			return
		}
		if r.Header.Get(internalRequestIDHeader) == "" {
			httputil.WriteError(w, http.StatusBadRequest, "X-AT-Request-Id is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func decodeInternal(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, internalMaxBodyBytes)).Decode(v); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// internalIdentity mirrors authpkg.CallerIdentity on the wire with the JSON
// names the service consumes (internal/kernel/identity.Identity). Explicit
// DTO: the auth struct has no json tags, and changing it would change the
// public /api/v1/me shape.
type internalIdentity struct {
	MatrixUserID         string   `json:"matrixUserID,omitempty"`
	HumanPermissionLevel int      `json:"humanPermissionLevel,omitempty"`
	Role                 string   `json:"role"`
	Username             string   `json:"username,omitempty"`
	Team                 string   `json:"team,omitempty"`
	Teams                []string `json:"teams,omitempty"`
	Capabilities         []string `json:"capabilities,omitempty"`
	AccessibleWorkers    []string `json:"accessibleWorkers,omitempty"`
	WorkerName           string   `json:"workerName,omitempty"`
}

func wireIdentity(c *authpkg.CallerIdentity) internalIdentity {
	if c == nil {
		return internalIdentity{}
	}
	return internalIdentity{
		MatrixUserID:         c.MatrixUserID,
		HumanPermissionLevel: c.HumanPermissionLevel,
		Role:                 c.Role,
		Username:             c.Username,
		Team:                 c.Team,
		Teams:                c.Teams,
		Capabilities:         c.Capabilities,
		AccessibleWorkers:    c.AccessibleWorkers,
		WorkerName:           c.WorkerName,
	}
}

// tokenReview exchanges a public bearer token for the enriched caller
// identity (58 §5). An invalid/expired token is 401; the service surfaces it
// verbatim as unauthenticated.
func (a *internalAPI) tokenReview(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !decodeInternal(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Token) == "" {
		httputil.WriteError(w, http.StatusBadRequest, "token is required")
		return
	}
	if a.auth == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "authenticator is not configured")
		return
	}
	id, err := a.auth.ReviewToken(r.Context(), body.Token)
	if err != nil {
		httputil.WriteError(w, http.StatusUnauthorized, "invalid or expired token")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, wireIdentity(id))
}

// provision creates or updates the Worker CR from the service spec.
//
// Wire shape: the service sends {"name","generation","spec"} where spec is
// the camelCase controller API shape (resources.WireSpec) — it unmarshals
// straight into v1beta1.WorkerSpec. Idempotent on (name, generation): an
// applied generation >= the requested one returns 200 without a write.
func (a *internalAPI) provision(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required")
		return
	}
	var body struct {
		Name        string          `json:"name"`
		Generation  int64           `json:"generation"`
		Incarnation string          `json:"incarnation"`
		Spec        json.RawMessage `json:"spec"`
	}
	if !decodeInternal(w, r, &body) {
		return
	}
	if body.Name != "" && body.Name != name {
		httputil.WriteError(w, http.StatusBadRequest, "name in body does not match the path")
		return
	}
	if body.Generation <= 0 {
		httputil.WriteError(w, http.StatusBadRequest, "generation must be positive")
		return
	}
	var spec v1beta1.WorkerSpec
	if len(body.Spec) > 0 {
		if err := json.Unmarshal(body.Spec, &spec); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "invalid spec: "+err.Error())
			return
		}
	}
	runtime := backend.ResolveRuntime(spec.Runtime, a.defaultRuntime)
	if strings.EqualFold(runtime, backend.RuntimeCopaw) {
		httputil.WriteError(w, http.StatusBadRequest, "runtime copaw is deprecated; use qwenpaw")
		return
	}
	spec.Runtime = runtime
	if strings.TrimSpace(spec.Model) == "" {
		spec.Model = a.defaultModel
	}
	if spec.ContainerManaged == nil {
		managed := a.defaultManaged
		spec.ContainerManaged = &managed
	}

	ctx := r.Context()
	key := types.NamespacedName{Namespace: a.namespace, Name: name}
	var existing v1beta1.Worker
	err := a.client.Get(ctx, key, &existing)
	switch {
	case apierrors.IsNotFound(err):
		if len(body.Spec) == 0 {
			httputil.WriteError(w, http.StatusBadRequest, "spec is required to create a worker")
			return
		}
		worker := &v1beta1.Worker{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: a.namespace,
				Annotations: map[string]string{
					provisionGenerationAnnotation:  strconv.FormatInt(body.Generation, 10),
					provisionIncarnationAnnotation: body.Incarnation,
				},
			},
			Spec: spec,
		}
		appendImplicitModelGrant(ctx, worker, spec.ModelProvider, spec.Model)
		a.stamp(&worker.ObjectMeta)
		if err := a.client.Create(ctx, worker); err != nil {
			writeK8sError(w, "provision worker", err)
			return
		}
	case err != nil:
		writeK8sError(w, "provision worker: get", err)
		return
	default:
		applied := appliedProvision(existing.Annotations)
		// Idempotent on (incarnation, generation): a redelivery of the same
		// life at an already-applied generation is a no-op. A different
		// incarnation is a new life and must apply even at generation 1
		// (delete→create), where the old annotation would otherwise block it.
		if applied.incarnation == body.Incarnation && applied.generation >= body.Generation {
			httputil.WriteJSON(w, http.StatusOK, map[string]any{})
			return
		}
		if len(body.Spec) > 0 {
			// Full spec (create/update). "sync" events send no spec and only
			// refresh the generation/incarnation bookkeeping below.
			existing.Spec = spec
			appendImplicitModelGrant(ctx, &existing, spec.ModelProvider, spec.Model)
		}
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		existing.Annotations[provisionGenerationAnnotation] = strconv.FormatInt(body.Generation, 10)
		existing.Annotations[provisionIncarnationAnnotation] = body.Incarnation
		a.stamp(&existing.ObjectMeta)
		if err := a.client.Update(ctx, &existing); err != nil {
			writeK8sError(w, "provision worker: update", err)
			return
		}
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{})
}

// deprovision deletes the Worker CR; an already-absent worker is success
// (idempotent redelivery).
func (a *internalAPI) deprovision(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required")
		return
	}
	var body struct {
		Name        string `json:"name"`
		Generation  int64  `json:"generation"`
		Incarnation string `json:"incarnation"`
	}
	// An empty body is tolerated (older callers); the guard simply has no
	// incarnation to compare.
	if r.ContentLength != 0 {
		if !decodeInternal(w, r, &body) {
			return
		}
	}
	if body.Name != "" && body.Name != name {
		httputil.WriteError(w, http.StatusBadRequest, "name in body does not match the path")
		return
	}
	ctx := r.Context()
	key := types.NamespacedName{Namespace: a.namespace, Name: name}
	var worker v1beta1.Worker
	err := a.client.Get(ctx, key, &worker)
	if apierrors.IsNotFound(err) {
		httputil.WriteJSON(w, http.StatusOK, map[string]any{})
		return
	}
	if err != nil {
		writeK8sError(w, "deprovision worker: get", err)
		return
	}
	// A delayed deprovision from a previous life must not delete the CR of
	// the current life (delete→create with a failed deprovision + backoff).
	if applied := appliedProvision(worker.Annotations); body.Incarnation != "" &&
		applied.incarnation != "" && applied.incarnation != body.Incarnation {
		httputil.WriteJSON(w, http.StatusOK, map[string]any{})
		return
	}
	if err := a.client.Delete(ctx, &worker); err != nil && !apierrors.IsNotFound(err) {
		writeK8sError(w, "deprovision worker: delete", err)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{})
}

// executionResponse is the observed execution state the service reads for
// retry decisions and get-by-name refresh (58 §5). SpecHash is intentionally
// empty: the controller's applied hash uses a different canonicalization
// (runtime+resources aware) than the service's spec_hash, and returning it
// would create false "spec changed" signals. R2 aligns the algorithms.
type executionResponse struct {
	Name             string `json:"name"`
	Generation       int64  `json:"generation"`
	Phase            string `json:"phase"`
	ContainerState   string `json:"containerState,omitempty"`
	MatrixUserID     string `json:"matrixUserID,omitempty"`
	RoomID           string `json:"roomID,omitempty"`
	Message          string `json:"message,omitempty"`
	LastActiveAt     string `json:"lastActiveAt,omitempty"`
	AgentStatus      string `json:"agentStatus,omitempty"`
	RunningTaskCount *int   `json:"runningTaskCount,omitempty"`
	LastRunAt        string `json:"lastRunAt,omitempty"`
	LastFinishAt     string `json:"lastFinishAt,omitempty"`
}

func (a *internalAPI) execution(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required")
		return
	}
	var worker v1beta1.Worker
	if err := a.client.Get(r.Context(), types.NamespacedName{Namespace: a.namespace, Name: name}, &worker); err != nil {
		if apierrors.IsNotFound(err) {
			httputil.WriteError(w, http.StatusNotFound, "worker not found")
			return
		}
		writeK8sError(w, "execution worker: get", err)
		return
	}
	phase := worker.Status.Phase
	if phase == "" {
		phase = "Pending"
	}
	httputil.WriteJSON(w, http.StatusOK, executionResponse{
		Name:             worker.Name,
		Generation:       appliedGeneration(worker.Annotations),
		Phase:            phase,
		ContainerState:   worker.Status.ContainerState,
		MatrixUserID:     worker.Status.MatrixUserID,
		RoomID:           worker.Status.RoomID,
		Message:          worker.Status.Message,
		LastActiveAt:     worker.Status.LastActiveAt,
		AgentStatus:      worker.Status.AgentStatus,
		RunningTaskCount: worker.Status.RunningTaskCount,
		LastRunAt:        worker.Status.LastRunAt,
		LastFinishAt:     worker.Status.LastFinishAt,
	})
}

type appliedProvisionState struct {
	generation  int64
	incarnation string
}

func appliedProvision(annotations map[string]string) appliedProvisionState {
	st := appliedProvisionState{incarnation: annotations[provisionIncarnationAnnotation]}
	raw, ok := annotations[provisionGenerationAnnotation]
	if !ok {
		return st
	}
	gen, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return st
	}
	st.generation = gen
	return st
}

func appliedGeneration(annotations map[string]string) int64 {
	raw, ok := annotations[provisionGenerationAnnotation]
	if !ok {
		return 0
	}
	gen, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return gen
}

func (a *internalAPI) stamp(meta *metav1.ObjectMeta) {
	if a.controllerName == "" {
		return
	}
	if meta.Labels == nil {
		meta.Labels = map[string]string{}
	}
	meta.Labels[v1beta1.LabelController] = a.controllerName
}
