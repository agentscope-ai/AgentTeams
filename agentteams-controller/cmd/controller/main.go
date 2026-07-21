package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/app"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/config"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const shutdownTimeout = 10 * time.Second

func main() {
	ctrl.SetLogger(zap.New())

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg := config.LoadConfig()

	application, err := app.New(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("agentteams-controller is running. Press Ctrl+C to stop.")

	startErr := application.Start(ctx)

	// Start returns once ctx is cancelled (signal) or the manager fails.
	// Stop is called with a fresh deadlined context so the HTTP server and
	// background goroutines get a bounded grace window even after SIGTERM
	// has already cancelled the parent ctx.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer stopCancel()
	if err := application.Stop(stopCtx); err != nil {
		fmt.Fprintf(os.Stderr, "graceful shutdown error: %v\n", err)
	}

	if startErr != nil {
		fmt.Fprintf(os.Stderr, "controller exited with error: %v\n", startErr)
		os.Exit(1)
	}
}
