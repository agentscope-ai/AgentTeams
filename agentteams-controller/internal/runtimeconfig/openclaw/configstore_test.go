package openclaw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

const (
	testConfigPath    = "/workspace/openclaw.json"
	testConfigInitial = `{"agents":{"defaults":{"model":{"primary":"agentteams-gateway/qwen3-max"}}}}`
	pluginsDiscordUp  = `{"plugins":[{"id":"discord","enabled":true,"status":"loaded","channelIds":["discord"]}]}`
	pluginsDiscordOff = `{"plugins":[{"id":"discord","enabled":false,"status":"loaded","channelIds":["discord"]}]}`
)

// fakeExec is a virtual worker container: a virtual filesystem, a plugins
// list, and a gateway appliedConfigHash that — with gwApplyOnWrite — moves
// to the sha256 of freshly written bytes, exactly like the real gateway's
// file watch applying a changed openclaw.json.
type fakeExec struct {
	fs             map[string]string
	plugins        string
	gwHash         *string
	gwErr          error
	gwApplyOnWrite bool
}

func (b *fakeExec) Exec(_ context.Context, _ string, command []string, _ time.Duration) (string, string, int, error) {
	switch {
	case len(command) == 3 && command[0] == "sh" && command[1] == "-c" && strings.Contains(command[2], "QWENPAW_WORKING_DIR"):
		return "/workspace", "", 0, nil
	case len(command) == 3 && command[0] == "sh" && command[1] == "-c" && strings.Contains(command[2], "${HOME}"):
		return "/home/worker", "", 0, nil
	case len(command) == 5 && command[0] == "sh" && command[1] == "-c" && command[2] == `test -f "$1"`:
		if _, ok := b.fs[command[4]]; ok {
			return "", "", 0, nil
		}
		return "", "no such file", 1, nil
	case len(command) == 5 && command[0] == "sh" && command[1] == "-c" && strings.HasPrefix(command[2], "cat"):
		c, ok := b.fs[command[4]]
		if !ok {
			return "", "cat: no such file", 1, nil
		}
		return c, "", 0, nil
	case len(command) >= 4 && command[0] == "openclaw" && command[1] == "plugins":
		return b.plugins, "", 0, nil
	case len(command) >= 4 && command[0] == "openclaw" && command[1] == "gateway":
		if b.gwErr != nil {
			return "", "gateway unavailable: " + b.gwErr.Error(), 1, nil
		}
		h := "null"
		if b.gwHash != nil {
			h = `"` + *b.gwHash + `"`
		}
		return `{"appliedConfigHash":` + h + `}`, "", 0, nil
	case len(command) == 2 && command[0] == "printenv":
		return "", "not defined", 1, nil
	}
	return "", "unhandled: " + strings.Join(command, " "), 127, nil
}

func (b *fakeExec) WriteFile(_ context.Context, _, path, content string) error {
	b.fs[path] = content
	if b.gwApplyOnWrite {
		sum := sha256.Sum256([]byte(content))
		h := hex.EncodeToString(sum[:])
		b.gwHash = &h
	}
	return nil
}

func newFakeExec(plugins string) *fakeExec {
	b := &fakeExec{
		fs:      map[string]string{testConfigPath: testConfigInitial},
		plugins: plugins,
	}
	h := "applied-baseline"
	b.gwHash = &h
	b.gwApplyOnWrite = true
	return b
}

// newTestManager builds a manager with millisecond reconcile timing so
// the tests never wait on the production 10s window.
func newTestManager(t *testing.T, exec *fakeExec) *ConfigManager {
	t.Helper()
	return NewConfigManager(New(), exec, ossfake.NewMemory(),
		WithReconcileWindow(50*time.Millisecond),
		WithReconcileInterval(5*time.Millisecond),
		WithReconcileGrace(30*time.Second),
	)
}

func subagentModelPatch() map[string]any {
	return map[string]any{
		"agents": map[string]any{
			"defaults": map[string]any{
				"subagents": map[string]any{"model": "agentteams-gateway/qwen3.5-flash"},
			},
		},
	}
}

func TestWrite_AppliedWhenGatewayReloads(t *testing.T) {
	exec := newFakeExec(pluginsDiscordUp)
	m := newTestManager(t, exec)
	res, err := m.Write(context.Background(), "w1", subagentModelPatch(), "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.Applied != StateApplied {
		t.Fatalf("applied=%q, want applied", res.Applied)
	}
	if !ValidRev(res.Rev) {
		t.Fatalf("rev=%q, want a well-formed rev", res.Rev)
	}
}

func TestWrite_PendingWhenGatewayNeverReloads(t *testing.T) {
	exec := newFakeExec(pluginsDiscordUp)
	exec.gwApplyOnWrite = false
	m := newTestManager(t, exec)
	res, err := m.Write(context.Background(), "w1", subagentModelPatch(), "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.Applied != StatePending {
		t.Fatalf("applied=%q, want pending (the gateway hash never moved)", res.Applied)
	}
}

func TestWrite_PendingWhenBaselineUnknowable(t *testing.T) {
	exec := newFakeExec(pluginsDiscordUp)
	exec.gwErr = errors.New("gateway not running")
	m := newTestManager(t, exec)
	res, err := m.Write(context.Background(), "w1", subagentModelPatch(), "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.Applied != StatePending {
		t.Fatalf("applied=%q, want pending (baseline capture failed)", res.Applied)
	}
	// The file plane is independent of the gateway: the write landed.
	if exec.fs[testConfigPath] == testConfigInitial {
		t.Fatalf("file not written despite the 200-class result")
	}
}

func TestWrite_AppliedButInactive(t *testing.T) {
	exec := newFakeExec(pluginsDiscordOff)
	m := newTestManager(t, exec)
	res, err := m.Write(context.Background(), "w1", map[string]any{
		"channels": map[string]any{"discord": map[string]any{"enabled": true}},
	}, "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.Applied != StateAppliedButInactive {
		t.Fatalf("applied=%q, want applied-but-inactive", res.Applied)
	}
	if len(res.Inactive) != 1 || res.Inactive[0] != "discord" {
		t.Fatalf("inactive=%v, want [discord]", res.Inactive)
	}
}

func TestWrite_RestartKeysInPendingRestart(t *testing.T) {
	exec := newFakeExec(pluginsDiscordUp)
	m := newTestManager(t, exec)
	res, err := m.Write(context.Background(), "w1", map[string]any{
		"gateway": map[string]any{"port": float64(19000)},
	}, "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(res.PendingRestart) != 1 || res.PendingRestart[0] != "gateway.port" {
		t.Fatalf("pendingRestart=%v, want [gateway.port]", res.PendingRestart)
	}
}

func TestRead_NoDesiredState(t *testing.T) {
	m := newTestManager(t, newFakeExec(pluginsDiscordUp))
	st, err := m.Read(context.Background(), "w1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if st.Applied != StateApplied {
		t.Fatalf("applied=%q, want applied with no desired state", st.Applied)
	}
	if len(st.PendingRestart) != 0 {
		t.Fatalf("pendingRestart=%v, want empty", st.PendingRestart)
	}
}

// TestRead_PendingThenUnappliedThenApplied walks the reconcile state
// machine of a write the gateway has not applied yet: fresh write with an
// unchanged hash -> pending; aged beyond the grace window -> unapplied;
// the gateway finally applies the bytes -> applied.
func TestRead_PendingThenUnappliedThenApplied(t *testing.T) {
	exec := newFakeExec(pluginsDiscordUp)
	exec.gwApplyOnWrite = false
	m := newTestManager(t, exec)
	if _, err := m.Write(context.Background(), "w1", subagentModelPatch(), ""); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Fresh desired state: unchanged hash within the grace -> pending.
	st, err := m.Read(context.Background(), "w1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if st.Applied != StatePending {
		t.Fatalf("applied=%q, want pending (fresh write, hash unchanged)", st.Applied)
	}

	// Age the desired state beyond the grace window -> unapplied.
	raw, err := m.store.GetObject(context.Background(), desiredKey("w1"))
	if err != nil {
		t.Fatalf("read desired: %v", err)
	}
	var d DesiredState
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("unmarshal desired: %v", err)
	}
	d.UpdatedAt = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	aged, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal desired: %v", err)
	}
	if err := m.store.PutObject(context.Background(), desiredKey("w1"), aged); err != nil {
		t.Fatalf("store aged desired: %v", err)
	}
	st, err = m.Read(context.Background(), "w1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if st.Applied != StateUnapplied {
		t.Fatalf("applied=%q, want unapplied (aged, hash unchanged)", st.Applied)
	}

	// The gateway finally applies the written bytes -> applied.
	sum := sha256.Sum256([]byte(exec.fs[testConfigPath]))
	h := hex.EncodeToString(sum[:])
	exec.gwHash = &h
	st, err = m.Read(context.Background(), "w1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if st.Applied != StateApplied {
		t.Fatalf("applied=%q, want applied (hash moved)", st.Applied)
	}
}

// TestRead_LegacyDesiredWithoutBaselineIsPending: a desired.json written
// by a pre-reconcile controller (no baseline fields) cannot be verified —
// it must stay pending, never applied (backward compatibility).
func TestRead_LegacyDesiredWithoutBaselineIsPending(t *testing.T) {
	m := newTestManager(t, newFakeExec(pluginsDiscordUp))
	legacy := `{"rev":"` + revOf([]byte(testConfigInitial)) + `","updatedAt":"` +
		time.Now().UTC().Format(time.RFC3339) + `","writtenKeys":["agents.defaults.model.primary"],"content":{}}`
	if err := m.store.PutObject(context.Background(), desiredKey("w1"), []byte(legacy)); err != nil {
		t.Fatalf("store legacy desired: %v", err)
	}
	st, err := m.Read(context.Background(), "w1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if st.Applied != StatePending {
		t.Fatalf("applied=%q, want pending (no baseline in legacy desired state)", st.Applied)
	}
}

// TestRead_CorruptDesiredTimestampIsError: an unparseable updatedAt is
// corrupt state — fail closed with an error rather than guessing
// freshness.
func TestRead_CorruptDesiredTimestampIsError(t *testing.T) {
	m := newTestManager(t, newFakeExec(pluginsDiscordUp))
	legacy := `{"rev":"` + revOf([]byte(testConfigInitial)) +
		`","baselineHash":"applied-baseline","baselineKnown":true,"updatedAt":"not-a-timestamp","writtenKeys":[],"content":{}}`
	if err := m.store.PutObject(context.Background(), desiredKey("w1"), []byte(legacy)); err != nil {
		t.Fatalf("store desired: %v", err)
	}
	if _, err := m.Read(context.Background(), "w1"); err == nil {
		t.Fatalf("Read must fail closed on a corrupt desired-state timestamp")
	}
}

// TestRead_GatewayDownIsError: with a known baseline, an unreachable
// gateway during a GET is an error (502-class at the HTTP layer), not a
// verdict — consistent with the fail-closed plugin check.
func TestRead_GatewayDownIsError(t *testing.T) {
	exec := newFakeExec(pluginsDiscordUp)
	m := newTestManager(t, exec)
	if _, err := m.Write(context.Background(), "w1", subagentModelPatch(), ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	exec.gwErr = errors.New("gateway not running")
	if _, err := m.Read(context.Background(), "w1"); err == nil {
		t.Fatalf("Read must fail when the gateway is unreachable with a known baseline")
	}
}
