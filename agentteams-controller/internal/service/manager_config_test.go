package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

// getObjectStub wraps Memory so tests can inject a non-NotExist GetObject error.
type getObjectStub struct {
	*ossfake.Memory
	getErr error
}

func (s *getObjectStub) GetObject(ctx context.Context, key string) ([]byte, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.Memory.GetObject(ctx, key)
}

func TestPutManagerConfig_PreservesModelReasoning(t *testing.T) {
	fake := ossfake.NewMemory()
	ctx := context.Background()

	existing := []byte(`{
		"models": {
			"providers": {
				"agentteams-gateway": {
					"models": [
						{"id": "qwen3.6-plus", "name": "qwen3.6-plus", "reasoning": false},
						{"id": "claude-sonnet-4-6", "name": "claude-sonnet-4-6", "reasoning": true}
					]
				}
			}
		}
	}`)
	if err := fake.PutObject(ctx, "agents/manager/openclaw.json", existing); err != nil {
		t.Fatalf("seed OSS: %v", err)
	}

	lc := NewManagerConfigStore(ManagerConfigStoreConfig{
		OSS:          fake,
		MatrixDomain: "agentteams.local",
		ManagerName:  "manager",
	})

	// Controller regenerate defaults reasoning=true for the active model.
	generated := []byte(`{
		"models": {
			"providers": {
				"agentteams-gateway": {
					"models": [
						{"id": "qwen3.6-plus", "name": "qwen3.6-plus", "reasoning": true},
						{"id": "claude-sonnet-4-6", "name": "claude-sonnet-4-6", "reasoning": true}
					]
				}
			}
		}
	}`)

	if err := lc.PutManagerConfig(generated); err != nil {
		t.Fatalf("PutManagerConfig: %v", err)
	}

	out, err := fake.GetObject(ctx, "agents/manager/openclaw.json")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byID := modelEntriesByID(got)
	if byID["qwen3.6-plus"]["reasoning"] != false {
		t.Errorf("qwen3.6-plus reasoning not preserved: got %v", byID["qwen3.6-plus"]["reasoning"])
	}
	if byID["claude-sonnet-4-6"]["reasoning"] != true {
		t.Errorf("claude-sonnet-4-6 reasoning changed unexpectedly: got %v", byID["claude-sonnet-4-6"]["reasoning"])
	}
}

func TestPutManagerConfig_PrefersLocalReasoningOverStaleOSS(t *testing.T) {
	fake := ossfake.NewMemory()
	ctx := context.Background()
	dir := t.TempDir()
	managerDir := dir + "/manager"
	if err := os.MkdirAll(managerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Stale OSS still has reasoning=true.
	staleOSS := []byte(`{
		"models": {"providers": {"agentteams-gateway": {"models": [
			{"id": "qwen3.6-plus", "reasoning": true}
		]}}}
	}`)
	if err := fake.PutObject(ctx, "agents/manager/openclaw.json", staleOSS); err != nil {
		t.Fatalf("seed OSS: %v", err)
	}
	// Live workspace (model-switch) already wrote reasoning=false.
	local := []byte(`{
		"models": {"providers": {"agentteams-gateway": {"models": [
			{"id": "qwen3.6-plus", "reasoning": false}
		]}}}
	}`)
	if err := os.WriteFile(managerDir+"/openclaw.json", local, 0o600); err != nil {
		t.Fatal(err)
	}

	lc := NewManagerConfigStore(ManagerConfigStoreConfig{
		OSS:          fake,
		MatrixDomain: "agentteams.local",
		ManagerName:  "manager",
		AgentFSDir:   dir,
	})

	generated := []byte(`{
		"models": {"providers": {"agentteams-gateway": {"models": [
			{"id": "qwen3.6-plus", "reasoning": true}
		]}}}
	}`)
	if err := lc.PutManagerConfig(generated); err != nil {
		t.Fatalf("PutManagerConfig: %v", err)
	}

	out, err := fake.GetObject(ctx, "agents/manager/openclaw.json")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if modelEntriesByID(got)["qwen3.6-plus"]["reasoning"] != false {
		t.Fatalf("expected local reasoning=false to win over stale OSS, got %v", modelEntriesByID(got)["qwen3.6-plus"]["reasoning"])
	}
}

func TestUpdateManagerGroupAllowFrom_PrefersLocalConfig(t *testing.T) {
	fake := ossfake.NewMemory()
	ctx := context.Background()
	dir := t.TempDir()
	managerDir := dir + "/manager"
	if err := os.MkdirAll(managerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	staleOSS := []byte(`{
		"channels": {"matrix": {"groupAllowFrom": []}},
		"models": {"providers": {"agentteams-gateway": {"models": [
			{"id": "qwen3.6-plus", "reasoning": true}
		]}}}
	}`)
	if err := fake.PutObject(ctx, "agents/manager/openclaw.json", staleOSS); err != nil {
		t.Fatalf("seed OSS: %v", err)
	}
	local := []byte(`{
		"channels": {"matrix": {"groupAllowFrom": []}},
		"models": {"providers": {"agentteams-gateway": {"models": [
			{"id": "qwen3.6-plus", "reasoning": false}
		]}}}
	}`)
	if err := os.WriteFile(managerDir+"/openclaw.json", local, 0o600); err != nil {
		t.Fatal(err)
	}

	lc := NewManagerConfigStore(ManagerConfigStoreConfig{
		OSS:          fake,
		MatrixDomain: "agentteams.local",
		ManagerName:  "manager",
		AgentFSDir:   dir,
	})
	if err := lc.UpdateManagerGroupAllowFrom("@worker1:agentteams.local", true); err != nil {
		t.Fatalf("UpdateManagerGroupAllowFrom: %v", err)
	}

	out, err := fake.GetObject(ctx, "agents/manager/openclaw.json")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if modelEntriesByID(got)["qwen3.6-plus"]["reasoning"] != false {
		t.Fatalf("groupAllowFrom update revived stale OSS reasoning: got %v", modelEntriesByID(got)["qwen3.6-plus"]["reasoning"])
	}
	allow := got["channels"].(map[string]interface{})["matrix"].(map[string]interface{})["groupAllowFrom"].([]interface{})
	if len(allow) != 1 || allow[0] != "@worker1:agentteams.local" {
		t.Fatalf("groupAllowFrom not updated: %v", allow)
	}
}

func TestPutManagerConfig_MalformedLocalFallsBackToOSS(t *testing.T) {
	fake := ossfake.NewMemory()
	ctx := context.Background()
	dir := t.TempDir()
	managerDir := dir + "/manager"
	if err := os.MkdirAll(managerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	ossCfg := []byte(`{
		"models": {"providers": {"agentteams-gateway": {"models": [
			{"id": "qwen3.6-plus", "reasoning": false}
		]}}}
	}`)
	if err := fake.PutObject(ctx, "agents/manager/openclaw.json", ossCfg); err != nil {
		t.Fatalf("seed OSS: %v", err)
	}
	if err := os.WriteFile(managerDir+"/openclaw.json", []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	lc := NewManagerConfigStore(ManagerConfigStoreConfig{
		OSS:          fake,
		MatrixDomain: "agentteams.local",
		ManagerName:  "manager",
		AgentFSDir:   dir,
	})
	generated := []byte(`{
		"models": {"providers": {"agentteams-gateway": {"models": [
			{"id": "qwen3.6-plus", "reasoning": true}
		]}}}
	}`)
	if err := lc.PutManagerConfig(generated); err != nil {
		t.Fatalf("PutManagerConfig: %v", err)
	}

	out, err := fake.GetObject(ctx, "agents/manager/openclaw.json")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if modelEntriesByID(got)["qwen3.6-plus"]["reasoning"] != false {
		t.Fatalf("expected valid OSS reasoning=false after corrupt local, got %v", modelEntriesByID(got)["qwen3.6-plus"]["reasoning"])
	}
}

func TestUpdateManagerGroupAllowFrom_OSSOnly(t *testing.T) {
	fake := ossfake.NewMemory()
	ctx := context.Background()
	existing := []byte(`{
		"channels": {"matrix": {"groupAllowFrom": []}},
		"models": {"providers": {"agentteams-gateway": {"models": [
			{"id": "qwen3.6-plus", "reasoning": false}
		]}}}
	}`)
	if err := fake.PutObject(ctx, "agents/manager/openclaw.json", existing); err != nil {
		t.Fatalf("seed OSS: %v", err)
	}

	lc := NewManagerConfigStore(ManagerConfigStoreConfig{
		OSS:          fake,
		MatrixDomain: "agentteams.local",
		ManagerName:  "manager",
		// AgentFSDir empty: k8s / OSS-only path.
	})
	if err := lc.UpdateManagerGroupAllowFrom("@worker1:agentteams.local", true); err != nil {
		t.Fatalf("UpdateManagerGroupAllowFrom: %v", err)
	}

	out, err := fake.GetObject(ctx, "agents/manager/openclaw.json")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if modelEntriesByID(got)["qwen3.6-plus"]["reasoning"] != false {
		t.Fatalf("OSS-only update lost reasoning: got %v", modelEntriesByID(got)["qwen3.6-plus"]["reasoning"])
	}
	allow := got["channels"].(map[string]interface{})["matrix"].(map[string]interface{})["groupAllowFrom"].([]interface{})
	if len(allow) != 1 || allow[0] != "@worker1:agentteams.local" {
		t.Fatalf("groupAllowFrom not updated on OSS-only path: %v", allow)
	}
}

func TestUpdateManagerGroupAllowFrom_OSSReadError(t *testing.T) {
	stub := &getObjectStub{Memory: ossfake.NewMemory(), getErr: errors.New("minio unavailable")}
	lc := NewManagerConfigStore(ManagerConfigStoreConfig{
		OSS:          stub,
		MatrixDomain: "agentteams.local",
		ManagerName:  "manager",
	})
	err := lc.UpdateManagerGroupAllowFrom("@worker1:agentteams.local", true)
	if err == nil {
		t.Fatal("expected OSS read error to be returned, got nil")
	}
	if !errors.Is(err, stub.getErr) {
		t.Fatalf("expected wrapped OSS read error, got %v", err)
	}
}

func TestPutManagerConfig_PreservesUserPluginEntries(t *testing.T) {
	fake := ossfake.NewMemory()
	ctx := context.Background()

	// Seed OSS with an existing manager openclaw.json that has user customizations:
	// memory-core dreaming schedule + an extra plugin load path.
	existing := []byte(`{
		"channels": {"matrix": {"groupAllowFrom": ["@worker1:agentteams.local"]}},
		"plugins": {
			"load": {"paths": ["/opt/openclaw/extensions/matrix", "/home/user/my-plugins"]},
			"entries": {
				"memory-core": {
					"enabled": true,
					"config": {"dreaming": {"enabled": true, "frequency": "0 */6 * * *", "timezone": "Asia/Shanghai"}}
				}
			}
		}
	}`)
	if err := fake.PutObject(ctx, "agents/manager/openclaw.json", existing); err != nil {
		t.Fatalf("seed OSS: %v", err)
	}

	lc := NewManagerConfigStore(ManagerConfigStoreConfig{
		OSS:          fake,
		MatrixDomain: "agentteams.local",
		ManagerName:  "manager",
		// AgentFSDir intentionally empty — writeManagerLocalConfig becomes a no-op.
	})

	// Controller regenerates config from CR spec. Defaults overwrite memory-core
	// with a daily schedule and drop the user's custom load path.
	generated := []byte(`{
		"channels": {"matrix": {"groupAllowFrom": []}},
		"plugins": {
			"load": {"paths": ["/opt/openclaw/extensions/matrix"]},
			"entries": {
				"memory-core": {
					"enabled": true,
					"config": {"dreaming": {"enabled": true, "frequency": "0 3 * * *", "timezone": "UTC"}}
				}
			}
		}
	}`)

	if err := lc.PutManagerConfig(generated); err != nil {
		t.Fatalf("PutManagerConfig: %v", err)
	}

	out, err := fake.GetObject(ctx, "agents/manager/openclaw.json")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	plugins := got["plugins"].(map[string]interface{})
	entries := plugins["entries"].(map[string]interface{})
	mc := entries["memory-core"].(map[string]interface{})
	cfg := mc["config"].(map[string]interface{})
	dreaming := cfg["dreaming"].(map[string]interface{})
	if dreaming["frequency"] != "0 */6 * * *" {
		t.Errorf("user dreaming.frequency lost: got %v", dreaming["frequency"])
	}
	if dreaming["timezone"] != "Asia/Shanghai" {
		t.Errorf("user dreaming.timezone lost: got %v", dreaming["timezone"])
	}

	load := plugins["load"].(map[string]interface{})
	paths := load["paths"].([]interface{})
	foundUserPath := false
	for _, p := range paths {
		if p == "/home/user/my-plugins" {
			foundUserPath = true
		}
	}
	if !foundUserPath {
		t.Errorf("user plugin load path lost: paths=%v", paths)
	}

	// Regression: groupAllowFrom merge must still work.
	channels := got["channels"].(map[string]interface{})
	matrix := channels["matrix"].(map[string]interface{})
	allow := matrix["groupAllowFrom"].([]interface{})
	if len(allow) != 1 || allow[0] != "@worker1:agentteams.local" {
		t.Errorf("groupAllowFrom merge broken: got %v", allow)
	}
}

func TestPutManagerConfig_NoExistingOSSObject(t *testing.T) {
	fake := ossfake.NewMemory()
	lc := NewManagerConfigStore(ManagerConfigStoreConfig{
		OSS:          fake,
		MatrixDomain: "agentteams.local",
		ManagerName:  "manager",
	})

	generated := []byte(`{"plugins":{"entries":{"memory-core":{"enabled":true}}}}`)
	if err := lc.PutManagerConfig(generated); err != nil {
		t.Fatalf("PutManagerConfig: %v", err)
	}

	out, err := fake.GetObject(context.Background(), "agents/manager/openclaw.json")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	// First write: no merge happens, generated config is stored verbatim.
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	plugins := got["plugins"].(map[string]interface{})
	entries := plugins["entries"].(map[string]interface{})
	if _, ok := entries["memory-core"]; !ok {
		t.Errorf("memory-core entry missing in first write: %v", entries)
	}
}

func TestPutManagerConfig_MalformedExistingJSON_FallsBackToGenerated(t *testing.T) {
	fake := ossfake.NewMemory()
	ctx := context.Background()
	if err := fake.PutObject(ctx, "agents/manager/openclaw.json", []byte("{not json")); err != nil {
		t.Fatalf("seed OSS: %v", err)
	}

	lc := NewManagerConfigStore(ManagerConfigStoreConfig{
		OSS:          fake,
		MatrixDomain: "agentteams.local",
		ManagerName:  "manager",
	})

	generated := []byte(`{"plugins":{"entries":{"memory-core":{"enabled":true}}}}`)
	if err := lc.PutManagerConfig(generated); err != nil {
		t.Fatalf("PutManagerConfig should swallow malformed existing JSON: %v", err)
	}

	out, err := fake.GetObject(ctx, "agents/manager/openclaw.json")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("written bytes should be valid JSON: %v", err)
	}
	// Generated config wins when existing JSON is corrupt.
	plugins := got["plugins"].(map[string]interface{})
	if _, ok := plugins["entries"].(map[string]interface{})["memory-core"]; !ok {
		t.Errorf("expected generated memory-core entry, got: %v", plugins)
	}
}
