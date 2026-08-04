package service

import (
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/config"
)

func TestWorkerEnvBuilderBuildIncludesFinalRuntimeEnv(t *testing.T) {
	builder := NewWorkerEnvBuilder(config.WorkerEnvDefaults{
		MatrixDomain:  "matrix.example.com",
		FSEndpoint:    "http://fs.example.com:9000",
		FSBucket:      "agentteams-fs",
		StoragePrefix: "teams/demo",
		ControllerURL: "http://controller.example.com:8090",
		AIGatewayURL:  "http://aigw.example.com:8080",
		MatrixURL:     "http://matrix.example.com:8080",
		Runtime:       "docker",
		SkillsAPIURL:  "nacos://skills.example.com:8848/public",
		NacosAuthType: "sts-agentteams",
	})

	env := builder.Build("alice", &WorkerProvisionResult{
		GatewayKey:    "gateway-key",
		MatrixToken:   "matrix-token",
		RoomID:        "!room123:matrix.example.com",
		MinIOPassword: "secret",
	})

	for key, want := range map[string]string{
		"AGENTTEAMS_WORKER_NAME":         "alice",
		"AGENTTEAMS_FS_ACCESS_KEY":       "alice",
		"AGENTTEAMS_FS_SECRET_KEY":       "secret",
		"AGENTTEAMS_FS_ENDPOINT":         "http://fs.example.com:9000",
		"AGENTTEAMS_FS_BUCKET":           "agentteams-fs",
		"AGENTTEAMS_STORAGE_PREFIX":      "teams/demo",
		"AGENTTEAMS_CONTROLLER_URL":      "http://controller.example.com:8090",
		"AGENTTEAMS_AI_GATEWAY_URL":      "http://aigw.example.com:8080",
		"AGENTTEAMS_MATRIX_URL":          "http://matrix.example.com:8080",
		"AGENTTEAMS_MATRIX_DOMAIN":       "matrix.example.com",
		"OPENCLAW_DISABLE_BONJOUR":       "1",
		"OPENCLAW_MDNS_HOSTNAME":         "agentteams-w-alice",
		"HOME":                           "/root/agentteams-fs/agents/alice",
		"AGENTTEAMS_WORKER_GATEWAY_KEY":  "gateway-key",
		"AGENTTEAMS_WORKER_MATRIX_TOKEN": "matrix-token",
		"AGENTTEAMS_WORKER_ROOM_ID":      "!room123:matrix.example.com",
		"SKILLS_API_URL":                 "nacos://skills.example.com:8848/public",
		"NACOS_AUTH_TYPE":                "sts-agentteams",
	} {
		if got := env[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	for _, legacyKey := range []string{"AGENTTEAMS_MINIO_ENDPOINT", "AGENTTEAMS_MINIO_BUCKET"} {
		if _, ok := env[legacyKey]; ok {
			t.Fatalf("unexpected legacy env %s in worker env", legacyKey)
		}
	}
}

func TestWorkerEnvBuilderBuildForRuntimeIsolatesDeepAgentsSecrets(t *testing.T) {
	builder := NewWorkerEnvBuilder(config.WorkerEnvDefaults{
		CheckpointDSN:     "postgresql://checkpoint.example/agentteams",
		CheckpointAESKey:  "0123456789abcdef0123456789abcdef",
		DeepAgentsEnabled: true,
	})
	prov := &WorkerProvisionResult{}

	deepagentsEnv := builder.BuildForRuntime("alice", "deepagents", prov)
	for key, want := range map[string]string{
		"AGENTTEAMS_CHECKPOINT_DSN":     "postgresql://checkpoint.example/agentteams",
		"AGENTTEAMS_CHECKPOINT_AES_KEY": "0123456789abcdef0123456789abcdef",
		"HOME":                          "/var/lib/agentteams",
	} {
		if got := deepagentsEnv[key]; got != want {
			t.Fatalf("deepagents %s = %q, want %q", key, got, want)
		}
	}

	for _, runtime := range []string{"", "openclaw", "copaw", "hermes", "openhuman"} {
		env := builder.BuildForRuntime("alice", runtime, prov)
		for _, key := range []string{"AGENTTEAMS_CHECKPOINT_DSN", "AGENTTEAMS_CHECKPOINT_AES_KEY"} {
			if _, ok := env[key]; ok {
				t.Fatalf("runtime %q unexpectedly received %s", runtime, key)
			}
		}
	}
}

func TestWorkerEnvBuilderValidateRuntimeRequiresEnabledCheckpoint(t *testing.T) {
	for name, defaults := range map[string]config.WorkerEnvDefaults{
		"disabled": {},
		"missing dsn": {
			DeepAgentsEnabled: true,
			CheckpointAESKey:  "0123456789abcdef0123456789abcdef",
		},
		"missing aes key": {
			DeepAgentsEnabled: true,
			CheckpointDSN:     "postgresql://checkpoint.example/agentteams",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := NewWorkerEnvBuilder(defaults).ValidateRuntime("deepagents"); err == nil {
				t.Fatal("expected DeepAgents runtime validation error")
			}
		})
	}
	valid := NewWorkerEnvBuilder(config.WorkerEnvDefaults{
		DeepAgentsEnabled: true,
		CheckpointDSN:     "postgresql://checkpoint.example/agentteams",
		CheckpointAESKey:  "0123456789abcdef0123456789abcdef",
	})
	if err := valid.ValidateRuntime("deepagents"); err != nil {
		t.Fatalf("valid DeepAgents config: %v", err)
	}
	if err := valid.ValidateRuntime("openclaw"); err != nil {
		t.Fatalf("non-DeepAgents runtime: %v", err)
	}
}

func TestWorkerEnvBuilderBuildManagerUsesConfiguredRuntimeAndBucket(t *testing.T) {
	builder := NewWorkerEnvBuilder(config.WorkerEnvDefaults{
		MatrixDomain:         "matrix.example.com",
		FSEndpoint:           "http://fs.example.com:9000",
		FSBucket:             "agentteams-fs",
		StoragePrefix:        "teams/demo",
		ControllerURL:        "http://controller.example.com:8090",
		AIGatewayURL:         "http://aigw.example.com:8080",
		MatrixURL:            "http://matrix.example.com:8080",
		AdminUser:            "admin",
		Runtime:              "docker",
		DefaultWorkerRuntime: "copaw",
		SkillsAPIURL:         "nacos://skills.example.com:8848/public",
	})

	env := builder.BuildManager("manager", &ManagerProvisionResult{
		GatewayKey:     "gateway-key",
		MatrixPassword: "matrix-password",
		MinIOPassword:  "secret",
	}, v1beta1.ManagerSpec{})

	for key, want := range map[string]string{
		"AGENTTEAMS_MANAGER_NAME":           "manager",
		"AGENTTEAMS_MANAGER_GATEWAY_KEY":    "gateway-key",
		"AGENTTEAMS_MANAGER_PASSWORD":       "matrix-password",
		"AGENTTEAMS_FS_ACCESS_KEY":          "manager",
		"AGENTTEAMS_FS_SECRET_KEY":          "secret",
		"AGENTTEAMS_FS_BUCKET":              "agentteams-fs",
		"AGENTTEAMS_RUNTIME":                "docker",
		"AGENTTEAMS_DEFAULT_WORKER_RUNTIME": "copaw",
		"AGENTTEAMS_ADMIN_USER":             "admin",
		"SKILLS_API_URL":                    "nacos://skills.example.com:8848/public",
	} {
		if got := env[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	for _, legacyKey := range []string{"AGENTTEAMS_MINIO_ACCESS_KEY", "AGENTTEAMS_MINIO_SECRET_KEY", "AGENTTEAMS_MINIO_BUCKET"} {
		if _, ok := env[legacyKey]; ok {
			t.Fatalf("unexpected legacy env %s in manager env", legacyKey)
		}
	}
}
