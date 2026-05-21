package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/agentconfig"
	"github.com/hiclaw/hiclaw-controller/internal/oss/ossfake"
)

func TestDeployWorkerConfigSeedsLocalFilesWithoutOverwritingRuntimeState(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	agentFSDir := filepath.Join(tmp, "agents")
	workerDir := filepath.Join(agentFSDir, "alice")
	if err := os.MkdirAll(filepath.Join(workerDir, "config"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workerDir, "config", "credagent.json"), []byte(`{"source":"template"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workerDir, "notes.md"), []byte("template note"), 0644); err != nil {
		t.Fatal(err)
	}

	store := ossfake.NewMemory()
	if err := store.PutObject(ctx, "agents/alice/config/credagent.json", []byte(`{"source":"runtime"}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.PutObject(ctx, "agents/alice/openclaw.json", []byte(`{"old":true}`)); err != nil {
		t.Fatal(err)
	}

	deployer := NewDeployer(DeployerConfig{
		AgentConfig: agentconfig.NewGenerator(agentconfig.Config{}),
		OSS:         store,
		AgentFSDir:  agentFSDir,
	})
	err := deployer.DeployWorkerConfig(ctx, WorkerDeployRequest{
		Name:        "alice",
		MatrixToken: "matrix-token",
		GatewayKey:  "gateway-key",
	})
	if err != nil {
		t.Fatalf("DeployWorkerConfig failed: %v", err)
	}

	got, err := store.GetObject(ctx, "agents/alice/config/credagent.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"source":"runtime"}` {
		t.Fatalf("credagent.json overwritten: %s", got)
	}

	got, err = store.GetObject(ctx, "agents/alice/notes.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "template note" {
		t.Fatalf("notes.md not seeded: %s", got)
	}

	got, err = store.GetObject(ctx, "agents/alice/openclaw.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "gateway-key") {
		t.Fatalf("openclaw.json was not overwritten by controller config: %s", got)
	}
}

func TestDeployWorkerConfigInlineSoulOverridesPackageSeed(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	agentFSDir := filepath.Join(tmp, "agents")
	workerDir := filepath.Join(agentFSDir, "alice")
	if err := os.MkdirAll(workerDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workerDir, "SOUL.md"), []byte("OVERRIDDEN SOUL FROM INLINE\n"), 0644); err != nil {
		t.Fatal(err)
	}

	store := ossfake.NewMemory()
	if err := store.PutObject(ctx, "agents/alice/SOUL.md", []byte("ORIGINAL SOUL FROM PACKAGE\n")); err != nil {
		t.Fatal(err)
	}

	deployer := NewDeployer(DeployerConfig{
		AgentConfig: agentconfig.NewGenerator(agentconfig.Config{}),
		OSS:         store,
		AgentFSDir:  agentFSDir,
	})
	err := deployer.DeployWorkerConfig(ctx, WorkerDeployRequest{
		Name:        "alice",
		MatrixToken: "matrix-token",
		GatewayKey:  "gateway-key",
		IsUpdate:    true,
		Spec: v1beta1.WorkerSpec{
			Runtime: "hermes",
			Soul:    "OVERRIDDEN SOUL FROM INLINE",
		},
	})
	if err != nil {
		t.Fatalf("DeployWorkerConfig failed: %v", err)
	}

	got, err := store.GetObject(ctx, "agents/alice/SOUL.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "OVERRIDDEN SOUL FROM INLINE") {
		t.Fatalf("SOUL.md did not contain inline content: %s", got)
	}
	if strings.Contains(string(got), "ORIGINAL SOUL FROM PACKAGE") {
		t.Fatalf("SOUL.md still contains package seed content: %s", got)
	}
}
