package agentconfig

import (
	"encoding/json"
	"testing"
)

func TestOpenClawHeartbeatSwitch(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name      string
		enabled   *bool
		inherited *HeartbeatConfig
		want      string
	}{
		{"default off", nil, nil, "0m"},
		{"enabled", &on, nil, "30m"},
		{"disabled", &off, nil, "0m"},
		{"inherited cadence", nil, &HeartbeatConfig{Enabled: true, Every: "15m"}, "15m"},
		{"override inherited cadence", &off, &HeartbeatConfig{Enabled: true, Every: "15m"}, "0m"},
		{"enable without team cadence", &on, &HeartbeatConfig{Every: "0m"}, "30m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGenerator(Config{})
			data, err := g.GenerateOpenClawConfig(WorkerConfigRequest{Runtime: "openclaw", LlmHeartbeatEnabled: tc.enabled, Heartbeat: tc.inherited})
			if err != nil {
				t.Fatal(err)
			}
			var cfg map[string]any
			if err := json.Unmarshal(data, &cfg); err != nil {
				t.Fatal(err)
			}
			hb := cfg["agents"].(map[string]any)["defaults"].(map[string]any)["heartbeat"].(map[string]any)
			if hb["every"] != tc.want {
				t.Fatalf("heartbeat = %#v, want every=%s", hb, tc.want)
			}
			if _, ok := hb["enabled"]; ok {
				t.Fatal("OpenClaw rejects the heartbeat.enabled key")
			}
		})
	}
}

func TestInjectHeartbeatExplicitlyDisablesOpenClaw(t *testing.T) {
	data := InjectHeartbeat([]byte(`{"agents":{"defaults":{"heartbeat":{"every":"30m"}}}}`), false, "", "openclaw")
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	hb := cfg["agents"].(map[string]any)["defaults"].(map[string]any)["heartbeat"].(map[string]any)
	if hb["every"] != "0m" {
		t.Fatalf("heartbeat = %#v", hb)
	}
}
