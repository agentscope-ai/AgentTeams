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
		FSBucket:      "hiclaw-fs",
		StoragePrefix: "teams/demo",
		ControllerURL: "http://controller.example.com:8090",
		AIGatewayURL:  "http://aigw.example.com:8080",
		MatrixURL:     "http://matrix.example.com:8080",
		Runtime:       "docker",
		SkillsAPIURL:  "nacos://skills.example.com:8848/public",
		NacosAuthType: "sts-hiclaw",
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
		"AGENTTEAMS_FS_BUCKET":           "hiclaw-fs",
		"AGENTTEAMS_STORAGE_PREFIX":      "teams/demo",
		"AGENTTEAMS_CONTROLLER_URL":      "http://controller.example.com:8090",
		"AGENTTEAMS_AI_GATEWAY_URL":      "http://aigw.example.com:8080",
		"AGENTTEAMS_MATRIX_URL":          "http://matrix.example.com:8080",
		"AGENTTEAMS_MATRIX_DOMAIN":       "matrix.example.com",
		"OPENCLAW_DISABLE_BONJOUR":       "1",
		"OPENCLAW_MDNS_HOSTNAME":         "hiclaw-w-alice",
		"HOME":                           "/root/agentteams-fs/agents/alice",
		"AGENTTEAMS_WORKER_GATEWAY_KEY":  "gateway-key",
		"AGENTTEAMS_WORKER_MATRIX_TOKEN": "matrix-token",
		"AGENTTEAMS_WORKER_ROOM_ID":      "!room123:matrix.example.com",
		"SKILLS_API_URL":                 "nacos://skills.example.com:8848/public",
		"NACOS_AUTH_TYPE":                "sts-hiclaw",
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

func TestWorkerEnvBuilderBuildManagerUsesConfiguredRuntimeAndBucket(t *testing.T) {
	builder := NewWorkerEnvBuilder(config.WorkerEnvDefaults{
		MatrixDomain:         "matrix.example.com",
		FSEndpoint:           "http://fs.example.com:9000",
		FSBucket:             "hiclaw-fs",
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
		"AGENTTEAMS_FS_BUCKET":              "hiclaw-fs",
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
