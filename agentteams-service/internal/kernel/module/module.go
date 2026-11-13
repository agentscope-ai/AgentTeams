// Package module defines the kernel module lifecycle (任务书 §2.5):
// Init → Start in registration order, Stop in reverse order, aggregated
// health reporting. R1-2a registers only a noop module to prove the
// chain; 2b/2c business modules are a separate layer.
package module

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Status is a health state, worst-first ranked ok < degraded < down.
type Status string

const (
	StatusOK       Status = "ok"
	StatusDegraded Status = "degraded"
	StatusDown     Status = "down"
)

// rankOf maps a status to its severity; an unknown status ranks worse
// than down (a module must report a known status to count as healthy).
func rankOf(s Status) int {
	if r, ok := rank[s]; ok {
		return r
	}
	return 3
}

var rank = map[Status]int{StatusOK: 0, StatusDegraded: 1, StatusDown: 2}

// WorseThan reports whether s is a worse state than o (down > degraded > ok).
// Unknown statuses are worse than down.
func (s Status) WorseThan(o Status) bool {
	return rankOf(s) > rankOf(o)
}

// Health is the aggregated health state of a module.
type Health struct {
	Status Status
	Detail string
}

// Module is a kernel-managed unit of the service.
type Module interface {
	ID() string
	Init(ctx context.Context) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Health(ctx context.Context) Health
}

// Registry owns the module set and its lifecycle ordering.
type Registry struct {
	mu   sync.RWMutex
	mods []Module
	ids  map[string]bool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{ids: make(map[string]bool)}
}

// Register adds a module. A duplicate (or empty) ID is an error.
func (r *Registry) Register(m Module) error {
	if m == nil {
		return errors.New("module: nil module")
	}
	id := m.ID()
	if id == "" {
		return errors.New("module: empty ID")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ids[id] {
		return fmt.Errorf("module: duplicate ID %q", id)
	}
	r.ids[id] = true
	r.mods = append(r.mods, m)
	return nil
}

// Modules returns the registered modules in registration order.
func (r *Registry) Modules() []Module {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Module, len(r.mods))
	copy(out, r.mods)
	return out
}

// InitAll runs Init on every module in registration order, stopping at
// the first error.
func (r *Registry) InitAll(ctx context.Context) error {
	for _, m := range r.Modules() {
		if err := m.Init(ctx); err != nil {
			return fmt.Errorf("module %s: init: %w", m.ID(), err)
		}
	}
	return nil
}

// StartAll runs Start on every module in registration order, stopping at
// the first error.
func (r *Registry) StartAll(ctx context.Context) error {
	for _, m := range r.Modules() {
		if err := m.Start(ctx); err != nil {
			return fmt.Errorf("module %s: start: %w", m.ID(), err)
		}
	}
	return nil
}

// StopAll runs Stop in REVERSE registration order, collecting every error
// into a single errors.Join result (a failing stop never prevents the
// remaining modules from stopping).
func (r *Registry) StopAll(ctx context.Context) error {
	mods := r.Modules()
	var errs []error
	for i := len(mods) - 1; i >= 0; i-- {
		if err := mods[i].Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("module %s: stop: %w", mods[i].ID(), err))
		}
	}
	return errors.Join(errs...)
}

// HealthAll aggregates module health, taking the worst status
// (down > degraded > ok). The detail lists every non-ok module.
func (r *Registry) HealthAll(ctx context.Context) Health {
	worst := StatusOK
	var bad []string
	for _, m := range r.Modules() {
		h := m.Health(ctx)
		if h.Status.WorseThan(worst) {
			worst = h.Status
		}
		if h.Status != StatusOK {
			bad = append(bad, fmt.Sprintf("%s(%s)", m.ID(), h.Status))
		}
	}
	if worst == StatusOK {
		return Health{Status: StatusOK}
	}
	return Health{Status: worst, Detail: strings.Join(bad, ", ")}
}
