package openclaw

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

// concurrentConfigExec pauses one real file replacement while another
// request-scoped manager tries to mutate the same worker.
type concurrentConfigExec struct {
	mu      sync.Mutex
	exec    *fakeExec
	pause   bool
	entered chan struct{}
	release chan struct{}
}

func (e *concurrentConfigExec) Exec(ctx context.Context, worker string, command []string, timeout time.Duration) (string, string, int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.exec.Exec(ctx, worker, command, timeout)
}

func (e *concurrentConfigExec) WriteFile(ctx context.Context, worker, path, content string) error {
	e.mu.Lock()
	pause := e.pause
	e.pause = false
	e.mu.Unlock()
	if pause {
		close(e.entered)
		select {
		case <-e.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.exec.WriteFile(ctx, worker, path, content)
}

func TestConfigManagersSerializeMutations(t *testing.T) {
	for _, mode := range []string{"same_revision", "without_revision", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			exec := &concurrentConfigExec{exec: newFakeExec(pluginsDiscordUp), entered: make(chan struct{}), release: make(chan struct{})}
			store := ossfake.NewMemory()
			opts := []Option{WithReconcileWindow(time.Millisecond), WithReconcileInterval(time.Millisecond)}
			first := NewConfigManager(New(), exec, store, opts...)
			second := NewConfigManager(New(), exec, store, opts...)
			initialRev := revOf([]byte(testConfigInitial))
			currentRev := initialRev
			if mode == "rollback" {
				initial, err := first.Write(ctx, "w1", map[string]any{"hooks": map[string]any{"enabled": true}}, initialRev)
				if err != nil {
					t.Fatal(err)
				}
				currentRev = initial.Rev
			}
			exec.pause = true
			type outcome struct {
				result WriteResult
				err    error
			}
			a := make(chan outcome, 1)
			b := make(chan outcome, 1)
			go func() {
				r, err := first.Write(ctx, "w1", map[string]any{"gateway": map[string]any{"publicOrigin": "https://example.invalid"}}, currentRev)
				a <- outcome{r, err}
			}()
			select {
			case <-exec.entered:
			case <-ctx.Done():
				t.Fatal("first mutation did not reach file replacement")
			}
			started := make(chan struct{})
			go func() {
				close(started)
				var r WriteResult
				var err error
				if mode == "rollback" {
					r, err = second.Rollback(ctx, "w1", initialRev)
				} else {
					rev := currentRev
					if mode == "without_revision" {
						rev = ""
					}
					r, err = second.Write(ctx, "w1", map[string]any{"hooks": map[string]any{"enabled": true}}, rev)
				}
				b <- outcome{r, err}
			}()
			<-started
			// The old code finishes B while A is paused. The fixed code lets B
			// proceed only after A's file, snapshot and desired-state commit.
			var early *outcome
			select {
			case r := <-b:
				early = &r
			case <-time.After(100 * time.Millisecond):
			}
			close(exec.release)
			var one, two outcome
			select {
			case one = <-a:
			case <-ctx.Done():
				t.Fatal("first mutation did not finish")
			}
			if early != nil {
				two = *early
			} else {
				select {
				case two = <-b:
				case <-ctx.Done():
					t.Fatal("second mutation did not finish")
				}
			}
			if one.err != nil {
				t.Fatal(one.err)
			}
			if mode == "same_revision" {
				var stale *StaleRevisionError
				if !errors.As(two.err, &stale) {
					t.Fatalf("second request with the same revision must conflict, got %v", two.err)
				}
				if stale.Current != one.result.Rev {
					t.Fatalf("conflict revision %s, want committed %s", stale.Current, one.result.Rev)
				}
				return
			}
			if two.err != nil {
				t.Fatal(two.err)
			}
			if mode == "rollback" {
				if two.result.Snapshot != snapshotKey("w1", one.result.Rev) {
					t.Fatalf("rollback snapshot %s did not preserve concurrent successful write %s", two.result.Snapshot, one.result.Rev)
				}
				raw, err := store.GetObject(ctx, two.result.Snapshot)
				if err != nil || revOf(raw) != one.result.Rev {
					t.Fatalf("rollback snapshot does not contain preceding write: %v", err)
				}
			} else {
				state, err := second.Read(ctx, "w1")
				if err != nil {
					t.Fatal(err)
				}
				hooks, _ := state.Values["hooks"].(map[string]any)
				gateway, _ := state.Values["gateway"].(map[string]any)
				if hooks["enabled"] != true || gateway["publicOrigin"] != "https://example.invalid" {
					t.Fatalf("concurrent patches were lost: %#v", state.Values)
				}
			}
			desired, err := second.loadDesired(ctx, "w1")
			if err != nil || desired == nil || desired.Rev != two.result.Rev {
				t.Fatalf("desired state does not match the last mutation: %#v %v", desired, err)
			}
		})
	}
}

func TestReadRedactsUndeclaredCredentialFields(t *testing.T) {
	exec := newFakeExec(pluginsDiscordUp)
	exec.fs[testConfigPath] = `{"channels":{"matrix":{"accessToken":"synthetic-matrix-token","homeserver":"https://matrix.invalid"}},"plugins":{"entries":{"example":{"config":{"api_key":"synthetic-api-key"}}}},"env":{"vars":{"SERVICE_PASSWORD":"synthetic-password"}}}`
	m := newTestManager(t, exec)
	state, err := m.Read(context.Background(), "w1")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"channels.matrix.accessToken", "plugins.entries.example.config.api_key", "env.vars.SERVICE_PASSWORD"} {
		if got, _ := docAt(state.Values, key); got != "***" {
			t.Errorf("credential field %s was not redacted", key)
		}
	}
	if got, _ := docAt(state.Values, "channels.matrix.homeserver"); got != "https://matrix.invalid" {
		t.Fatal("non-credential channel field changed")
	}
}

type pauseAfterConfigWrite struct {
	*concurrentConfigExec
}

func (e *pauseAfterConfigWrite) WriteFile(ctx context.Context, worker, path, content string) error {
	e.mu.Lock()
	err := e.exec.WriteFile(ctx, worker, path, content)
	e.mu.Unlock()
	if err != nil {
		return err
	}
	close(e.entered)
	select {
	case <-e.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestReadDoesNotReportUncommittedConfigApplied(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	virtual := newFakeExec(pluginsDiscordUp)
	virtual.gwApplyOnWrite = false
	exec := &pauseAfterConfigWrite{&concurrentConfigExec{exec: virtual, entered: make(chan struct{}), release: make(chan struct{})}}
	store := ossfake.NewMemory()
	opts := []Option{WithReconcileWindow(time.Millisecond), WithReconcileInterval(time.Millisecond)}
	writer := NewConfigManager(New(), exec, store, opts...)
	reader := NewConfigManager(New(), exec, store, opts...)
	writeDone := make(chan error, 1)
	go func() {
		_, err := writer.Write(ctx, "w1", map[string]any{"hooks": map[string]any{"enabled": true}}, "")
		writeDone <- err
	}()
	select {
	case <-exec.entered:
	case <-ctx.Done():
		t.Fatal("write did not reach commit")
	}
	type result struct {
		state ConfigState
		err   error
	}
	readDone := make(chan result, 1)
	go func() { state, err := reader.Read(ctx, "w1"); readDone <- result{state, err} }()
	var early *result
	select {
	case r := <-readDone:
		early = &r
	case <-time.After(100 * time.Millisecond):
	}
	close(exec.release)
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("write did not finish")
	}
	var r result
	if early != nil {
		r = *early
	} else {
		select {
		case r = <-readDone:
		case <-ctx.Done():
			t.Fatal("read did not finish")
		}
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.state.Applied != StatePending {
		t.Fatalf("unapplied on-disk edit was reported %q before desired-state commit", r.state.Applied)
	}
}
