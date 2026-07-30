package controller

import (
	"context"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const defaultAutoSleepInterval = time.Minute

type AutoSleepController struct {
	client.Client
	Namespace string
	Interval  time.Duration
	Now       func() time.Time
}

func (c *AutoSleepController) Start(ctx context.Context) error {
	interval := c.Interval
	if interval <= 0 {
		interval = defaultAutoSleepInterval
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

func (c *AutoSleepController) reconcile(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("auto-sleep")

	var workers v1beta1.WorkerList
	if err := c.List(ctx, &workers, client.InNamespace(c.Namespace)); err != nil {
		logger.Error(err, "list workers")
		return
	}
	for _, worker := range workers.Items {
		if !shouldSleep(c.Now(), worker.Spec.DesiredState(), worker.Spec.IdleTimeout, worker.Status.LastActiveAt) {
			continue
		}
		if err := c.setWorkerState(ctx, worker.Name, "Sleeping"); err != nil {
			logger.Error(err, "set worker sleeping", "worker", worker.Name)
		}
	}

	// Team members are Worker CRs referenced from spec.workerMembers, so the
	// Worker loop above covers both standalone and team-bound workers.
}

func shouldSleep(now time.Time, state, idleTimeout, lastActiveAt string) bool {
	if state != "Running" || idleTimeout == "" || lastActiveAt == "" {
		return false
	}
	timeout, err := time.ParseDuration(idleTimeout)
	if err != nil || timeout <= 0 {
		return false
	}
	lastActive, err := time.Parse(time.RFC3339, lastActiveAt)
	if err != nil {
		return false
	}
	return now.Sub(lastActive) > timeout
}

func (c *AutoSleepController) setWorkerState(ctx context.Context, name, state string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var worker v1beta1.Worker
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: c.Namespace}, &worker); err != nil {
			return client.IgnoreNotFound(err)
		}
		if worker.Spec.DesiredState() == state {
			return nil
		}
		worker.Spec.State = &state
		return c.Update(ctx, &worker)
	})
}
