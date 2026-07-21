//go:build integration

package controller_test

import (
	"context"
	"sync"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
)

// labelCapture is a tiny helper around mockBackend.CreateFn that lets
// integration tests read back the exact Labels map handed to
// backend.Create for a given container/member name. Needed because
// MockWorkerBackend.Calls.Create only records names — the four-layer
// merge output the reconciler hands the backend would otherwise be
// invisible to the test.
type labelCapture struct {
	mu     sync.Mutex
	byName map[string]map[string]string
}

func newLabelCapture() *labelCapture {
	return &labelCapture{byName: map[string]map[string]string{}}
}

// CreateFn returns a CreateFn suitable for mockBackend.CreateFn. It
// preserves the mock's default Running-state side effect (so Status
// and downstream reconcile phases still converge to Running) and
// records the captured Labels keyed by req.Name.
func (c *labelCapture) CreateFn() func(ctx context.Context, req backend.CreateRequest) (*backend.WorkerResult, error) {
	return func(_ context.Context, req backend.CreateRequest) (*backend.WorkerResult, error) {
		c.mu.Lock()
		copyLabels := make(map[string]string, len(req.Labels))
		for k, v := range req.Labels {
			copyLabels[k] = v
		}
		c.byName[req.Name] = copyLabels
		c.mu.Unlock()
		return &backend.WorkerResult{
			Name:    req.Name,
			Backend: "mock",
			Status:  backend.StatusStarting,
		}, nil
	}
}

// LabelsFor returns the captured labels for the given Create request
// name, or nil if no Create for that name has been observed yet.
// Returns a copy so callers can read it without holding the lock.
func (c *labelCapture) LabelsFor(name string) map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	labels, ok := c.byName[name]
	if !ok {
		return nil
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}

// Keys returns the set of names for which Create requests have been
// captured, useful for test diagnostics when an expected name never
// showed up.
func (c *labelCapture) Keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.byName))
	for k := range c.byName {
		out = append(out, k)
	}
	return out
}

// assertLabel is a small readability helper: fail the test if
// labels[key] != want, showing the full map in the failure message so
// the test log makes the mismatch obvious.
func assertLabel(t *testing.T, labels map[string]string, key, want string) {
	t.Helper()
	if got := labels[key]; got != want {
		t.Errorf("label[%q] = %q, want %q (full=%v)", key, got, want, labels)
	}
}
