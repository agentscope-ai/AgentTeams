package service

import (
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/config"
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
		"HICLAW_WORKER_NAME":         "alice",
		"HICLAW_FS_ACCESS_KEY":       "alice",
		"HICLAW_FS_SECRET_KEY":       "secret",
		"HICLAW_FS_ENDPOINT":         "http://fs.example.com:9000",
		"HICLAW_FS_BUCKET":           "hiclaw-fs",
		"HICLAW_STORAGE_PREFIX":      "teams/demo",
		"HICLAW_CONTROLLER_URL":      "http://controller.example.com:8090",
		"HICLAW_AI_GATEWAY_URL":      "http://aigw.example.com:8080",
		"HICLAW_MATRIX_URL":          "http://matrix.example.com:8080",
		"HICLAW_MATRIX_DOMAIN":       "matrix.example.com",
		"OPENCLAW_DISABLE_BONJOUR":   "1",
		"OPENCLAW_MDNS_HOSTNAME":     "hiclaw-w-alice",
		"HOME":                       "/root/hiclaw-fs/agents/alice",
		"HICLAW_WORKER_GATEWAY_KEY":  "gateway-key",
		"HICLAW_WORKER_MATRIX_TOKEN": "matrix-token",
		"HICLAW_WORKER_ROOM_ID":      "!room123:matrix.example.com",
		"SKILLS_API_URL":             "nacos://skills.example.com:8848/public",
		"NACOS_AUTH_TYPE":            "sts-hiclaw",
	} {
		if got := env[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	for _, legacyKey := range []string{"HICLAW_MINIO_ENDPOINT", "HICLAW_MINIO_BUCKET", "HICLAW_OSS_BUCKET"} {
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
		"HICLAW_MANAGER_NAME":           "manager",
		"HICLAW_MANAGER_GATEWAY_KEY":    "gateway-key",
		"HICLAW_MANAGER_PASSWORD":       "matrix-password",
		"HICLAW_FS_ACCESS_KEY":          "manager",
		"HICLAW_FS_SECRET_KEY":          "secret",
		"HICLAW_FS_BUCKET":              "hiclaw-fs",
		"HICLAW_RUNTIME":                "docker",
		"HICLAW_DEFAULT_WORKER_RUNTIME": "copaw",
		"HICLAW_ADMIN_USER":             "admin",
		"SKILLS_API_URL":                "nacos://skills.example.com:8848/public",
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
