package service

import (
	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/config"
)

// WorkerEnvBuilder constructs environment variable maps for worker containers.
// Configuration defaults are injected at construction time rather than read
// from os.Getenv at call time, keeping the service layer test-friendly.
type WorkerEnvBuilder struct {
	defaults config.WorkerEnvDefaults
}

func NewWorkerEnvBuilder(defaults config.WorkerEnvDefaults) *WorkerEnvBuilder {
	return &WorkerEnvBuilder{defaults: defaults}
}

// Build returns the env map for a worker container, merging per-worker
// credentials with cluster-wide defaults.
func (b *WorkerEnvBuilder) Build(workerName string, prov *WorkerProvisionResult) map[string]string {
	env := map[string]string{
		"AGENTTEAMS_WORKER_NAME":         workerName,
		"AGENTTEAMS_WORKER_GATEWAY_KEY":  prov.GatewayKey,
		"AGENTTEAMS_WORKER_MATRIX_TOKEN": prov.MatrixToken,
		"AGENTTEAMS_WORKER_ROOM_ID":      prov.RoomID,
		"AGENTTEAMS_FS_ACCESS_KEY":       workerName,
		"AGENTTEAMS_FS_SECRET_KEY":       prov.MinIOPassword,
		"OPENCLAW_DISABLE_BONJOUR":       "1",
		"OPENCLAW_MDNS_HOSTNAME":         "hiclaw-w-" + workerName,
		"AGENTTEAMS_CONSOLE_PORT":        "8088",
		"HOME":                           "/root/agentteams-fs/agents/" + workerName,
	}

	b.applyClusterDefaults(env)
	return env
}

// BuildManager returns the env map for a Manager container.
func (b *WorkerEnvBuilder) BuildManager(managerName string, prov *ManagerProvisionResult, spec v1beta1.ManagerSpec) map[string]string {
	runtime := b.defaults.Runtime
	if runtime == "" {
		runtime = "k8s"
	}

	env := map[string]string{
		"AGENTTEAMS_MANAGER_NAME":        managerName,
		"AGENTTEAMS_MANAGER_GATEWAY_KEY": prov.GatewayKey,
		// In AS mode MatrixPassword is empty; set a placeholder so the
		// entrypoint's :? validation passes. The password is never used
		// for login in AS mode (token obtained via AS login instead).
		"AGENTTEAMS_MANAGER_PASSWORD": valueOrPlaceholder(prov.MatrixPassword),
		// Pre-inject the Matrix access token so the Manager entrypoint can
		// skip password-based login. Required in AppService mode (no password)
		// and beneficial in legacy mode (avoids a redundant login round-trip).
		"AGENTTEAMS_MANAGER_MATRIX_TOKEN": prov.MatrixToken,
		"AGENTTEAMS_FS_ACCESS_KEY":        managerName,
		"AGENTTEAMS_FS_SECRET_KEY":        prov.MinIOPassword,
		"OPENCLAW_DISABLE_BONJOUR":        "1",
		"OPENCLAW_MDNS_HOSTNAME":          "agentteams-manager",
		"HOME":                            "/root/manager-workspace",
		"AGENTTEAMS_RUNTIME":              runtime,
	}

	if spec.Model != "" {
		env["AGENTTEAMS_DEFAULT_MODEL"] = spec.Model
	}
	if spec.Runtime != "" {
		env["AGENTTEAMS_MANAGER_RUNTIME"] = spec.Runtime
	}
	if b.defaults.AdminUser != "" {
		env["AGENTTEAMS_ADMIN_USER"] = b.defaults.AdminUser
	}
	if b.defaults.DefaultWorkerRuntime != "" {
		env["AGENTTEAMS_DEFAULT_WORKER_RUNTIME"] = b.defaults.DefaultWorkerRuntime
	}

	cfg := spec.Config
	if cfg.HeartbeatInterval != "" {
		env["AGENTTEAMS_MANAGER_HEARTBEAT_INTERVAL"] = cfg.HeartbeatInterval
	}
	if cfg.WorkerIdleTimeout != "" {
		env["AGENTTEAMS_MANAGER_WORKER_IDLE_TIMEOUT"] = cfg.WorkerIdleTimeout
	}
	if cfg.NotifyChannel != "" {
		env["AGENTTEAMS_MANAGER_NOTIFY_CHANNEL"] = cfg.NotifyChannel
	}

	b.applyClusterDefaults(env)
	return env
}

func (b *WorkerEnvBuilder) applyClusterDefaults(env map[string]string) {
	for k, v := range map[string]string{
		"AGENTTEAMS_MATRIX_DOMAIN":  b.defaults.MatrixDomain,
		"AGENTTEAMS_FS_ENDPOINT":    b.defaults.FSEndpoint,
		"AGENTTEAMS_FS_BUCKET":      b.defaults.FSBucket,
		"AGENTTEAMS_STORAGE_PREFIX": b.defaults.StoragePrefix,
		"AGENTTEAMS_CONTROLLER_URL": b.defaults.ControllerURL,
		"AGENTTEAMS_AI_GATEWAY_URL": b.defaults.AIGatewayURL,
		"AGENTTEAMS_MATRIX_URL":     b.defaults.MatrixURL,
	} {
		if v != "" {
			env[k] = v
		}
	}

	// YOLO mode: when the controller was started with AGENTTEAMS_YOLO=1, propagate
	// it to every manager and worker container it provisions so the agent's
	// auto-confirm path triggers reliably (otherwise an agent without this
	// signal will block on confirmation prompts during integration tests).
	if b.defaults.YoloMode {
		env["AGENTTEAMS_YOLO"] = "1"
	}

	// Matrix-plugin trace logging: when the controller was started with
	// AGENTTEAMS_MATRIX_DEBUG=1, propagate it to every manager + worker container.
	// The container entrypoints translate it to OPENCLAW_MATRIX_DEBUG=1, which
	// makes openclaw's matrix plugin emit structured INFO-level traces (sync
	// state transitions, room.invite/join, message handler arrival + filter
	// outcomes). Used to debug "worker never joined" / "manager never replied"
	// hangs without rebuilding images.
	if b.defaults.MatrixDebug {
		env["AGENTTEAMS_MATRIX_DEBUG"] = "1"
	}

	// CMS observability configuration
	if b.defaults.CMSTracesEnabled {
		env["AGENTTEAMS_CMS_TRACES_ENABLED"] = "true"
	}
	if b.defaults.CMSMetricsEnabled {
		env["AGENTTEAMS_CMS_METRICS_ENABLED"] = "true"
	}
	if b.defaults.CMSEndpoint != "" {
		env["AGENTTEAMS_CMS_ENDPOINT"] = b.defaults.CMSEndpoint
	}
	if b.defaults.CMSLicenseKey != "" {
		env["AGENTTEAMS_CMS_LICENSE_KEY"] = b.defaults.CMSLicenseKey
	}
	if b.defaults.CMSProject != "" {
		env["AGENTTEAMS_CMS_PROJECT"] = b.defaults.CMSProject
	}
	if b.defaults.CMSWorkspace != "" {
		env["AGENTTEAMS_CMS_WORKSPACE"] = b.defaults.CMSWorkspace
	}
	if b.defaults.SkillsAPIURL != "" {
		env["SKILLS_API_URL"] = b.defaults.SkillsAPIURL
	}
	if b.defaults.NacosAuthType != "" {
		env["NACOS_AUTH_TYPE"] = b.defaults.NacosAuthType
	}
}

// valueOrPlaceholder returns v if non-empty, otherwise a harmless placeholder.
// Used for env vars that must be present but are unused in certain modes.
func valueOrPlaceholder(v string) string {
	if v != "" {
		return v
	}
	return "as-mode-not-used"
}
