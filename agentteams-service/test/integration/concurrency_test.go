//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/modules/resources"
)

// TestConcurrentCreates exercises the pool + per-create transactions with
// distinct names: every create must commit its row, outbox event and audit
// row, with no cross-transaction leakage.
func TestConcurrentCreates(t *testing.T) {
	h := newHarness(t)
	const n = 20
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("conc-%02d", i)
			_, err := h.store.Create(ctx, worker(name), meta("worker.create"))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent create: %v", err)
		}
	}

	rows, err := h.store.List(ctx, resources.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != n {
		t.Fatalf("rows = %d, want %d", len(rows), n)
	}
	if got := h.count(t, "ops.outbox"); got != n {
		t.Fatalf("outbox = %d, want %d", got, n)
	}
	if got := h.count(t, "ops.audit_events"); got != n {
		t.Fatalf("audit = %d, want %d", got, n)
	}
}

// TestOptimisticLockRace races two UpdateSpec calls on the same version.
// Exactly one wins; the loser gets ErrConflict and must leave no outbox or
// audit trace of its own.
func TestOptimisticLockRace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.Create(ctx, worker("race-w"), meta("worker.create")); err != nil {
		t.Fatalf("create: %v", err)
	}

	base := worker("race-w").Spec()
	a, b := base, base
	a.Model = "model-a"
	b.Model = "model-b"

	type result struct {
		err error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, spec := range []resources.Spec{a, b} {
		wg.Add(1)
		go func(spec resources.Spec) {
			defer wg.Done()
			_, err := h.store.UpdateSpec(ctx, "race-w", spec, 1, meta("worker.update"))
			results <- result{err: err}
		}(spec)
	}
	wg.Wait()
	close(results)

	var ok, conflict int
	for res := range results {
		switch {
		case res.err == nil:
			ok++
		case strings.Contains(res.err.Error(), resources.ErrConflict.Error()):
			conflict++
		default:
			t.Fatalf("unexpected error: %v", res.err)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("ok=%d conflict=%d, want 1/1", ok, conflict)
	}

	got, err := h.store.Get(ctx, "race-w")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Generation != 2 || got.Version != 2 {
		t.Fatalf("gen/ver = %d/%d, want 2/2", got.Generation, got.Version)
	}
	if got.Model != "model-a" && got.Model != "model-b" {
		t.Fatalf("model = %q", got.Model)
	}
	// provision + exactly one update event.
	if n := h.count(t, "ops.outbox"); n != 2 {
		t.Fatalf("outbox = %d, want 2", n)
	}
}

// TestConcurrentDeletes: N deletes at the same version produce exactly one
// deprovision event; every loser must see not-found (the row is soft-deleted
// and invisible to the lock query), with no extra outbox/audit rows.
func TestConcurrentDeletes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.Create(ctx, worker("del-race"), meta("worker.create")); err != nil {
		t.Fatalf("create: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- h.store.SoftDelete(ctx, "del-race", 1, meta("worker.delete"))
		}()
	}
	wg.Wait()
	close(errs)

	var ok, notFound int
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case strings.Contains(err.Error(), resources.ErrNotFound.Error()):
			notFound++
		default:
			t.Fatalf("unexpected delete error: %v", err)
		}
	}
	if ok != 1 || notFound != n-1 {
		t.Fatalf("ok=%d notFound=%d, want 1/%d", ok, notFound, n-1)
	}
	// provision + exactly one deprovision.
	if got := h.count(t, "ops.outbox"); got != 2 {
		t.Fatalf("outbox = %d, want 2", got)
	}
}

// TestConcurrentDeleteVsUpdate races the two mutations at the same version.
// Exactly one wins; the row is either live at generation 2 or deleted at
// generation 1, and only the winner's event is written.
func TestConcurrentDeleteVsUpdate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	for round := 0; round < 8; round++ {
		name := fmt.Sprintf("du-%02d", round)
		if _, err := h.store.Create(ctx, worker(name), meta("worker.create")); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		spec := worker(name).Spec()
		spec.Model = "updated-" + name

		start := make(chan struct{})
		errs := make(chan error, 2)
		go func() {
			<-start
			errs <- h.store.SoftDelete(ctx, name, 1, meta("worker.delete"))
		}()
		go func() {
			<-start
			_, err := h.store.UpdateSpec(ctx, name, spec, 1, meta("worker.update"))
			errs <- err
		}()
		close(start)

		var ok, conflict int
		for i := 0; i < 2; i++ {
			err := <-errs
			switch {
			case err == nil:
				ok++
			case strings.Contains(err.Error(), resources.ErrConflict.Error()),
				strings.Contains(err.Error(), resources.ErrNotFound.Error()):
				conflict++
			default:
				t.Fatalf("%s unexpected error: %v", name, err)
			}
		}
		if ok != 1 || conflict != 1 {
			t.Fatalf("%s ok=%d conflict=%d, want 1/1", name, ok, conflict)
		}

		// Exactly one of the two events (beyond the initial provision).
		if got := h.countWorker(t, name); got != 2 {
			t.Fatalf("%s outbox events = %d, want 2", name, got)
		}

		var (
			deletedAt  *string
			generation int64
			version    int64
		)
		if err := h.res.QueryRow("SELECT deleted_at, generation, version FROM res.workers WHERE name = ?", name).
			Scan(&deletedAt, &generation, &version); err != nil {
			t.Fatalf("%s raw row: %v", name, err)
		}
		if deletedAt != nil {
			// SoftDelete bumps the version but leaves generation unchanged.
			if generation != 1 || version != 2 {
				t.Fatalf("%s deleted at gen/ver %d/%d, want 1/2", name, generation, version)
			}
		} else if generation != 2 || version != 2 {
			t.Fatalf("%s live at gen/ver %d/%d, want 2/2", name, generation, version)
		}
	}
}

func (h *harness) countWorker(t *testing.T, name string) int {
	t.Helper()
	var n int
	if err := h.res.QueryRow("SELECT COUNT(*) FROM ops.outbox WHERE worker_name = ?", name).Scan(&n); err != nil {
		t.Fatalf("count outbox for %s: %v", name, err)
	}
	return n
}

// TestConcurrentCreatesSameName: a same-name create race must yield exactly
// one success and N-1 already_exists answers, never a 1213 deadlock/500.
func TestConcurrentCreatesSameName(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const n = 10

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.store.Create(ctx, worker("same-name"), meta("worker.create"))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	var ok, exists int
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case strings.Contains(err.Error(), resources.ErrAlreadyExists.Error()):
			exists++
		default:
			t.Fatalf("unexpected create error: %v", err)
		}
	}
	if ok != 1 || exists != n-1 {
		t.Fatalf("ok=%d exists=%d, want 1/%d", ok, exists, n-1)
	}
	if got := h.count(t, "res.workers"); got != 1 {
		t.Fatalf("workers = %d, want 1", got)
	}
	if got := h.count(t, "ops.outbox"); got != 1 {
		t.Fatalf("outbox = %d, want 1", got)
	}
}
