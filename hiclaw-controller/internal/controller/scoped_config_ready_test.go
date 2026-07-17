package controller

import (
	"context"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/oss/ossfake"
)

func probeEnv() map[string]string {
	return map[string]string{
		"AGENTTEAMS_FS_ACCESS_KEY":  "ak",
		"AGENTTEAMS_FS_SECRET_KEY":  "sk",
		"AGENTTEAMS_FS_ENDPOINT":    "http://fs.example.com:9000",
		"AGENTTEAMS_FS_BUCKET":      "agentteams",
		"AGENTTEAMS_STORAGE_PREFIX": "teams/demo",
	}
}

// TestScopedWorkerConfigReadyShortCircuits covers the non-blocking probe's
// applicability guards (Issue 4). When the worker has no scoped credentials, or
// the storage endpoint/bucket/prefix are unset, there is nothing to probe and
// the function must report ready without contacting storage — otherwise the
// reconcile would requeue forever waiting for a config that will never be
// projected.
func TestScopedWorkerConfigReadyShortCircuits(t *testing.T) {
	// Force the endpoint env lookup to empty so the ambient CI environment
	// cannot make the endpoint-present branch attempt a network Stat.
	t.Setenv("AGENTTEAMS_FS_ENDPOINT", "")

	tests := []struct {
		name      string
		workerEnv map[string]string
	}{
		{
			name:      "no scoped credentials",
			workerEnv: map[string]string{},
		},
		{
			name: "credentials but no endpoint/bucket/prefix",
			workerEnv: map[string]string{
				"AGENTTEAMS_FS_ACCESS_KEY": "ak",
				"AGENTTEAMS_FS_SECRET_KEY": "sk",
			},
		},
		{
			name: "credentials and endpoint but no bucket",
			workerEnv: map[string]string{
				"AGENTTEAMS_FS_ACCESS_KEY":  "ak",
				"AGENTTEAMS_FS_SECRET_KEY":  "sk",
				"AGENTTEAMS_FS_ENDPOINT":    "http://fs.example.com:9000",
				"AGENTTEAMS_STORAGE_PREFIX": "teams/demo",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ready, err := scopedWorkerConfigReady(context.Background(), "alice", backend.RuntimeOpenClaw, "", tc.workerEnv)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !ready {
				t.Fatalf("expected ready=true (nothing to probe), got false")
			}
		})
	}
}

func TestScopedWorkerConfigObjectKey(t *testing.T) {
	tests := []struct {
		name        string
		runtimeName string
		runtime     string
		deployMode  string
		want        string
	}{
		{
			name:        "legacy openclaw",
			runtimeName: "alice",
			runtime:     backend.RuntimeOpenClaw,
			want:        "agents/alice/openclaw.json",
		},
		{
			name:        "legacy hermes",
			runtimeName: "bob",
			runtime:     backend.RuntimeHermes,
			want:        "agents/bob/openclaw.json",
		},
		{
			name:        "qwenpaw uses runtime.yaml",
			runtimeName: "qwen-worker",
			runtime:     backend.RuntimeQwenPaw,
			want:        "agents/qwen-worker/runtime/runtime.yaml",
		},
		{
			name:        "edge uses runtime.yaml even for openclaw",
			runtimeName: "edge-01",
			runtime:     backend.RuntimeOpenClaw,
			deployMode:  v1beta1.DeployModeEdge,
			want:        "agents/edge-01/runtime/runtime.yaml",
		},
		{
			name:        "runtimeName preferred over CR name shape",
			runtimeName: "runtime-id",
			runtime:     backend.RuntimeCopaw,
			want:        "agents/runtime-id/openclaw.json",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := scopedWorkerConfigObjectKey(tc.runtimeName, tc.runtime, tc.deployMode)
			if got != tc.want {
				t.Fatalf("scopedWorkerConfigObjectKey(%q, %q, %q) = %q, want %q",
					tc.runtimeName, tc.runtime, tc.deployMode, got, tc.want)
			}
		})
	}
}

func TestScopedWorkerConfigReadyMissingObject(t *testing.T) {
	t.Setenv("AGENTTEAMS_FS_ENDPOINT", "")
	store := ossfake.NewMemory()

	ready, err := scopedWorkerConfigReadyWithClient(
		context.Background(),
		"alice",
		backend.RuntimeOpenClaw,
		"",
		probeEnv(),
		store,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ready {
		t.Fatal("expected ready=false when object is missing")
	}
}

func TestScopedWorkerConfigReadyQwenPawPath(t *testing.T) {
	t.Setenv("AGENTTEAMS_FS_ENDPOINT", "")
	store := ossfake.NewMemory()

	// Wrong (legacy) key must not satisfy the probe.
	if err := store.PutObject(context.Background(), "agents/qwen-a/openclaw.json", []byte(`{}`)); err != nil {
		t.Fatalf("seed legacy key: %v", err)
	}
	ready, err := scopedWorkerConfigReadyWithClient(
		context.Background(),
		"qwen-a",
		backend.RuntimeQwenPaw,
		"",
		probeEnv(),
		store,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ready {
		t.Fatal("expected ready=false when only openclaw.json exists for qwenpaw")
	}

	if err := store.PutObject(context.Background(), "agents/qwen-a/runtime/runtime.yaml", []byte("apiVersion: v1\n")); err != nil {
		t.Fatalf("seed runtime.yaml: %v", err)
	}
	ready, err = scopedWorkerConfigReadyWithClient(
		context.Background(),
		"qwen-a",
		backend.RuntimeQwenPaw,
		"",
		probeEnv(),
		store,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ready {
		t.Fatal("expected ready=true when runtime.yaml is present for qwenpaw")
	}
}

func TestScopedWorkerConfigReadyLegacyPath(t *testing.T) {
	t.Setenv("AGENTTEAMS_FS_ENDPOINT", "")
	store := ossfake.NewMemory()

	if err := store.PutObject(context.Background(), "agents/alice/runtime/runtime.yaml", []byte("apiVersion: v1\n")); err != nil {
		t.Fatalf("seed runtime.yaml: %v", err)
	}
	ready, err := scopedWorkerConfigReadyWithClient(
		context.Background(),
		"alice",
		backend.RuntimeOpenClaw,
		"",
		probeEnv(),
		store,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ready {
		t.Fatal("expected ready=false when only runtime.yaml exists for legacy runtime")
	}

	if err := store.PutObject(context.Background(), "agents/alice/openclaw.json", []byte(`{}`)); err != nil {
		t.Fatalf("seed openclaw.json: %v", err)
	}
	ready, err = scopedWorkerConfigReadyWithClient(
		context.Background(),
		"alice",
		backend.RuntimeOpenClaw,
		"",
		probeEnv(),
		store,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ready {
		t.Fatal("expected ready=true when openclaw.json is present for legacy runtime")
	}
}

func TestScopedWorkerConfigReadyPrefersRuntimeName(t *testing.T) {
	t.Setenv("AGENTTEAMS_FS_ENDPOINT", "")
	store := ossfake.NewMemory()

	// Object lives under RuntimeName, not CR Name.
	if err := store.PutObject(context.Background(), "agents/runtime-id/openclaw.json", []byte(`{}`)); err != nil {
		t.Fatalf("seed runtime-name key: %v", err)
	}

	// Probing with CR Name must miss.
	ready, err := scopedWorkerConfigReadyWithClient(
		context.Background(),
		"cr-name",
		backend.RuntimeOpenClaw,
		"",
		probeEnv(),
		store,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ready {
		t.Fatal("expected ready=false when probing CR Name while object is under RuntimeName")
	}

	ready, err = scopedWorkerConfigReadyWithClient(
		context.Background(),
		"runtime-id",
		backend.RuntimeOpenClaw,
		"",
		probeEnv(),
		store,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ready {
		t.Fatal("expected ready=true when probing RuntimeName")
	}
}
