// Command agentteams-service is the service entrypoint.
//
// Boot order (任务书 §2.8): config → log → db → migrate → module registry
// (kernel noop proves the lifecycle chain) → business wiring (resources
// store/handlers + identity review cache) → httpapi + uds health faces →
// wait for signal → graceful shutdown (HTTP faces first, then modules in
// reverse, then the DB pools deferred).
//
// Exit codes: 2 for configuration errors, 1 for runtime errors, 0 for a
// clean shutdown (and for G1 disabled mode, which boots nothing).
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/adapter"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/audit"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/config"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/db"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/httpapi"
	idemstore "github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/idem"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/identity"
	klog "github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/log"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/module"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/outbox"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/uds"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/modules/resources"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run is the testable entrypoint.
func run(args []string) int {
	cfg, err := config.Load(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 2
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	lg, err := klog.New(cfg.LogLevel, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "log:", err)
		return 2
	}

	if !cfg.Enabled {
		lg.Info("service disabled (G1 dual mode); nothing to do",
			"hint", "set "+config.EnvEnabled+"=true to serve the worker API")
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pools, err := db.OpenAll(ctx, cfg.ResDSN, cfg.OpsDSN)
	if err != nil {
		lg.Error("open database pools", "err", err)
		return 1
	}
	defer pools.Close()

	if err := db.Migrate(ctx, cfg.ResDSN, cfg.OpsDSN, cfg.ServiceID, lg); err != nil {
		lg.Error("run migrations", "err", err)
		return 1
	}

	// --- business wiring (R1 resources slice) -------------------------
	//
	// The store's transactions run on the res pool and address the ops
	// tables with schema-qualified names, so the ResDSN account must hold
	// INSERT/SELECT on ops (deployment prerequisite, 58 §4).
	store := resources.NewSQLStore(pools.Res)
	reviewClient := adapter.New(cfg.ControllerURL, cfg.InternalToken)
	reviews := identity.NewCache(reviewClient)

	idemStore := idemstore.NewSQLStore(pools.Res)
	handler := &resources.Handler{
		Store:          store,
		Idem:           idemStore,
		Log:            lg.With("component", "resources"),
		DefaultRuntime: cfg.DefaultWorkerRuntime,
		DefaultModel:   cfg.DefaultModel,
		Auditor:        &denyAuditor{db: pools.Res, log: lg.With("component", "audit")},
	}

	proxy := &resources.Proxy{
		Store:         store,
		ControllerURL: cfg.ControllerURL,
		Log:           lg.With("component", "proxy"),
	}
	status := &resources.StatusHandler{Store: store, Log: lg.With("component", "status")}
	dispatcher := &outbox.Dispatcher{
		DB:       pools.Ops,
		Client:   reviewClient,
		Interval: cfg.OutboxInterval,
		Log:      lg.With("component", "outbox"),
	}

	// get-by-name cache-aside (58 §1.3/§5): the controller is authoritative
	// for execution state and existence; an unreachable controller degrades
	// to the stored row.
	handler.ResolveExecution = func(ctx context.Context, name string) (resources.Observed, int64, bool, error) {
		ex, err := reviewClient.Execution(ctx, name)
		if err != nil {
			if errors.Is(err, adapter.ErrNotFound) {
				return resources.Observed{}, 0, false, nil
			}
			return resources.Observed{}, 0, false, err
		}
		return resources.Observed{
			Phase:            ex.Phase,
			ContainerState:   ex.ContainerState,
			MatrixUserID:     ex.MatrixUserID,
			RoomID:           ex.RoomID,
			Message:          ex.Message,
			LastActiveAt:     parseObservedTime(ex.LastActiveAt),
			AgentStatus:      ex.AgentStatus,
			RunningTaskCount: ex.RunningTaskCount,
			LastRunAt:        parseObservedTime(ex.LastRunAt),
			LastFinishAt:     parseObservedTime(ex.LastFinishAt),
		}, ex.Generation, true, nil
	}

	reg := module.NewRegistry()
	if err := reg.Register(&noopModule{id: "kernel-noop"}); err != nil {
		lg.Error("register noop module", "err", err)
		return 1
	}
	if err := reg.InitAll(ctx); err != nil {
		lg.Error("init modules", "err", err)
		return 1
	}
	if err := reg.StartAll(ctx); err != nil {
		lg.Error("start modules", "err", err)
		return 1
	}

	api := httpapi.New(lg.With("component", "http"))
	api.AddReadyCheck("db", func(ctx context.Context) module.Health {
		if err := pools.Health(ctx); err != nil {
			// Detail must stay DSN-free: /readyz is unauthenticated and a
			// raw driver error can carry host/port/user (58 §2.6). The full
			// error stays in the server log for diagnosis.
			lg.Warn("readyz db check failed", "err", err)
			return module.Health{Status: module.StatusDown, Detail: "database unreachable"}
		}
		return module.Health{Status: module.StatusOK}
	})
	api.AddReadyCheck("modules", reg.HealthAll)

	// Public face: every native route resolves the caller through the
	// controller token-review cache (58 §5), so handlers only see an
	// authenticated identity.
	authed := identity.Middleware(reviews)
	api.Mount("POST /api/v1/workers", authed(handler.CreateHTTP()))
	api.Mount("GET /api/v1/workers", authed(handler.ListHTTP()))
	api.Mount("GET /api/v1/workers/{name}", authed(handler.GetHTTP()))
	// D110 = A1: PUT/DELETE keep the controller as write authority and are
	// forwarded byte-for-byte; the service mirrors the outcome into
	// res.workers (write-through / status-code rule).
	api.Mount("PUT /api/v1/workers/{name}", authed(proxy.UpdateHTTP()))
	api.Mount("DELETE /api/v1/workers/{name}", authed(proxy.DeleteHTTP()))
	// Controller → service observation writeback (58 §5): shared bearer +
	// RFC3339 timestamp inside the ±5min replay window + request id.
	api.Mount("POST /internal/v1/workers/status",
		httpapi.InternalAuth(cfg.InternalToken, config.ReplayWindow, nil, status.ReceiveHTTP()))

	tcpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Default is 1 MiB of headers; worker requests carry a bearer token
		// and small custom headers, so 64 KiB is already generous.
		MaxHeaderBytes: 64 << 10,
	}
	var udsSrv *uds.Server
	if cfg.UDS != "" {
		udsSrv = uds.New(cfg.UDS, api.HealthHandler(), lg.With("component", "uds"))
		if err := udsSrv.Listen(); err != nil {
			lg.Error("uds listen", "err", err)
			return 1
		}
		go udsSrv.Serve()
	}

	// The outbox dispatcher is the only writer of provision/deprovision
	// deliveries. Run it on a child context so shutdown can stop it even on
	// the listen-error path.
	// Expired idempotency rows ride the dispatcher cadence (58 §4.5): same
	// process, same interval, no separate scheduler.
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			if n, err := idemStore.DeleteExpired(ctx); err != nil {
				lg.Warn("idempotency cleanup failed", "err", err)
			} else if n > 0 {
				lg.Info("idempotency rows expired", "rows", n)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	dispCtx, cancelDispatcher := context.WithCancel(ctx)
	defer cancelDispatcher()
	dispDone := make(chan struct{})
	go func() {
		defer close(dispDone)
		dispatcher.Run(dispCtx)
	}()

	listenErr := make(chan error, 1)
	go func() {
		if err := tcpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
		}
	}()

	lg.Info("service started",
		"service_id", cfg.ServiceID,
		"listen", cfg.ListenAddr,
		"uds", cfg.UDS,
		"controller", cfg.ControllerURL)

	// Graceful shutdown: HTTP faces first, then modules in reverse,
	// then the DB pools (deferred). Bounded by ShutdownTimeout.
	shutdown := func() {
		sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := tcpSrv.Shutdown(sctx); err != nil {
			lg.Warn("http shutdown incomplete (forced)", "err", err)
		}
		if udsSrv != nil {
			if err := udsSrv.Shutdown(sctx); err != nil {
				lg.Warn("uds shutdown incomplete (forced)", "err", err)
			}
		}
		if err := reg.StopAll(sctx); err != nil {
			lg.Warn("module stop", "err", err)
		}
		cancelDispatcher()
		select {
		case <-dispDone:
		case <-time.After(2 * time.Second):
			lg.Warn("outbox dispatcher did not stop in time")
		}
	}

	code := 0
	select {
	case <-ctx.Done():
		lg.Info("shutdown signal received", "timeout", cfg.ShutdownTimeout)
	case err := <-listenErr:
		lg.Error("http server failed", "err", err)
		code = 1
	}
	shutdown()
	lg.Info("service stopped")
	return code
}

// denyAuditor persists handler rejections (4xx/5xx decisions) to
// ops.audit_events (58 §2: every decision is auditable). It is best-effort:
// an audit failure is logged but never changes the client-visible response.
type denyAuditor struct {
	db  *sql.DB
	log *slog.Logger
}

func (d *denyAuditor) Deny(ctx context.Context, meta *resources.AuditMeta, code, reason string) error {
	if meta == nil {
		meta = &resources.AuditMeta{}
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		d.log.Warn("audit deny: begin", "err", err)
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := audit.Record(ctx, tx, audit.Event{
		RequestID:       meta.RequestID,
		ActorType:       meta.ActorType,
		ActorID:         meta.ActorID,
		OnBehalfOfType:  meta.OnBehalfOfType,
		OnBehalfOfID:    meta.OnBehalfOfID,
		TeamUID:         meta.TeamUID,
		ProjectUID:      meta.ProjectUID,
		TaskID:          meta.TaskID,
		Action:          meta.Action,
		Decision:        "deny",
		DenyReason:      truncate(reason+" ("+code+")", 255),
		TokenJTI:        meta.TokenJTI,
		Client:          meta.Client,
		ResourceVersion: meta.ResourceVersion,
	}, time.Now().UTC()); err != nil {
		d.log.Warn("audit deny: record", "err", err)
		return err
	}
	return tx.Commit()
}

// parseObservedTime parses the controller's RFC3339 status timestamps; an
// empty or malformed value is "not reported" (nil).
func parseObservedTime(raw string) *time.Time {
	if raw == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil
	}
	return &t
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// noopModule proves the module lifecycle chain end-to-end (任务书 §2.8:
// "R1-2a 先注册一个 noop 模块证明链路").
type noopModule struct{ id string }

func (m *noopModule) ID() string                      { return m.id }
func (m *noopModule) Init(ctx context.Context) error  { return nil }
func (m *noopModule) Start(ctx context.Context) error { return nil }
func (m *noopModule) Stop(ctx context.Context) error  { return nil }
func (m *noopModule) Health(ctx context.Context) module.Health {
	return module.Health{Status: module.StatusOK}
}
