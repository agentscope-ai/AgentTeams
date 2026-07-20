package metrics

import (
	"context"
	"fmt"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const defaultCRCountRefreshInterval = 30 * time.Second

// CRCountCollector periodically refreshes low-cardinality CR inventory gauges
// from the controller cache.
type CRCountCollector struct {
	Client       client.Client
	Namespace    string
	Interval     time.Duration
	SkipManagers bool
}

func (c *CRCountCollector) Start(ctx context.Context) error {
	if c.Client == nil {
		return fmt.Errorf("cr count collector requires a client")
	}
	interval := c.Interval
	if interval <= 0 {
		interval = defaultCRCountRefreshInterval
	}
	logger := log.FromContext(ctx).WithName("cr-count-collector")
	if err := c.refresh(ctx); err != nil {
		logger.Error(err, "refresh CR counts")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := c.refresh(ctx); err != nil {
				logger.Error(err, "refresh CR counts")
			}
		}
	}
}

func (c *CRCountCollector) refresh(ctx context.Context) error {
	counts := newCRCountSnapshot()
	opts := c.listOptions()

	var workers v1beta1.WorkerList
	if err := c.Client.List(ctx, &workers, opts...); err != nil {
		return fmt.Errorf("list workers: %w", err)
	}
	for i := range workers.Items {
		counts["worker"][boundedCRStatus("worker", workers.Items[i].Status.Phase)]++
	}

	var teams v1beta1.TeamList
	if err := c.Client.List(ctx, &teams, opts...); err != nil {
		return fmt.Errorf("list teams: %w", err)
	}
	for i := range teams.Items {
		counts["team"][boundedCRStatus("team", teams.Items[i].Status.Phase)]++
	}

	var humans v1beta1.HumanList
	if err := c.Client.List(ctx, &humans, opts...); err != nil {
		return fmt.Errorf("list humans: %w", err)
	}
	for i := range humans.Items {
		counts["human"][boundedCRStatus("human", humans.Items[i].Status.Phase)]++
	}

	if !c.SkipManagers {
		var managers v1beta1.ManagerList
		if err := c.Client.List(ctx, &managers, opts...); err != nil {
			return fmt.Errorf("list managers: %w", err)
		}
		for i := range managers.Items {
			counts["manager"][boundedCRStatus("manager", managers.Items[i].Status.Phase)]++
		}
	}

	for kind, statuses := range counts {
		for status, count := range statuses {
			ControllerCRCount.WithLabelValues(kind, status).Set(count)
		}
	}
	return nil
}

func (c *CRCountCollector) listOptions() []client.ListOption {
	if c.Namespace == "" {
		return nil
	}
	return []client.ListOption{client.InNamespace(c.Namespace)}
}

func newCRCountSnapshot() map[string]map[string]float64 {
	counts := make(map[string]map[string]float64, len(crStatusesByKind))
	for kind, statuses := range crStatusesByKind {
		counts[kind] = make(map[string]float64, len(statuses))
		for _, status := range statuses {
			counts[kind][status] = 0
		}
	}
	return counts
}

func boundedCRStatus(kind, status string) string {
	if status == "" {
		return "unknown"
	}
	for _, allowed := range crStatusesByKind[kind] {
		if status == allowed {
			return status
		}
	}
	return "unknown"
}
