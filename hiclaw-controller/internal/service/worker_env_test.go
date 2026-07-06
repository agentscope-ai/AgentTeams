package service

import (
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/config"
)

func TestWorkerEnvBuilderBuildIncludesFinalRuntimeEnv(t *testing.T) {
	builder := NewWorkerEnvBuilder(config.WorkerEnvDefaults{
		MatrixDomain:    "matrix.example.com",
		FSEndpoint:      "http://fs.example.com:9000",
		FSBucket:        "agentteams-fs",
		StoragePrefix:   "teams/demo",
		StorageProvider: "oss",
		ControllerURL:   "http://controller.example.com:8090",
		AIGatewayURL:    "http://aigw.example.com:8080",
		MatrixURL:       "http://matrix.example.com:8080",
		Runtime:         "docker",
		SkillsAPIURL:    "nacos://skills.example.com:8848/public",
		NacosAuthType:   "sts-hiclaw",
	})

	env := builder.Build("alice", &WorkerProvisionResult{
		GatewayKey:    "gateway-key",
		MatrixToken:   "matrix-token",
		MinIOPassword: "secret",
	})

	for key, want := range map[string]string{
		"AGENTTEAMS_WORKER_NAME":         "alice",
		"AGENTTEAMS_FS_ACCESS_KEY":       "alice",
		"AGENTTEAMS_FS_SECRET_KEY":       "secret",
		"AGENTTEAMS_FS_ENDPOINT":         "http://fs.example.com:9000",
		"AGENTTEAMS_FS_BUCKET":           "agentteams-fs",
		"AGENTTEAMS_STORAGE_PREFIX":      "teams/demo",
		"AGENTTEAMS_STORAGE_PROVIDER":    "oss",
		"AGENTTEAMS_CONTROLLER_URL":      "http://controller.example.com:8090",
		"AGENTTEAMS_AI_GATEWAY_URL":      "http://aigw.example.com:8080",
		"AGENTTEAMS_MATRIX_URL":          "http://matrix.example.com:8080",
		"AGENTTEAMS_MATRIX_DOMAIN":       "matrix.example.com",
		"OPENCLAW_DISABLE_BONJOUR":       "1",
		"OPENCLAW_MDNS_HOSTNAME":         "hiclaw-w-alice",
		"HOME":                           "/root/hiclaw-fs/agents/alice",
		"AGENTTEAMS_WORKER_GATEWAY_KEY":  "gateway-key",
		"AGENTTEAMS_WORKER_MATRIX_TOKEN": "matrix-token",
		"SKILLS_API_URL":                 "nacos://skills.example.com:8848/public",
		"NACOS_AUTH_TYPE":                "sts-hiclaw",
	} {
		if got := env[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	for _, legacyKey := range []string{
		"HICLAW_WORKER_NAME",
		"HICLAW_WORKER_GATEWAY_KEY",
		"HICLAW_WORKER_MATRIX_TOKEN",
		"HICLAW_FS_ACCESS_KEY",
		"HICLAW_FS_SECRET_KEY",
		"HICLAW_FS_ENDPOINT",
		"HICLAW_FS_BUCKET",
		"HICLAW_STORAGE_PREFIX",
		"HICLAW_CONTROLLER_URL",
		"HICLAW_AI_GATEWAY_URL",
		"HICLAW_MATRIX_URL",
		"HICLAW_MATRIX_DOMAIN",
		"HICLAW_MINIO_ENDPOINT",
		"HICLAW_MINIO_BUCKET",
		"HICLAW_OSS_BUCKET",
	} {
		if _, ok := env[legacyKey]; ok {
			t.Fatalf("unexpected legacy env %s in worker env", legacyKey)
		}
	}
}

func TestWorkerEnvBuilderBuildManagerUsesConfiguredRuntimeAndBucket(t *testing.T) {
	builder := NewWorkerEnvBuilder(config.WorkerEnvDefaults{
		MatrixDomain:    "matrix.example.com",
		FSEndpoint:      "http://fs.example.com:9000",
		FSBucket:        "agentteams-fs",
		StoragePrefix:   "teams/demo",
		StorageProvider: "oss",
		ControllerURL:   "http://controller.example.com:8090",
		AIGatewayURL:    "http://aigw.example.com:8080",
		MatrixURL:       "http://matrix.example.com:8080",
		AdminUser:       "admin",
		Runtime:         "docker",
		SkillsAPIURL:    "nacos://skills.example.com:8848/public",
	})

	env := builder.BuildManager("manager", &ManagerProvisionResult{
		GatewayKey:     "gateway-key",
		MatrixPassword: "matrix-password",
		MinIOPassword:  "secret",
	}, v1beta1.ManagerSpec{})

	for key, want := range map[string]string{
		"AGENTTEAMS_MANAGER_NAME":        "manager",
		"AGENTTEAMS_MANAGER_GATEWAY_KEY": "gateway-key",
		"AGENTTEAMS_MANAGER_PASSWORD":    "matrix-password",
		"AGENTTEAMS_FS_ACCESS_KEY":       "manager",
		"AGENTTEAMS_FS_SECRET_KEY":       "secret",
		"AGENTTEAMS_FS_BUCKET":           "agentteams-fs",
		"AGENTTEAMS_STORAGE_PROVIDER":    "oss",
		"AGENTTEAMS_RUNTIME":             "docker",
		"AGENTTEAMS_ADMIN_USER":          "admin",
		"SKILLS_API_URL":                 "nacos://skills.example.com:8848/public",
	} {
		if got := env[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	for _, legacyKey := range []string{"HICLAW_MINIO_ACCESS_KEY", "HICLAW_MINIO_SECRET_KEY", "HICLAW_MINIO_BUCKET", "HICLAW_OSS_BUCKET"} {
		if _, ok := env[legacyKey]; ok {
			t.Fatalf("unexpected legacy env %s in manager env", legacyKey)
		}
	}
}
