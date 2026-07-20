package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func TestPutManagerConfig_PreservesUserPluginEntries(t *testing.T) {
	fake := ossfake.NewMemory()
	ctx := context.Background()

	// Seed OSS with an existing manager openclaw.json that has user customizations:
	// memory-core dreaming schedule + an extra plugin load path.
	existing := []byte(`{
		"channels": {"matrix": {"groupAllowFrom": ["@worker1:hiclaw.local"]}},
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

	lc := NewLegacyCompat(LegacyConfig{
		OSS:          fake,
		MatrixDomain: "hiclaw.local",
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
	if len(allow) != 1 || allow[0] != "@worker1:hiclaw.local" {
		t.Errorf("groupAllowFrom merge broken: got %v", allow)
	}
}

func TestPutManagerConfig_NoExistingOSSObject(t *testing.T) {
	fake := ossfake.NewMemory()
	lc := NewLegacyCompat(LegacyConfig{
		OSS:          fake,
		MatrixDomain: "hiclaw.local",
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

	lc := NewLegacyCompat(LegacyConfig{
		OSS:          fake,
		MatrixDomain: "hiclaw.local",
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
