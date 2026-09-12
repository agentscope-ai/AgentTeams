package server

import (
	"context"
	"errors"
	"net/http"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/credentials"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/proxy"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ServerDeps aggregates all dependencies needed by the HTTP API handlers.
type ServerDeps struct {
	Client          client.Client
	Backend         *backend.Registry
	Gateway         gateway.Client
	OSS             oss.StorageClient
	STS             *credentials.STSService
	AuthMw          *authpkg.Middleware
	KubeMode        string
	Namespace       string
	ControllerName  string               // AGENTTEAMS_CONTROLLER_NAME; empty in embedded mode
	SocketPath      string               // Docker proxy (embedded only)
	ContainerPrefix string               // effective worker container prefix (config.ContainerPrefix); embedded-only address resolution
	MatrixConfig    matrix.Config        // for AppService rotation endpoint
	MatrixClient    matrix.Client        // for project intervention notifications (SendMessageAsAdmin); nil to skip
	Provisioner     *service.Provisioner // for Matrix token refresh

	DefaultWorkerRuntime string // install-time default for Worker create requests
}

// HTTPServer serves the unified controller REST API.
type HTTPServer struct {
	Addr   string
	Mux    *http.ServeMux
	server *http.Server
}

func NewHTTPServer(addr string, deps ServerDeps) *HTTPServer {
	mux := http.NewServeMux()
	s := &HTTPServer{
		Addr: addr,
		Mux:  mux,
		server: &http.Server{
			Addr:    addr,
			Handler: withControllerHTTPMetrics(mux),
		},
	}

	mw := deps.AuthMw

	// --- Status / health (no auth) ---
	sh := NewStatusHandler(deps.Client, deps.Namespace, deps.KubeMode)
	mux.HandleFunc("GET /healthz", sh.Healthz)

	// --- Status endpoints (authenticated, any role) ---
	mux.Handle("GET /api/v1/status", mw.RequireAuthz(authpkg.ActionGet, "status", nil)(http.HandlerFunc(sh.ClusterStatus)))
	mux.Handle("GET /api/v1/version", mw.Authenticate(http.HandlerFunc(sh.Version)))

	// --- Declarative resource CRUD ---
	rh := NewResourceHandler(deps.Client, deps.Namespace, deps.Backend, deps.ControllerName)
	rh.defaultWorkerRuntime = deps.DefaultWorkerRuntime
	nameFn := authpkg.NameFromPath

	// Workers
	mux.Handle("POST /api/v1/workers", mw.RequireAuthz(authpkg.ActionCreate, "worker", nil)(http.HandlerFunc(rh.CreateWorker)))
	mux.Handle("GET /api/v1/workers", mw.RequireAuthz(authpkg.ActionList, "worker", nil)(http.HandlerFunc(rh.ListWorkers)))
	mux.Handle("GET /api/v1/workers/{name}", mw.RequireAuthz(authpkg.ActionGet, "worker", nameFn)(http.HandlerFunc(rh.GetWorker)))
	mux.Handle("PUT /api/v1/workers/{name}", mw.RequireAuthz(authpkg.ActionUpdate, "worker", nameFn)(http.HandlerFunc(rh.UpdateWorker)))
	mux.Handle("DELETE /api/v1/workers/{name}", mw.RequireAuthz(authpkg.ActionDelete, "worker", nameFn)(http.HandlerFunc(rh.DeleteWorker)))

	// Teams
	mux.Handle("POST /api/v1/teams", mw.RequireAuthz(authpkg.ActionCreate, "team", nil)(http.HandlerFunc(rh.CreateTeam)))
	mux.Handle("GET /api/v1/teams", mw.RequireAuthz(authpkg.ActionList, "team", nil)(http.HandlerFunc(rh.ListTeams)))
	mux.Handle("GET /api/v1/teams/{name}", mw.RequireAuthz(authpkg.ActionGet, "team", nameFn)(http.HandlerFunc(rh.GetTeam)))
	mux.Handle("PUT /api/v1/teams/{name}", mw.RequireAuthz(authpkg.ActionUpdate, "team", nameFn)(http.HandlerFunc(rh.UpdateTeam)))
	mux.Handle("DELETE /api/v1/teams/{name}", mw.RequireAuthz(authpkg.ActionDelete, "team", nameFn)(http.HandlerFunc(rh.DeleteTeam)))

	// Humans
	mux.Handle("POST /api/v1/humans", mw.RequireAuthz(authpkg.ActionCreate, "human", nil)(http.HandlerFunc(rh.CreateHuman)))
	mux.Handle("GET /api/v1/humans", mw.RequireAuthz(authpkg.ActionList, "human", nil)(http.HandlerFunc(rh.ListHumans)))
	mux.Handle("GET /api/v1/humans/{name}", mw.RequireAuthz(authpkg.ActionGet, "human", nameFn)(http.HandlerFunc(rh.GetHuman)))
	mux.Handle("PUT /api/v1/humans/{name}", mw.RequireAuthz(authpkg.ActionUpdate, "human", nameFn)(http.HandlerFunc(rh.UpdateHuman)))
	mux.Handle("DELETE /api/v1/humans/{name}", mw.RequireAuthz(authpkg.ActionDelete, "human", nameFn)(http.HandlerFunc(rh.DeleteHuman)))

	// Managers
	mux.Handle("POST /api/v1/managers", mw.RequireAuthz(authpkg.ActionCreate, "manager", nil)(http.HandlerFunc(rh.CreateManager)))
	mux.Handle("GET /api/v1/managers", mw.RequireAuthz(authpkg.ActionList, "manager", nil)(http.HandlerFunc(rh.ListManagers)))
	mux.Handle("GET /api/v1/managers/{name}", mw.RequireAuthz(authpkg.ActionGet, "manager", nameFn)(http.HandlerFunc(rh.GetManager)))
	mux.Handle("PUT /api/v1/managers/{name}", mw.RequireAuthz(authpkg.ActionUpdate, "manager", nameFn)(http.HandlerFunc(rh.UpdateManager)))
	mux.Handle("DELETE /api/v1/managers/{name}", mw.RequireAuthz(authpkg.ActionDelete, "manager", nameFn)(http.HandlerFunc(rh.DeleteManager)))

	// --- Package upload ---
	ph := NewPackageHandler(deps.OSS)
	mux.Handle("POST /api/v1/packages", mw.RequireAuthz(authpkg.ActionCreate, "worker", nil)(http.HandlerFunc(ph.Upload)))

	// --- Imperative lifecycle ---
	lh := NewLifecycleHandler(deps.Client, deps.Backend, deps.Namespace)
	mux.Handle("POST /api/v1/workers/{name}/wake", mw.RequireAuthz(authpkg.ActionWake, "worker", nameFn)(http.HandlerFunc(lh.Wake)))
	mux.Handle("POST /api/v1/workers/{name}/sleep", mw.RequireAuthz(authpkg.ActionSleep, "worker", nameFn)(http.HandlerFunc(lh.Sleep)))
	mux.Handle("POST /api/v1/workers/{name}/ensure-ready", mw.RequireAuthz(authpkg.ActionEnsureReady, "worker", nameFn)(http.HandlerFunc(lh.EnsureReady)))
	mux.Handle("POST /api/v1/workers/{name}/ready", mw.RequireAuthz(authpkg.ActionReady, "worker", nameFn)(http.HandlerFunc(lh.Ready)))
	mux.Handle("GET /api/v1/workers/{name}/status", mw.RequireAuthz(authpkg.ActionStatus, "worker", nameFn)(http.HandlerFunc(lh.GetWorkerRuntimeStatus)))

	// --- Projects (workflow inspection; read from object storage) ---
	projh := NewProjectHandler(deps.Client, deps.Namespace, deps.OSS, deps.MatrixClient)
	projectNameFn := func(r *http.Request) string { return r.PathValue("id") }
	projectTaskNameFn := func(r *http.Request) string { return r.PathValue("id") + "/" + r.PathValue("taskId") }
	mux.Handle("GET /api/v1/projects", mw.RequireAuthz(authpkg.ActionList, "project", nil)(http.HandlerFunc(projh.ListProjects)))
	mux.Handle("GET /api/v1/projects/{id}/workflow", mw.RequireAuthz(authpkg.ActionGet, "project", projectNameFn)(http.HandlerFunc(projh.GetProjectWorkflow)))
	mux.Handle("GET /api/v1/projects/{id}/tasks/{taskId}", mw.RequireAuthz(authpkg.ActionGet, "project", projectNameFn)(http.HandlerFunc(projh.GetTaskInspection)))
	mux.Handle("GET /api/v1/projects/{id}/tasks/{taskId}/artifact", mw.RequireAuthz(authpkg.ActionGet, "project", projectNameFn)(http.HandlerFunc(projh.GetTaskArtifact)))
	mux.Handle("GET /api/v1/projects/{id}/spawns", mw.RequireAuthz(authpkg.ActionGet, "project", projectNameFn)(http.HandlerFunc(projh.GetProjectSpawns)))
	mux.Handle("GET /api/v1/projects/{id}/spawns/{sessionId}/messages", mw.RequireAuthz(authpkg.ActionGet, "project", projectNameFn)(http.HandlerFunc(projh.GetProjectSpawnMessages)))
	mux.Handle("GET /api/v1/projects/{id}/history", mw.RequireAuthz(authpkg.ActionGet, "project", projectNameFn)(http.HandlerFunc(projh.GetProjectHistory)))
	mux.Handle("GET /api/v1/projects/{id}/history/{timestamp}", mw.RequireAuthz(authpkg.ActionGet, "project", projectNameFn)(http.HandlerFunc(projh.GetProjectHistorySnapshot)))

	// --- Worker checkpoints (execution timeline; proxy to the worker's qwenpaw app) ---
	ckh := NewCheckpointHandler(deps.Client, deps.Namespace, deps.KubeMode, deps.ContainerPrefix)
	mux.Handle("GET /api/v1/workers/{name}/checkpoints/{sub}", mw.RequireAuthz(authpkg.ActionGet, "worker", nameFn)(http.HandlerFunc(ckh.proxyCheckpoint)))

	// --- Worker knowledge base files (read-only MEMORY.md / memory/** / digest/** inspection; proxy to the worker's qwenpaw app) ---
	wfh := NewWorkspaceFilesHandler(deps.Client, deps.Namespace, deps.KubeMode, deps.ContainerPrefix)
	mux.Handle("GET /api/v1/workers/{name}/workspace-files/{sub}", mw.RequireAuthz(authpkg.ActionGet, "worker", nameFn)(http.HandlerFunc(wfh.proxyWorkspaceFiles)))
	mux.Handle("PUT /api/v1/workers/{name}/workspace-files/file-content", mw.RequireAuthz(authpkg.ActionWorkspaceFilesWrite, "worker", nameFn)(http.HandlerFunc(wfh.proxyWorkspaceFileWrite)))

	// --- Worker tool approval (team-scoped; proxy to the worker's qwenpaw app) ---
	ah := NewApprovalHandler(deps.Client, deps.Namespace, deps.KubeMode, deps.ContainerPrefix)
	mux.Handle("GET /api/v1/workers/{name}/approval", mw.RequireAuthz(authpkg.ActionGet, "worker", nameFn)(http.HandlerFunc(ah.getWorkerApproval)))
	mux.Handle("PUT /api/v1/workers/{name}/approval", mw.RequireAuthz(authpkg.ActionWorkerApproval, "worker", nameFn)(http.HandlerFunc(ah.updateWorkerApproval)))

	// --- Worker channels (channel configuration; proxy to the worker's qwenpaw app) ---
	// Reads use ActionGet; mutations use ActionUpdate so the authorizer's
	// worker-scoped policy applies (L2 human writes ride on the
	// worker-scoped update rule; until it lands the middleware denies them
	// and only admin reaches the handler). The handler is the real boundary
	// either way: team leaders are read-only (403 on mutations) and every
	// scoped caller is team-checked (W8: 404, never 403).
	chh := NewChannelsHandler(deps.Client, deps.Namespace, deps.KubeMode, deps.ContainerPrefix, deps.OSS)
	mux.Handle("GET /api/v1/workers/{name}/channels", mw.RequireAuthz(authpkg.ActionGet, "worker", nameFn)(http.HandlerFunc(chh.getChannels)))
	mux.Handle("GET /api/v1/workers/{name}/channels/{sub}", mw.RequireAuthz(authpkg.ActionGet, "worker", nameFn)(http.HandlerFunc(chh.getChannelResource)))
	mux.Handle("PUT /api/v1/workers/{name}/channels/{channel}", mw.RequireAuthz(authpkg.ActionUpdate, "worker", nameFn)(http.HandlerFunc(chh.putChannel)))
	mux.Handle("GET /api/v1/workers/{name}/channels/{channel}/health", mw.RequireAuthz(authpkg.ActionGet, "worker", nameFn)(http.HandlerFunc(chh.getChannelHealth)))
	mux.Handle("GET /api/v1/workers/{name}/channels/{channel}/qrcode", mw.RequireAuthz(authpkg.ActionGet, "worker", nameFn)(http.HandlerFunc(chh.getChannelQrcode)))
	mux.Handle("GET /api/v1/workers/{name}/channels/{channel}/qrcode/status", mw.RequireAuthz(authpkg.ActionGet, "worker", nameFn)(http.HandlerFunc(chh.getQrcodeStatus)))
	mux.Handle("POST /api/v1/workers/{name}/channels/{channel}/restart", mw.RequireAuthz(authpkg.ActionUpdate, "worker", nameFn)(http.HandlerFunc(chh.restartChannel)))
	mux.Handle("POST /api/v1/workers/{name}/channels/{channel}/conflict-check", mw.RequireAuthz(authpkg.ActionUpdate, "worker", nameFn)(http.HandlerFunc(chh.checkChannelConflict)))

	// W-PR-2: human intervention + lifecycle (write endpoints). All writes go
	// through RequireAuthz ActionUpdate + "project" so the authorizer's
	// requireSameTeam (TeamLeader / L2) rejects cross-team writes at the code
	// level before the handler runs. The handler additionally calls
	// checkProjectAccess after resolving the owning team (middleware cannot
	// resolve project -> team, so requireSameTeam would otherwise short-circuit
	// on an empty ResourceTeam).
	mux.Handle("POST /api/v1/projects", mw.RequireAuthz(authpkg.ActionCreate, "project", nil)(http.HandlerFunc(projh.CreateProject)))
	mux.Handle("POST /api/v1/projects/{id}/pause", mw.RequireAuthz(authpkg.ActionUpdate, "project", projectNameFn)(http.HandlerFunc(projh.PauseProject)))
	mux.Handle("POST /api/v1/projects/{id}/resume", mw.RequireAuthz(authpkg.ActionUpdate, "project", projectNameFn)(http.HandlerFunc(projh.ResumeProject)))
	mux.Handle("POST /api/v1/projects/{id}/replan", mw.RequireAuthz(authpkg.ActionUpdate, "project", projectNameFn)(http.HandlerFunc(projh.ReplanProject)))
	mux.Handle("POST /api/v1/projects/{id}/tasks/{taskId}/cancel", mw.RequireAuthz(authpkg.ActionUpdate, "project", projectTaskNameFn)(http.HandlerFunc(projh.CancelTask)))
	mux.Handle("POST /api/v1/projects/{id}/complete", mw.RequireAuthz(authpkg.ActionUpdate, "project", projectNameFn)(http.HandlerFunc(projh.CompleteProject)))

	// --- Gateway ---
	gh := NewGatewayHandler(deps.Gateway)
	mux.Handle("POST /api/v1/gateway/consumers", mw.RequireAuthz(authpkg.ActionCreate, "gateway", nil)(http.HandlerFunc(gh.CreateConsumer)))
	mux.Handle("POST /api/v1/gateway/consumers/{id}/bind", mw.RequireAuthz(authpkg.ActionUpdate, "gateway", nil)(http.HandlerFunc(gh.BindConsumer)))
	mux.Handle("DELETE /api/v1/gateway/consumers/{id}", mw.RequireAuthz(authpkg.ActionDelete, "gateway", nil)(http.HandlerFunc(gh.DeleteConsumer)))

	// --- Credentials ---
	// STS is self-scoped: no {name} in path; handler uses CallerIdentity to scope the issued token.
	ch := NewCredentialsHandler(deps.STS, deps.Provisioner)
	mux.Handle("POST /api/v1/credentials/sts", mw.RequireAuthz(authpkg.ActionSTS, "credentials", nil)(http.HandlerFunc(ch.RefreshSTS)))
	mux.Handle("POST /api/v1/credentials/matrix-token", mw.RequireAuthz(authpkg.ActionRefreshMatrixToken, "credentials", nil)(http.HandlerFunc(ch.RefreshMatrixToken)))

	// --- AppService management ---
	ash := NewAppServiceHandler(deps.MatrixConfig)
	mux.Handle("POST /api/v1/appservice/rotate-token", mw.RequireAuthz(authpkg.ActionUpdate, "appservice", nil)(http.HandlerFunc(ash.RotateToken)))
	if deps.MatrixConfig.AppServiceEnabled && deps.MatrixConfig.AppServiceHSToken != "" {
		asEvents := NewAppserviceHandler(deps.MatrixConfig.AppServiceHSToken, deps.Client, deps.Namespace)
		mux.Handle("PUT /_matrix/app/v1/transactions/{txnId}", http.HandlerFunc(asEvents.HandleTransactions))
		mux.Handle("GET /_matrix/app/v1/users/{userId}", http.HandlerFunc(asEvents.HandleUserQuery))
		mux.Handle("GET /_matrix/app/v1/rooms/{roomAlias}", http.HandlerFunc(asEvents.HandleRoomQuery))
	}

	// --- Docker API passthrough (embedded mode only) ---
	if deps.KubeMode == "embedded" && deps.SocketPath != "" {
		validator := proxy.NewSecurityValidator()
		proxyHandler := proxy.NewHandler(deps.SocketPath, validator)
		mux.Handle("/docker/", mw.RequireAuthz(authpkg.ActionGateway, "gateway", nil)(http.StripPrefix("/docker", proxyHandler)))
	}

	return s
}

func (s *HTTPServer) Start() error {
	logger := log.Log.WithName("http-server")
	logger.Info("starting unified REST API server", "addr", s.Addr)
	if err := s.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully stops the HTTP server, waiting for in-flight requests
// to finish or ctx to be cancelled. Idempotent.
func (s *HTTPServer) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}
