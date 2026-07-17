package service

import (
	"path/filepath"
	"testing"
)

// TestBuiltinAgentDirRouting asserts builtinAgentDir maps each role/runtime to
// the embedded template directory. openhuman and qwenpaw were previously
// unmapped and fell through to the generic worker-agent dir (Issue 8/16), so a
// worker of that runtime got the wrong AGENTS.md/skills.
func TestBuiltinAgentDirRouting(t *testing.T) {
	workerAgentDir := filepath.Join("opt", "hiclaw", "agent", "worker-agent")
	base := filepath.Dir(workerAgentDir)
	d := &Deployer{workerAgentDir: workerAgentDir}

	tests := []struct {
		name    string
		role    string
		runtime string
		want    string
	}{
		{"team leader", "team_leader", "", filepath.Join(base, "team-leader-agent")},
		{"copaw worker", "worker", "copaw", filepath.Join(base, "copaw-worker-agent")},
		{"hermes worker", "worker", "hermes", filepath.Join(base, "hermes-worker-agent")},
		{"openhuman worker", "worker", "openhuman", filepath.Join(base, "openhuman-worker-agent")},
		{"qwenpaw worker", "worker", "qwenpaw", filepath.Join(base, "qwenpaw-worker-agent")},
		{"openclaw falls back to default", "worker", "openclaw", workerAgentDir},
		{"unknown runtime falls back to default", "worker", "", workerAgentDir},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := d.builtinAgentDir(tc.role, tc.runtime); got != tc.want {
				t.Errorf("builtinAgentDir(%q, %q) = %q, want %q", tc.role, tc.runtime, got, tc.want)
			}
		})
	}
}
