package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/agentconfig"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func TestDeployWorkerConfigReconcilesStandaloneHumanAllowlist(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	if err := store.PutObject(ctx, "agents/worker/openclaw.json", []byte(`{"channels":{"matrix":{"groupAllowFrom":["@runtime-manual:local"],"dm":{"allowFrom":["@runtime-manual:local"]}}}}`)); err != nil {
		t.Fatal(err)
	}
	d := NewDeployer(DeployerConfig{
		AgentConfig: agentconfig.NewGenerator(agentconfig.Config{MatrixDomain: "local"}),
		OSS:         store, AgentFSDir: t.TempDir(),
	})
	req := WorkerDeployRequest{Name: "worker", ReconcileChannelPolicy: true, HumanMatrixIDs: []string{"@alice:external"}}
	policy := &v1beta1.ChannelPolicySpec{
		GroupAllowExtra: []string{"@alice:external", "@manual:local"},
		DmAllowExtra:    []string{"@alice:external", "@manual:local"},
	}
	req.Spec.ChannelPolicy = policy
	check := func(wantAlice bool) {
		t.Helper()
		if err := d.DeployWorkerConfig(ctx, req); err != nil {
			t.Fatal(err)
		}
		data, err := store.GetObject(ctx, "agents/worker/openclaw.json")
		if err != nil {
			t.Fatal(err)
		}
		var config map[string]any
		if err := json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		matrix := config["channels"].(map[string]any)["matrix"].(map[string]any)
		for _, raw := range []any{matrix["groupAllowFrom"], matrix["dm"].(map[string]any)["allowFrom"]} {
			ids := raw.([]any)
			alice, manual, runtimeManual := false, false, false
			for _, id := range ids {
				alice = alice || id == "@alice:external"
				manual = manual || id == "@manual:local"
				runtimeManual = runtimeManual || id == "@runtime-manual:local"
			}
			if alice != wantAlice || !manual || !runtimeManual {
				t.Fatalf("allowlist=%v, want Alice=%v and manual preserved", ids, wantAlice)
			}
		}
	}
	check(true)
	req.HumanMatrixIDs = nil
	policy.GroupAllowExtra = []string{"@manual:local"}
	policy.DmAllowExtra = []string{"@manual:local"}
	check(false)
	req.HumanMatrixIDs = []string{"@alice:external"}
	policy.GroupAllowExtra = append(policy.GroupAllowExtra, "@alice:external")
	policy.DmAllowExtra = append(policy.DmAllowExtra, "@alice:external")
	policy.GroupDenyExtra = []string{"@alice:external"}
	policy.DmDenyExtra = []string{"@alice:external"}
	check(false)
}

type humanAccessWriteFailure struct {
	*ossfake.Memory
	failConfig   bool
	configWrites int
}

func (s *humanAccessWriteFailure) PutObject(ctx context.Context, key string, data []byte) error {
	if strings.HasSuffix(key, "/openclaw.json") {
		if s.failConfig {
			return errors.New("config write failed")
		}
		s.configWrites++
	}
	return s.Memory.PutObject(ctx, key, data)
}

func TestHumanAllowlistOwnershipSurvivesFailedConfigWrites(t *testing.T) {
	ctx := context.Background()
	store := &humanAccessWriteFailure{Memory: ossfake.NewMemory(), failConfig: true}
	d := NewDeployer(DeployerConfig{AgentConfig: agentconfig.NewGenerator(agentconfig.Config{MatrixDomain: "local"}), OSS: store, AgentFSDir: t.TempDir()})
	req := WorkerDeployRequest{Name: "worker", ReconcileChannelPolicy: true, HumanMatrixIDs: []string{"@alice:external"}}
	req.Spec.ChannelPolicy = &v1beta1.ChannelPolicySpec{GroupAllowExtra: req.HumanMatrixIDs, DmAllowExtra: req.HumanMatrixIDs}
	if err := d.DeployWorkerConfig(ctx, req); err == nil {
		t.Fatal("expected write failure")
	}
	store.failConfig = false
	if err := d.DeployWorkerConfig(ctx, req); err != nil {
		t.Fatal(err)
	}
	writes := store.configWrites
	if err := d.DeployWorkerConfig(ctx, req); err != nil {
		t.Fatal(err)
	}
	if store.configWrites != writes {
		t.Fatal("unchanged reconciliation rewrote config")
	}
	// Retry a failed revoke with a new deployer, as after a controller restart.
	req.HumanMatrixIDs = nil
	req.Spec.ChannelPolicy = nil
	store.failConfig = true
	if err := d.DeployWorkerConfig(ctx, req); err == nil {
		t.Fatal("expected revoke write failure")
	}
	store.failConfig = false
	d = NewDeployer(DeployerConfig{AgentConfig: agentconfig.NewGenerator(agentconfig.Config{MatrixDomain: "local"}), OSS: store, AgentFSDir: t.TempDir()})
	if err := d.DeployWorkerConfig(ctx, req); err != nil {
		t.Fatal(err)
	}
	data, err := store.GetObject(ctx, "agents/worker/openclaw.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "@alice:external") {
		t.Fatal("revoked identity survived failed write and restart")
	}
}

func TestHumanAllowlistPreservesPreexistingGrantForSameIdentity(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	if err := store.PutObject(ctx, "agents/worker/openclaw.json", []byte(`{"channels":{"matrix":{"groupAllowFrom":["@alice:external"],"dm":{"allowFrom":["@alice:external"]}}}}`)); err != nil {
		t.Fatal(err)
	}
	d := NewDeployer(DeployerConfig{AgentConfig: agentconfig.NewGenerator(agentconfig.Config{MatrixDomain: "local"}), OSS: store, AgentFSDir: t.TempDir()})
	req := WorkerDeployRequest{Name: "worker", ReconcileChannelPolicy: true, HumanMatrixIDs: []string{"@alice:external"}}
	req.Spec.ChannelPolicy = &v1beta1.ChannelPolicySpec{GroupAllowExtra: req.HumanMatrixIDs, DmAllowExtra: req.HumanMatrixIDs}
	if err := d.DeployWorkerConfig(ctx, req); err != nil {
		t.Fatal(err)
	}
	req.HumanMatrixIDs = nil
	req.Spec.ChannelPolicy = nil
	if err := d.DeployWorkerConfig(ctx, req); err != nil {
		t.Fatal(err)
	}
	data, err := store.GetObject(ctx, "agents/worker/openclaw.json")
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	for _, dm := range []bool{true, false} {
		found := false
		for _, id := range channelIDs(matrixChannel(config), dm) {
			found = found || id == "@alice:external"
		}
		if !found {
			t.Fatal("removed independent grant for the same identity")
		}
	}
}
