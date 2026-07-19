package controller

import (
	"context"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/metrics"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	defaultHealthMonitorInterval = 30 * time.Second

	// Health state constants
	HealthStateHealthy = "healthy"
	HealthStateStalled = "stalled"
	HealthStateZombie  = "zombie"
	HealthStateIdle    = "idle"

	// Thresholds for health classification
	stalledThreshold = 60 * time.Minute // No activity for 60min while having tasks = stalled
	zombieThreshold  = 15 * time.Minute // No heartbeat for 15min = zombie
	idleThreshold    = 5 * time.Minute  // No activity for 5min with no tasks = idle
)

// HealthMonitorController is a background loop that classifies worker health
// states based on heartbeat and activity staleness. It follows the same
// ticker-based pattern as AutoSleepController.
type HealthMonitorController struct {
	client.Client
	Namespace string
	Interval  time.Duration
	Now       func() time.Time
}

func (c *HealthMonitorController) Start(ctx context.Context) error {
	interval := c.Interval
	if interval <= 0 {
		interval = defaultHealthMonitorInterval
	}
	if c.Now == nil {
		c.Now = time.Now
	}

	c.reconcile(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.reconcile(ctx)
		}
	}
}

func (c *HealthMonitorController) reconcile(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("health-monitor")

	var workers v1beta1.WorkerList
	if err := c.List(ctx, &workers, client.InNamespace(c.Namespace)); err != nil {
		logger.Error(err, "list workers")
		return
	}

	now := c.Now()
	for _, worker := range workers.Items {
		newState := c.classifyHealth(now, worker)
		if newState == worker.Status.HealthState {
			continue
		}

		oldState := worker.Status.HealthState
		if err := c.setHealthState(ctx, worker.Name, newState, now); err != nil {
			logger.Error(err, "set health state", "worker", worker.Name, "state", newState)
			continue
		}

		// Emit metrics for state transition
		metrics.ObserveWorkerHealthTransition(oldState, newState)
		metrics.SetWorkerHealthState(worker.Name, newState)

		logger.Info("health state changed",
			"worker", worker.Name,
			"from", oldState,
			"to", newState,
		)
	}
}

// classifyHealth determines the health state for a worker based on its status.
func (c *HealthMonitorController) classifyHealth(now time.Time, worker v1beta1.Worker) string {
	// Only classify running workers
	if worker.Status.Phase != "Running" {
		return ""
	}

	// Parse timestamps
	lastActive := parseTimestamp(worker.Status.LastActiveAt)
	lastHeartbeat := parseTimestamp(worker.Status.LastHeartbeat)

	// No heartbeat data yet — cannot classify
	if lastHeartbeat.IsZero() && lastActive.IsZero() {
		return ""
	}

	// Determine staleness
	activeStale := !lastActive.IsZero() && now.Sub(lastActive) > stalledThreshold
	heartbeatStale := !lastHeartbeat.IsZero() && now.Sub(lastHeartbeat) > zombieThreshold

	// Zombie: no heartbeat for threshold duration
	if heartbeatStale {
		return HealthStateZombie
	}

	// Stalled: has activity but it's stale (likely has tasks but not progressing)
	// We infer "has tasks" from having any LastActiveAt set
	if activeStale && !lastActive.IsZero() {
		return HealthStateStalled
	}

	// Idle: recent heartbeat but no recent activity
	if !lastHeartbeat.IsZero() && now.Sub(lastHeartbeat) < zombieThreshold {
		if lastActive.IsZero() || now.Sub(lastActive) > idleThreshold {
			return HealthStateIdle
		}
	}

	// Healthy: recent activity
	return HealthStateHealthy
}

func (c *HealthMonitorController) setHealthState(ctx context.Context, name, state string, now time.Time) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var worker v1beta1.Worker
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: c.Namespace}, &worker); err != nil {
			return client.IgnoreNotFound(err)
		}
		if worker.Status.HealthState == state {
			return nil
		}
		worker.Status.HealthState = state
		worker.Status.LastHealthTransition = now.UTC().Format(time.RFC3339)
		return c.Status().Update(ctx, &worker)
	})
}

func parseTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
