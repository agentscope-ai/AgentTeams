package hermes

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

// Execute the manager's real shell file operations against a temporary home.
// No Hermes process or runtime hot-application is represented by this fixture.
type policyFileExec struct{ home string }

func (e policyFileExec) Exec(ctx context.Context, _ string, command []string, _ time.Duration) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Env = append(os.Environ(), "HERMES_HOME="+e.home)
	var out, errout bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errout
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return out.String(), errout.String(), exit.ExitCode(), nil
	}
	return out.String(), errout.String(), 0, err
}
func (e policyFileExec) WriteFile(_ context.Context, _, path, body string) error {
	return os.WriteFile(path, []byte(body), 0600)
}

func TestConfigPolicyCoversWriteAndRollback(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	path := filepath.Join(home, "config.yaml")
	initial := []byte("model:\n  default: old-model\n  provider: original\n")
	if err := os.WriteFile(path, initial, 0600); err != nil {
		t.Fatal(err)
	}
	store := ossfake.NewMemory()
	m := NewConfigManager(New(), policyFileExec{home}, store)
	target := []byte("model:\n  default: snapshot-model\n  provider: original\n")
	rev := revOf(target)
	if err := store.PutObject(ctx, snapshotKey("worker", rev), target); err != nil {
		t.Fatal(err)
	}
	denied := errors.New("model denied")
	calls := 0
	m.SetChangeValidator(func(_ context.Context, current, next map[string]any) error {
		calls++
		old := current["model"].(map[string]any)
		newModel := next["model"].(map[string]any)
		if old["default"] != "old-model" || newModel["provider"] != "original" {
			t.Fatalf("validator did not see full current/target documents")
		}
		return denied
	})
	for _, action := range []string{"write", "rollback"} {
		t.Run(action, func(t *testing.T) {
			var err error
			if action == "write" {
				_, err = m.Write(ctx, "worker", map[string]any{"model": map[string]any{"default": "new-model"}}, "")
			} else {
				_, err = m.Rollback(ctx, "worker", rev)
			}
			if !errors.Is(err, denied) {
				t.Fatalf("error=%v", err)
			}
			data, _ := os.ReadFile(path)
			if !bytes.Equal(data, initial) {
				t.Fatal("rejected mutation changed file")
			}
			if err := store.Stat(ctx, snapshotKey("worker", revOf(initial))); err == nil {
				t.Fatal("rejected mutation created snapshot")
			}
		})
	}
	if calls != 2 {
		t.Fatalf("policy calls=%d", calls)
	}
	m.SetChangeValidator(func(context.Context, map[string]any, map[string]any) error { return nil })
	if _, err := m.Write(ctx, "worker", map[string]any{"model": map[string]any{"default": "allowed"}}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Rollback(ctx, "worker", revOf(initial)); err != nil {
		t.Fatal(err)
	}
	back, _ := os.ReadFile(path)
	if !bytes.Equal(back, initial) {
		t.Fatal("authorized rollback did not restore initial bytes")
	}
}

func TestConfigRollbackPolicyCanAuthorizeRecoveryOfInvalidCurrentFile(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	path := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(path, []byte("broken: ["), 0600); err != nil {
		t.Fatal(err)
	}
	store := ossfake.NewMemory()
	target := []byte("model:\n  default: restored\n")
	rev := revOf(target)
	if err := store.PutObject(ctx, snapshotKey("worker", rev), target); err != nil {
		t.Fatal(err)
	}
	m := NewConfigManager(New(), policyFileExec{home}, store)
	m.SetChangeValidator(func(_ context.Context, current, next map[string]any) error {
		if current != nil || next["model"] == nil {
			t.Fatal("invalid-current recovery boundary missing")
		}
		return nil
	})
	if _, err := m.Rollback(ctx, "worker", rev); err != nil {
		t.Fatal(err)
	}
	back, _ := os.ReadFile(path)
	if !bytes.Equal(back, target) {
		t.Fatal("recovery failed")
	}
}
