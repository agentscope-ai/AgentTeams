package server

import (
	"context"
	"errors"
	"net/http"

	authpkg "github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/credentials"
	"github.com/hiclaw/hiclaw-controller/internal/gateway"
	"github.com/hiclaw/hiclaw-controller/internal/matrix"
	"github.com/hiclaw/hiclaw-controller/internal/oss"
	"github.com/hiclaw/hiclaw-controller/internal/proxy"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ServerDeps aggregates all dependencies needed by the HTTP API handlers.
type ServerDeps struct {
	Client         client.Client
	Backend        *backend.Registry
	Gateway        gateway.Client
	OSS            oss.StorageClient
	STS            *credentials.STSService
	AuthMw         *authpkg.Middleware
	KubeMode       string
	Namespace      string
	ControllerName string               // HICLAW_CONTROLLER_NAME; empty in embedded mode
	SocketPath     string               // Docker proxy (embedded only)
	MatrixConfig   matrix.Config        // for AppService rotation endpoint
	Provisioner    *service.Provisioner // for Matrix token refresh
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
