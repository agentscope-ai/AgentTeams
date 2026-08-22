package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/apiserver"
	"github.com/hiclaw/hiclaw-controller/internal/controller"
	"github.com/hiclaw/hiclaw-controller/internal/executor"
	"github.com/hiclaw/hiclaw-controller/internal/server"
	"github.com/hiclaw/hiclaw-controller/internal/store"
	"github.com/hiclaw/hiclaw-controller/internal/watcher"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	ctrl.SetLogger(zap.New())
	logger := ctrl.Log.WithName("hiclaw-controller")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	kubeMode := os.Getenv("HICLAW_KUBE_MODE")
	if kubeMode == "" {
		kubeMode = "embedded"
	}

	dataDir := os.Getenv("HICLAW_DATA_DIR")
	if dataDir == "" {
		dataDir = "/data/hiclaw-controller"
	}
	// Ensure absolute path
	if !filepath.IsAbs(dataDir) {
		if wd, err := os.Getwd(); err == nil {
			dataDir = filepath.Join(wd, dataDir)
		}
	}

	httpAddr := os.Getenv("HICLAW_HTTP_ADDR")
	if httpAddr == "" {
		httpAddr = ":8090"
	}

	configDir := os.Getenv("HICLAW_CONFIG_DIR")
	if configDir == "" {
		configDir = "/root/hiclaw-fs/hiclaw-config"
	}

	crdDir := os.Getenv("HICLAW_CRD_DIR")
	if crdDir == "" {
		crdDir = "/opt/hiclaw/config/crd"
	}

	// Build scheme
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	if err := v1beta1.AddToScheme(scheme); err != nil {
		logger.Error(err, "failed to add hiclaw types to scheme")
		os.Exit(1)
	}

	// Initialize executors
	shell := executor.NewShell("/opt/hiclaw/agent/skills")
	packages := executor.NewPackageResolver("/tmp/import")

	var mgr ctrl.Manager

	if kubeMode == "embedded" {
		// ── Embedded mode: kine + kube-apiserver + file watcher ──
		logger.Info("starting embedded mode", "dataDir", dataDir, "configDir", configDir)

		// 1. Start kine (SQLite backend)
		kineServer, err := store.StartKine(ctx, store.Config{
			DataDir:       dataDir,
			ListenAddress: "127.0.0.1:2379",
		})
		if err != nil {
			logger.Error(err, "failed to start kine")
			os.Exit(1)
		}
		logger.Info("kine started", "endpoints", kineServer.ETCDConfig.Endpoints)

		// 2. Start embedded kube-apiserver
		restCfg, err := apiserver.Start(ctx, apiserver.Config{
			DataDir:    dataDir,
			EtcdURL:    "http://127.0.0.1:2379",
			BindAddr:   "127.0.0.1",
			SecurePort: "6443",
			CRDDir:     crdDir,
		})
		if err != nil {
			logger.Error(err, "failed to start embedded kube-apiserver")
			os.Exit(1)
		}
		logger.Info("embedded kube-apiserver ready")

		// 3. Create controller-runtime manager
		mgr, err = ctrl.NewManager(restCfg, ctrl.Options{
			Scheme: scheme,
			Metrics: metricsserver.Options{
				BindAddress: "0", // hiclaw-controller only does config reconcile, no metrics needed
			},
		})
		if err != nil {
			logger.Error(err, "failed to create controller manager")
			os.Exit(1)
		}

		// 4. Start file watcher (MinIO mirror → local dir → kine via apiserver)
		fw := watcher.New(configDir, mgr.GetClient())
		if err := fw.InitialSync(ctx); err != nil {
			logger.Error(err, "initial sync failed (non-fatal)")
		}
		go func() {
			if err := fw.Watch(ctx); err != nil && ctx.Err() == nil {
				logger.Error(err, "file watcher stopped unexpectedly")
			}
		}()
		logger.Info("file watcher started", "dir", configDir)

	} else {
		// ── In-cluster mode: connect to K8s API Server directly ──
		logger.Info("starting in-cluster mode")

		restCfg := ctrl.GetConfigOrDie()
		var err error
		mgr, err = ctrl.NewManager(restCfg, ctrl.Options{
			Scheme: scheme,
		})
		if err != nil {
			logger.Error(err, "failed to create controller manager")
			os.Exit(1)
		}
	}

	// 5. Register reconcilers
	if err := (&controller.WorkerReconciler{
		Client:   mgr.GetClient(),
		Executor: shell,
		Packages: packages,
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "failed to setup WorkerReconciler")
		os.Exit(1)
	}

	if err := (&controller.TeamReconciler{
		Client:   mgr.GetClient(),
		Executor: shell,
		Packages: packages,
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "failed to setup TeamReconciler")
		os.Exit(1)
	}

	if err := (&controller.HumanReconciler{
		Client:   mgr.GetClient(),
		Executor: shell,
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "failed to setup HumanReconciler")
		os.Exit(1)
	}

	// 6. Start HTTP API server in background
	go func() {
		httpServer := server.NewHTTPServer(httpAddr, kubeMode)
		if err := httpServer.Start(); err != nil {
			logger.Error(err, "HTTP server failed")
		}
	}()

	// 7. Start controller manager (blocking)
	logger.Info("hiclaw-controller ready", "kubeMode", kubeMode, "httpAddr", httpAddr)
	fmt.Println("hiclaw-controller is running. Press Ctrl+C to stop.")

	if err := mgr.Start(ctx); err != nil {
		logger.Error(err, "controller manager exited with error")
		os.Exit(1)
	}
}
