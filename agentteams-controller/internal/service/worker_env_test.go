package service

import (
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/config"
)

func TestWorkerEnvBuilderBuildIncludesFinalRuntimeEnv(t *testing.T) {
	builder := NewWorkerEnvBuilder(config.WorkerEnvDefaults{
		MatrixDomain:    "matrix.example.com",
		FSEndpoint:      "http://fs.example.com:9000",
		FSBucket:        "agentteams-fs",
		StoragePrefix:   "teams/demo",
		StorageProvider: "minio",
		ControllerURL:   "http://controller.example.com:8090",
		AIGatewayURL:    "http://aigw.example.com:8080",
		MatrixURL:       "http://matrix.example.com:8080",
		Runtime:         "docker",
		SkillsAPIURL:    "nacos://skills.example.com:8848/public",
		NacosAuthType:   "sts-agentteams",
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
		"AGENTTEAMS_STORAGE_PROVIDER":    "minio",
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

func TestWorkerEnvBuilderBuildManagerUsesConfiguredRuntimeAndBucket(t *testing.T) {
	builder := NewWorkerEnvBuilder(config.WorkerEnvDefaults{
		MatrixDomain:         "matrix.example.com",
		FSEndpoint:           "http://fs.example.com:9000",
		FSBucket:             "agentteams-fs",
		StoragePrefix:        "teams/demo",
		StorageProvider:      "minio",
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
		"AGENTTEAMS_STORAGE_PROVIDER":       "minio",
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

func TestMergeUserEnv_SystemWins(t *testing.T) {
	sysEnv := map[string]string{WorkerConsolePortEnv: WorkerConsolePortDefault, "SYS_ONLY": "1"}
	userEnv := map[string]string{
		WorkerConsolePortEnv: "9090", // conflicts → discarded
		"USER_ONLY":          "42",   // new key → merged
	}
	ignored := MergeUserEnv(sysEnv, userEnv)
	if got := sysEnv[WorkerConsolePortEnv]; got != WorkerConsolePortDefault {
		t.Fatalf("system key changed to %q, must stay %q", got, WorkerConsolePortDefault)
	}
	if got := sysEnv["USER_ONLY"]; got != "42" {
		t.Fatalf("user key not merged, got %q", got)
	}
	if len(ignored) != 1 || ignored[0] != WorkerConsolePortEnv {
		t.Fatalf("ignored=%v, want [%s]", ignored, WorkerConsolePortEnv)
	}
}

func TestMergeUserEnv_NilUserEnv(t *testing.T) {
	sysEnv := map[string]string{WorkerConsolePortEnv: WorkerConsolePortDefault}
	if ignored := MergeUserEnv(sysEnv, nil); len(ignored) != 0 {
		t.Fatalf("ignored=%v, want none", ignored)
	}
	if got := sysEnv[WorkerConsolePortEnv]; got != WorkerConsolePortDefault {
		t.Fatalf("sysEnv mutated to %q", got)
	}
}

func TestEffectiveWorkerConsolePort_ConflictingUserValueDiscarded(t *testing.T) {
	cases := map[string]string{
		"9090":       "8088",
		" 7000 ":     "8088",
		"not-a-port": "8088",
		"99999":      "8088",
		"0":          "8088",
	}
	for userVal, want := range cases {
		if got := EffectiveWorkerConsolePort(map[string]string{WorkerConsolePortEnv: userVal}); got != want {
			t.Errorf("user value %q → %q, want %q (system wins)", userVal, got, want)
		}
	}
	if got := EffectiveWorkerConsolePort(nil); got != WorkerConsolePortDefault {
		t.Fatalf("nil user env → %q, want %q", got, WorkerConsolePortDefault)
	}
}

func TestWorkerEnvBuilderBuild_ConsolePortIsSharedConstant(t *testing.T) {
	builder := NewWorkerEnvBuilder(config.WorkerEnvDefaults{})
	env := builder.Build("w1", &WorkerProvisionResult{})
	if env[WorkerConsolePortEnv] != WorkerConsolePortDefault {
		t.Fatalf("builder port %q != shared constant %q — proxy would diverge", env[WorkerConsolePortEnv], WorkerConsolePortDefault)
	}
}
