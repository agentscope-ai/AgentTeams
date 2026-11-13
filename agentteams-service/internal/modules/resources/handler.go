package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/config"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/identity"
)

// idemKeyMaxLen is the ops.idempotency_keys.idem_key column width.
const idemKeyMaxLen = 128

// DenyAuditor records a deny decision in ops.audit_events. The service
// owns deny audit rows (the Store writes the allow row inside the create
// transaction — 58 §2/§4.3). Implementations must be safe for concurrent
// use; a nil Auditor disables deny auditing.
type DenyAuditor interface {
	Deny(ctx context.Context, a *AuditMeta, code, reason string) error
}

// ExecutionResolver resolves the controller's observed execution state for
// one worker (GET /internal/v1/workers/{name}/execution). found=false means
// the controller does not know the worker (404). A nil resolver disables the
// get-by-name cache-aside refresh, so a bare handler still serves the row.
type ExecutionResolver func(ctx context.Context, name string) (obs Observed, generation int64, found bool, err error)

// Handler serves the three native worker routes (58 §1.1-§1.3):
//
//	POST /api/v1/workers
//	GET  /api/v1/workers
//	GET  /api/v1/workers/{name}
//
// PUT/DELETE stay controller-authoritative and are proxied by 2c, not here.
type Handler struct {
	Store Store
	Idem  IdemStore
	Log   *slog.Logger
	// Now is the handler clock (tests); it stamps the timestamps handed to
	// Store.Create, which re-stamps them in its own transaction. nil →
	// time.Now.
	Now            func() time.Time
	DefaultRuntime string      // create-time runtime when the request omits it; "" → "openclaw"
	DefaultModel   string      // create-time model when the request omits it; "" → no default
	Auditor        DenyAuditor // optional deny-audit sink
	// ResolveExecution enables the get-by-name cache-aside refresh (58 §1.3:
	// a controller-side write or delete is reconciled on the next single
	// read; list never refreshes per row).
	ResolveExecution ExecutionResolver
}

// Register mounts the three native routes on mux using Go 1.22
// method+path patterns.
func (h *Handler) Register(m *http.ServeMux) {
	m.HandleFunc("POST /api/v1/workers", h.createWorker)
	m.HandleFunc("GET /api/v1/workers", h.listWorkers)
	m.HandleFunc("GET /api/v1/workers/{name}", h.getWorker)
}

// CreateRequest mirrors agentteams-controller internal/server/types.go:10-46
// (CreateWorkerRequest) field for field; controller-typed slices/objects are
// carried as raw JSON because the service is a separate Go module. Note: the
// R1-2b2 brief says "26 fields"; the controller struct actually has 30
// (including containerManaged/state), and all 30 are accepted here.
type CreateRequest struct {
	Env                          map[string]string `json:"env,omitempty"`
	Name                         string            `json:"name"`
	WorkerName                   string            `json:"workerName,omitempty"`
	Model                        string            `json:"model,omitempty"`
	SubagentModel                string            `json:"subagentModel,omitempty"`
	LlmStreamFirstContentTimeout string            `json:"llmStreamFirstContentTimeout,omitempty"`
	LlmStreamIdleTimeout         string            `json:"llmStreamIdleTimeout,omitempty"`
	ModelProvider                string            `json:"modelProvider,omitempty"`
	Runtime                      string            `json:"runtime,omitempty"`
	Image                        string            `json:"image,omitempty"`
	Identity                     string            `json:"identity,omitempty"`
	Soul                         string            `json:"soul,omitempty"`
	Agents                       string            `json:"agents,omitempty"`
	Skills                       []string          `json:"skills,omitempty"`
	RemoteSkills                 json.RawMessage   `json:"remoteSkills,omitempty"`
	McpServers                   json.RawMessage   `json:"mcpServers,omitempty"`
	Package                      string            `json:"package,omitempty"`
	Expose                       json.RawMessage   `json:"expose,omitempty"`
	ChannelPolicy                json.RawMessage   `json:"channelPolicy,omitempty"`
	Resources                    json.RawMessage   `json:"resources,omitempty"`
	LlmMaxRetries                string            `json:"llmMaxRetries,omitempty"`
	LlmBackoffBase               string            `json:"llmBackoffBase,omitempty"`
	LlmBackoffCap                string            `json:"llmBackoffCap,omitempty"`
	LlmMaxConcurrent             string            `json:"llmMaxConcurrent,omitempty"`
	LlmMaxQpm                    string            `json:"llmMaxQpm,omitempty"`
	LlmRateLimitPause            string            `json:"llmRateLimitPause,omitempty"`
	LlmRateLimitJitter           string            `json:"llmRateLimitJitter,omitempty"`
	LlmAcquireTimeout            string            `json:"llmAcquireTimeout,omitempty"`
	ContainerManaged             *bool             `json:"containerManaged,omitempty"`
	State                        *string           `json:"state,omitempty"`
}

// --- create ---

// maxRequestBodyBytes is the per-request body cap applied here as well as in
// the kernel middleware, so an embedded handler (tests, future UDS) cannot
// buffer an unbounded JSON document.
const maxRequestBodyBytes = 1 << 20

func (h *Handler) createWorker(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	caller := identity.FromContext(r.Context())
	if caller == nil {
		h.fail(w, r, nil, "worker.create", http.StatusUnauthorized, httputil.CodeUnauthenticated, "invalid or missing bearer token")
		return
	}
	if !CanCreate(caller) {
		h.fail(w, r, caller, "worker.create", http.StatusForbidden, httputil.CodeForbidden,
			fmt.Sprintf("authorization denied: %s cannot create workers", caller.Role))
		return
	}

	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.fail(w, r, caller, "worker.create", http.StatusBadRequest, httputil.CodeInvalidRequest, "invalid JSON: "+err.Error())
		return
	}

	// Idempotency replay: a retry returns the first accepted outcome
	// verbatim (status + body) without touching the store. Lookup errors
	// other than a miss are logged and the create proceeds (R1 fail-open:
	// the idem store must not take the create path down with it).
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) > idemKeyMaxLen {
		h.fail(w, r, caller, "worker.create", http.StatusBadRequest, httputil.CodeInvalidRequest,
			fmt.Sprintf("Idempotency-Key must be at most %d characters", idemKeyMaxLen))
		return
	}
	if key != "" && h.Idem != nil {
		rec, err := h.Idem.Get(r.Context(), key)
		switch {
		case err == nil && rec != nil:
			writeIdemReplay(w, rec)
			return
		case err != nil && !errors.Is(err, ErrIdemMiss):
			h.logger().Warn("idempotency lookup failed; proceeding with create",
				"err", err, "request_id", httputil.RequestID(r))
		}
	}

	// Frozen validation order: name → runtime → env → assemble (58 §1.1).
	if err := ValidateName(req.Name); err != nil {
		h.writeValidationError(w, r, caller, "worker.create", err)
		return
	}
	runtimeName := req.Runtime
	if runtimeName == "" {
		runtimeName = h.DefaultRuntime
	}
	if runtimeName == "" {
		runtimeName = runtimeOpenClaw
	}
	// Controller order (resource_handler.go:146-166): name -> env -> runtime.
	// Keeping it identical matters for multi-error G1 fixtures.
	if err := ValidateEnv(req.Env, caller.CanManageEnv()); err != nil {
		h.writeValidationError(w, r, caller, "worker.create", err)
		return
	}
	if err := ValidateRuntime(runtimeName); err != nil {
		h.writeValidationError(w, r, caller, "worker.create", err)
		return
	}

	// Defaults (resource_handler.go:154-169): containerManaged true, model
	// default when blank (whitespace counts as blank), runtime already
	// resolved, desired state carried from state ("" → response "Running").
	model := req.Model
	if strings.TrimSpace(model) == "" {
		model = h.DefaultModel
	}
	containerManaged := true
	if req.ContainerManaged != nil {
		containerManaged = *req.ContainerManaged
	}
	desiredState := ""
	if req.State != nil {
		desiredState = *req.State
	}

	now := h.now()
	worker := &Worker{
		Name:                         req.Name,
		WorkerName:                   req.WorkerName,
		TeamSlug:                     "", // 58 C1: CreateWorkerRequest has no team field
		Model:                        model,
		ModelProvider:                req.ModelProvider,
		SubagentModel:                req.SubagentModel,
		LLMStreamFirstContentTimeout: req.LlmStreamFirstContentTimeout,
		LLMStreamIdleTimeout:         req.LlmStreamIdleTimeout,
		LLMMaxRetries:                req.LlmMaxRetries,
		LLMBackoffBase:               req.LlmBackoffBase,
		LLMBackoffCap:                req.LlmBackoffCap,
		LLMMaxConcurrent:             req.LlmMaxConcurrent,
		LLMMaxQPM:                    req.LlmMaxQpm,
		LLMRateLimitPause:            req.LlmRateLimitPause,
		LLMRateLimitJitter:           req.LlmRateLimitJitter,
		LLMAcquireTimeout:            req.LlmAcquireTimeout,
		Runtime:                      runtimeName,
		Image:                        req.Image,
		Identity:                     req.Identity,
		Soul:                         req.Soul,
		Agents:                       req.Agents,
		Skills:                       req.Skills,
		RemoteSkills:                 req.RemoteSkills,
		MCPServers:                   req.McpServers,
		PackageRef:                   req.Package,
		Expose:                       req.Expose,
		ChannelPolicy:                req.ChannelPolicy,
		Resources:                    req.Resources,
		Env:                          req.Env,
		ContainerManaged:             containerManaged,
		DesiredState:                 desiredState,
		CreatedAt:                    now,
		UpdatedAt:                    now,
	}

	created, err := h.Store.Create(r.Context(), worker, h.auditMeta(r, caller, "worker.create"))
	if err != nil {
		switch {
		case errors.Is(err, ErrAlreadyExists):
			// No Idempotency-Key (or an expired one) and the name exists:
			// 58 §2 already_exists. Matches writeK8sError's message shape.
			h.fail(w, r, caller, "worker.create", http.StatusConflict, httputil.CodeAlreadyExists, "create worker: already exists")
		case errors.Is(err, ErrFieldTooLong):
			// A field exceeded its column width; retrying cannot help.
			h.fail(w, r, caller, "worker.create", http.StatusBadRequest, httputil.CodeInvalidRequest, "create worker: field value too long")
		default:
			h.logger().Error("create worker failed",
				"worker", worker.Name, "err", err, "request_id", httputil.RequestID(r))
			h.fail(w, r, caller, "worker.create", http.StatusInternalServerError, httputil.CodeInternal, "create worker: internal error")
		}
		return
	}
	if created == nil {
		created = worker
	}

	body, err := created.ResponseBytes(caller)
	if err != nil {
		h.logger().Error("encode create response failed",
			"worker", created.Name, "err", err, "request_id", httputil.RequestID(r))
		h.fail(w, r, caller, "worker.create", http.StatusInternalServerError, httputil.CodeInternal, "create worker: internal error")
		return
	}

	// Record the replay before writing the response: the client may retry the
	// moment it sees 201, and a Put failure is logged only (the worker exists
	// and the client must still see its 201 — 58 §4.5 best-effort write side).
	if key != "" && h.Idem != nil {
		if err := h.Idem.Put(r.Context(), key, created.Name, http.StatusCreated, body, config.IdempotencyTTL); err != nil {
			h.logger().Warn("idempotency record write failed",
				"err", err, "key", key, "request_id", httputil.RequestID(r))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(body)
}

// --- list ---

// workerFields is the parsed ?fields= selection of GET /api/v1/workers
// (resource_handler.go:320-329).
type workerFields int

const (
	workerFieldsAll workerFields = iota
	workerFieldsSpec
	workerFieldsStatus
	workerFieldsSpecStatus
)

// parseWorkerListFields is a verbatim port of
// resource_handler.go:339-369: empty tokens are ignored; only
// spec/status/all are valid; all cannot be combined with a subset; token
// order and duplicates do not matter.
func parseWorkerListFields(raw string) (workerFields, error) {
	const valid = "spec, status, all"
	spec, status, all := false, false, false
	for _, token := range strings.Split(raw, ",") {
		token = strings.TrimSpace(token)
		switch token {
		case "":
		case "spec":
			spec = true
		case "status":
			status = true
		case "all":
			all = true
		default:
			return 0, fmt.Errorf("invalid fields value %q: valid values are %s", token, valid)
		}
	}
	if all && (spec || status) {
		return 0, fmt.Errorf("invalid fields value: %q cannot be combined with other values; valid values are %s", "all", valid)
	}
	switch {
	case spec && status:
		return workerFieldsSpecStatus, nil
	case spec:
		return workerFieldsSpec, nil
	case status:
		return workerFieldsStatus, nil
	default:
		return workerFieldsAll, nil
	}
}

// projectWorkerListRow reduces one full response to the requested view
// (resource_handler.go:372-381).
func projectWorkerListRow(resp WorkerResponse, fields workerFields) any {
	switch fields {
	case workerFieldsSpec:
		return resp.SpecView()
	case workerFieldsStatus:
		return resp.StatusView()
	default: // workerFieldsSpecStatus
		return resp.SpecStatusView()
	}
}

// workerListProjection is the envelope for a projected list: the same
// workers/total wrapping, with each row reduced to the requested view
// (controller types.go:158-165).
type workerListProjection struct {
	Workers []any `json:"workers"`
	Total   int   `json:"total"`
}

func (h *Handler) listWorkers(w http.ResponseWriter, r *http.Request) {
	caller := identity.FromContext(r.Context())
	if caller == nil {
		h.fail(w, r, nil, "worker.list", http.StatusUnauthorized, httputil.CodeUnauthenticated, "invalid or missing bearer token")
		return
	}
	if !CanList(caller) {
		h.fail(w, r, caller, "worker.list", http.StatusForbidden, httputil.CodeForbidden,
			fmt.Sprintf("authorization denied: %s cannot list workers", caller.Role))
		return
	}

	fields, err := parseWorkerListFields(r.URL.Query().Get("fields"))
	if err != nil {
		h.fail(w, r, caller, "worker.list", http.StatusBadRequest, httputil.CodeInvalidRequest, err.Error())
		return
	}

	// ETag only for the stable spec projection; status/all are
	// poll-volatile and invalid fields must fall through to their 400
	// (controller specViewETag, http.go:559-568).
	if fields == workerFieldsSpec {
		httputil.ETagMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.listWorkersCore(w, r, caller, fields)
		})).ServeHTTP(w, r)
		return
	}
	h.listWorkersCore(w, r, caller, fields)
}

// listWorkersCore runs after role/fields checks: store list (already
// name-ASC), authorization filter, caller-scoped env/team reduction, then
// either the full envelope or the requested projection.
func (h *Handler) listWorkersCore(w http.ResponseWriter, r *http.Request, caller *identity.Identity, fields workerFields) {
	rows, err := h.Store.List(r.Context(), ListFilter{TeamSlug: r.URL.Query().Get("team")})
	if err != nil {
		h.logger().Error("list workers failed", "err", err, "request_id", httputil.RequestID(r))
		h.fail(w, r, caller, "worker.list", http.StatusInternalServerError, httputil.CodeInternal, "list workers: internal error")
		return
	}
	rows = FilterList(caller, rows)

	if fields == workerFieldsAll {
		out := make([]WorkerResponse, 0, len(rows))
		for _, worker := range rows {
			resp := worker.Response(caller)
			if caller.IsWorkerScoped() {
				SanitizeMCPForL3(&resp)
			}
			out = append(out, resp)
		}
		// len(out) is the post-filter total (58 §1.2: total follows the
		// visible rows, never the unfiltered store size).
		httputil.WriteJSON(w, http.StatusOK, WorkerListResponse{Workers: out, Total: len(out)})
		return
	}

	projected := make([]any, 0, len(rows))
	for _, worker := range rows {
		resp := worker.Response(caller)
		if caller.IsWorkerScoped() {
			SanitizeMCPForL3(&resp)
		}
		projected = append(projected, projectWorkerListRow(resp, fields))
	}
	httputil.WriteJSON(w, http.StatusOK, workerListProjection{Workers: projected, Total: len(projected)})
}

// --- get ---

func (h *Handler) getWorker(w http.ResponseWriter, r *http.Request) {
	caller := identity.FromContext(r.Context())
	if caller == nil {
		h.fail(w, r, nil, "worker.get", http.StatusUnauthorized, httputil.CodeUnauthenticated, "invalid or missing bearer token")
		return
	}
	name := r.PathValue("name")
	if name == "" {
		h.writeValidationError(w, r, caller, "worker.get", ErrWorkerNameRequired)
		return
	}

	worker, err := h.Store.Get(r.Context(), name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			h.fail(w, r, caller, "worker.get", http.StatusNotFound, httputil.CodeNotFound, "get worker: not found")
			return
		}
		h.logger().Error("get worker failed", "worker", name, "err", err, "request_id", httputil.RequestID(r))
		h.fail(w, r, caller, "worker.get", http.StatusInternalServerError, httputil.CodeInternal, "get worker: internal error")
		return
	}
	if !CanRead(caller, worker) {
		// W8: hide existence from scoped callers — 404, not 403.
		h.fail(w, r, caller, "worker.get", http.StatusNotFound, httputil.CodeNotFound, "get worker: not found")
		return
	}
	if h.ResolveExecution != nil {
		worker = h.refreshFromController(w, r, caller, worker)
		if worker == nil {
			return // the refresh already wrote 404 (controller deleted it)
		}
	}

	resp := worker.Response(caller)
	if caller.IsWorkerScoped() {
		SanitizeMCPForL3(&resp)
	}
	// get-by-name carries no ETag (58 §1.3: controller http.go:127 does not
	// wrap this route).
	httputil.WriteJSON(w, http.StatusOK, resp)
}

// refreshFromController is the cache-aside read path of workers.get
// (58 §1.3/§5). The controller is authoritative for execution state and for
// existence: 404 soft-deletes the service row and returns 404; a newer
// generation refreshes the observed columns; an unavailable controller
// degrades to the stored row (bounded staleness, D110).
func (h *Handler) refreshFromController(w http.ResponseWriter, r *http.Request, caller *identity.Identity, worker *Worker) *Worker {
	ctx := r.Context()
	obs, generation, found, err := h.ResolveExecution(ctx, worker.Name)
	if err != nil {
		h.logger().Warn("get worker: execution refresh unavailable; serving stored row",
			"worker", worker.Name, "err", err, "request_id", httputil.RequestID(r))
		return worker
	}
	if !found {
		if derr := h.Store.SoftDelete(ctx, worker.Name, worker.Version, h.auditMeta(r, caller, "worker.get")); derr != nil &&
			!errors.Is(derr, ErrNotFound) && !errors.Is(derr, ErrConflict) {
			h.logger().Warn("get worker: soft delete after controller 404 failed",
				"worker", worker.Name, "err", derr, "request_id", httputil.RequestID(r))
		}
		h.fail(w, r, caller, "worker.get", http.StatusNotFound, httputil.CodeNotFound, "get worker: not found")
		return nil
	}
	if generation > 0 && generation >= worker.Generation {
		ok, uerr := h.Store.UpdateStatus(ctx, worker.Name, obs, generation)
		if uerr != nil {
			h.logger().Warn("get worker: observed-state refresh failed",
				"worker", worker.Name, "err", uerr, "request_id", httputil.RequestID(r))
		} else if ok {
			worker.ObservedPhase = obs.Phase
			worker.ObservedContainerState = obs.ContainerState
			worker.MatrixUserID = obs.MatrixUserID
			worker.RoomID = obs.RoomID
			worker.Message = obs.Message
			worker.LastActiveAt = obs.LastActiveAt
			worker.AgentStatus = obs.AgentStatus
			worker.RunningTaskCount = obs.RunningTaskCount
			worker.LastRunAt = obs.LastRunAt
			worker.LastFinishAt = obs.LastFinishAt
		}
	}
	return worker
}

// --- helpers ---

func (h *Handler) logger() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

// auditMeta builds the request context the Store/Auditor persists. Token
// JTI is left empty here: the 2c token-review client owns the bearer token
// and will populate identity.TokenDigest(..) (kernel/identity docs); no
// other field is derivable in R1.
func (h *Handler) auditMeta(r *http.Request, caller *identity.Identity, action string) *AuditMeta {
	a := &AuditMeta{
		RequestID: httputil.RequestID(r),
		Action:    action,
		Client:    r.UserAgent(),
	}
	if token, ok := identity.BearerToken(r); ok {
		a.TokenJTI = identity.TokenDigest(token)
	}
	if caller != nil {
		a.ActorType = string(caller.Role)
		a.ActorID = caller.Username
		if a.ActorID == "" {
			a.ActorID = caller.MatrixUserID
		}
		if a.ActorID == "" {
			a.ActorID = caller.WorkerName
		}
	}
	return a
}

// fail writes the frozen envelope and records the deny decision. It is the
// single path for 401/403/404-hidden/400 validation denials.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, caller *identity.Identity, action string, status int, code, message string) {
	if h.Auditor != nil {
		if err := h.Auditor.Deny(r.Context(), h.auditMeta(r, caller, action), code, message); err != nil {
			h.logger().Warn("audit deny record failed",
				"err", err, "code", code, "request_id", httputil.RequestID(r))
		}
	}
	httputil.WriteError(w, status, code, message)
}

// writeValidationError maps a validation error through ErrorResponse and
// audits it as a deny.
func (h *Handler) writeValidationError(w http.ResponseWriter, r *http.Request, caller *identity.Identity, action string, err error) {
	status, code, message := ErrorResponse(err)
	h.fail(w, r, caller, action, status, code, message)
}

// writeIdemReplay writes a stored first response byte-for-byte.
func writeIdemReplay(w http.ResponseWriter, rec *IdemRecord) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rec.Status)
	if len(rec.Body) > 0 {
		_, _ = w.Write(rec.Body)
	}
}
