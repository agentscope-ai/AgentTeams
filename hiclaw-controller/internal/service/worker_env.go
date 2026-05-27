package service

import (
	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/config"
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
		"HICLAW_WORKER_NAME":         workerName,
		"HICLAW_WORKER_GATEWAY_KEY":  prov.GatewayKey,
		"HICLAW_WORKER_MATRIX_TOKEN": prov.MatrixToken,
		"HICLAW_WORKER_ROOM_ID":      prov.RoomID,
		"HICLAW_FS_ACCESS_KEY":       workerName,
		"HICLAW_FS_SECRET_KEY":       prov.MinIOPassword,
		"OPENCLAW_DISABLE_BONJOUR":   "1",
		"OPENCLAW_MDNS_HOSTNAME":     "hiclaw-w-" + workerName,
		"HICLAW_CONSOLE_PORT":        "8088",
		"HOME":                       "/root/hiclaw-fs/agents/" + workerName,
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
		"HICLAW_MANAGER_NAME":        managerName,
		"HICLAW_MANAGER_GATEWAY_KEY": prov.GatewayKey,
		"HICLAW_MANAGER_PASSWORD":    prov.MatrixPassword,
		"HICLAW_FS_ACCESS_KEY":       managerName,
		"HICLAW_FS_SECRET_KEY":       prov.MinIOPassword,
		"OPENCLAW_DISABLE_BONJOUR":   "1",
		"OPENCLAW_MDNS_HOSTNAME":     "hiclaw-manager",
		"HOME":                       "/root/manager-workspace",
		"HICLAW_RUNTIME":             runtime,
	}

	if spec.Model != "" {
		env["HICLAW_DEFAULT_MODEL"] = spec.Model
	}
	if spec.Runtime != "" {
		env["HICLAW_MANAGER_RUNTIME"] = spec.Runtime
	}
	if b.defaults.AdminUser != "" {
		env["HICLAW_ADMIN_USER"] = b.defaults.AdminUser
	}

	cfg := spec.Config
	if cfg.HeartbeatInterval != "" {
		env["HICLAW_MANAGER_HEARTBEAT_INTERVAL"] = cfg.HeartbeatInterval
	}
	if cfg.WorkerIdleTimeout != "" {
		env["HICLAW_MANAGER_WORKER_IDLE_TIMEOUT"] = cfg.WorkerIdleTimeout
	}
	if cfg.NotifyChannel != "" {
		env["HICLAW_MANAGER_NOTIFY_CHANNEL"] = cfg.NotifyChannel
	}

	b.applyClusterDefaults(env)
	return env
}

func (b *WorkerEnvBuilder) applyClusterDefaults(env map[string]string) {
	for k, v := range map[string]string{
		"HICLAW_MATRIX_DOMAIN":  b.defaults.MatrixDomain,
		"HICLAW_FS_ENDPOINT":    b.defaults.FSEndpoint,
		"HICLAW_FS_BUCKET":      b.defaults.FSBucket,
		"HICLAW_STORAGE_PREFIX": b.defaults.StoragePrefix,
		"HICLAW_CONTROLLER_URL": b.defaults.ControllerURL,
		"HICLAW_AI_GATEWAY_URL": b.defaults.AIGatewayURL,
		"HICLAW_MATRIX_URL":     b.defaults.MatrixURL,
	} {
		if v != "" {
			env[k] = v
		}
	}

	// YOLO mode: when the controller was started with HICLAW_YOLO=1, propagate
	// it to every manager and worker container it provisions so the agent's
	// auto-confirm path triggers reliably (otherwise an agent without this
	// signal will block on confirmation prompts during integration tests).
	if b.defaults.YoloMode {
		env["HICLAW_YOLO"] = "1"
	}

	// Matrix-plugin trace logging: when the controller was started with
	// HICLAW_MATRIX_DEBUG=1, propagate it to every manager + worker container.
	// The container entrypoints translate it to OPENCLAW_MATRIX_DEBUG=1, which
	// makes openclaw's matrix plugin emit structured INFO-level traces (sync
	// state transitions, room.invite/join, message handler arrival + filter
	// outcomes). Used to debug "worker never joined" / "manager never replied"
	// hangs without rebuilding images.
	if b.defaults.MatrixDebug {
		env["HICLAW_MATRIX_DEBUG"] = "1"
	}

	// CMS observability configuration
	if b.defaults.CMSTracesEnabled {
		env["HICLAW_CMS_TRACES_ENABLED"] = "true"
	}
	if b.defaults.CMSMetricsEnabled {
		env["HICLAW_CMS_METRICS_ENABLED"] = "true"
	}
	if b.defaults.CMSEndpoint != "" {
		env["HICLAW_CMS_ENDPOINT"] = b.defaults.CMSEndpoint
	}
	if b.defaults.CMSLicenseKey != "" {
		env["HICLAW_CMS_LICENSE_KEY"] = b.defaults.CMSLicenseKey
	}
	if b.defaults.CMSProject != "" {
		env["HICLAW_CMS_PROJECT"] = b.defaults.CMSProject
	}
	if b.defaults.CMSWorkspace != "" {
		env["HICLAW_CMS_WORKSPACE"] = b.defaults.CMSWorkspace
	}
	if b.defaults.SkillsAPIURL != "" {
		env["SKILLS_API_URL"] = b.defaults.SkillsAPIURL
	}
	if b.defaults.NacosAuthType != "" {
		env["NACOS_AUTH_TYPE"] = b.defaults.NacosAuthType
	}
}
