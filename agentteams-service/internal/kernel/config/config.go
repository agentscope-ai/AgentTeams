// Package config resolves agentteams-service configuration from the command
// line, the environment, and built-in defaults.
//
// Precedence (highest first): command-line flag > environment variable
// (AGENTTEAMS_SERVICE_*) > default. R1 intentionally does not support YAML
// file configuration (54 号 §3：独立 go.mod、独立进程).
package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// EnvPrefix is the environment variable prefix for all service settings.
const EnvPrefix = "AGENTTEAMS_SERVICE"

// Frozen values (58 号) — not operator-tunable. Consumed by auth (token
// review cache) and later slices; kept here as the single source.
const (
	ReviewCacheTTL    = 60 * time.Second // token-review 按 jti 缓存 60s
	IdempotencyTTL    = 24 * time.Hour   // idempotency_keys 保留 24h
	ReplayWindow      = 5 * time.Minute  // X-AT-Timestamp ±5min 重放窗
	LeaseTTL          = 30 * time.Second // kernel:upgrade 租约时长
	MaxOutboxAttempts = 5                // 5 次死信（54 号 §4.4）
	OutboxBackoffBase = 5 * time.Second  // 指数退避 5s*2^(n-1)
	OutboxBackoffCap  = 5 * time.Minute
)

// Environment variable names.
const (
	EnvListenAddr           = EnvPrefix + "_LISTEN_ADDR"
	EnvUDS                  = EnvPrefix + "_UDS"
	EnvEnabled              = EnvPrefix + "_ENABLED"
	EnvServiceID            = EnvPrefix + "_SERVICE_ID"
	EnvResDSN               = EnvPrefix + "_RES_DSN"
	EnvOpsDSN               = EnvPrefix + "_OPS_DSN"
	EnvControllerURL        = EnvPrefix + "_CONTROLLER_URL"
	EnvInternalToken        = EnvPrefix + "_INTERNAL_TOKEN"
	EnvDefaultWorkerRuntime = EnvPrefix + "_DEFAULT_WORKER_RUNTIME"
	EnvDefaultModel         = EnvPrefix + "_DEFAULT_MODEL"
	EnvOutboxInterval       = EnvPrefix + "_OUTBOX_INTERVAL"
	EnvLogLevel             = EnvPrefix + "_LOG_LEVEL"
	EnvShutdownTimeout      = EnvPrefix + "_SHUTDOWN_TIMEOUT"
)

// Defaults applied when neither flag nor environment variable is set.
const (
	DefaultListenAddr      = ":8080"
	DefaultLogLevel        = "info"
	DefaultShutdownTimeout = 15 * time.Second
	// DefaultWorkerRuntime is the service-side mirror of the controller's
	// defaultWorkerRuntime (ResolveRuntime fallback, interface.go:91).
	DefaultWorkerRuntime = "openclaw"
	// DefaultOutboxInterval is the dispatcher poll period (R1 value).
	DefaultOutboxInterval = 5 * time.Second
	// DefaultServiceID identifies the instance in ops.upgrade_leases and
	// audit rows when AGENTTEAMS_SERVICE_SERVICE_ID is not set.
	DefaultServiceID = "agentteams-service"
)

// Config holds the resolved service configuration.
type Config struct {
	// ListenAddr is the TCP listen address for the public HTTP face.
	ListenAddr string
	// UDS is the Unix domain socket path for the internal health face.
	// Empty means the UDS face is disabled.
	UDS string
	// Enabled switches the service API on (G1 dual mode, 58 §6):
	// disabled (default) = the controller's current state, the service
	// boots nothing and exits 0; enabled = the 5 public worker routes are
	// served.
	Enabled bool
	// ServiceID identifies this instance (upgrade lease holder, audit
	// correlation). Default: DefaultServiceID.
	ServiceID string
	// ResDSN is the MySQL/OceanBase DSN for the res schema (58 §4).
	// Required when Enabled. No default: credentials from the environment.
	ResDSN string
	// OpsDSN is the MySQL/OceanBase DSN for the ops schema (58 §4).
	// Required when Enabled. No default.
	OpsDSN string
	// ControllerURL is the base URL of agentteams-controller's internal
	// API (token-review / provision / deprovision / execution, 58 §5).
	// Required when Enabled.
	ControllerURL string
	// InternalToken is the shared secret for service<->controller internal
	// calls. Required when Enabled.
	InternalToken string
	// DefaultWorkerRuntime mirrors the controller's defaultWorkerRuntime
	// (applied when the create request omits runtime).
	DefaultWorkerRuntime string
	// DefaultModel mirrors the controller's defaultWorkerModel (applied
	// when the create request omits model). Empty = no service-side default.
	DefaultModel string
	// OutboxInterval is the outbox dispatcher poll period.
	OutboxInterval time.Duration
	// LogLevel is one of debug, info, warn, error.
	LogLevel string
	// ShutdownTimeout bounds graceful shutdown (HTTP drain + module stop + DB close).
	ShutdownTimeout time.Duration
}

// Load resolves the configuration from args (command line, without program
// name), the environment, and defaults, in that precedence order.
func Load(args []string) (*Config, error) {
	cfg := &Config{
		ListenAddr:      DefaultListenAddr,
		LogLevel:        DefaultLogLevel,
		ShutdownTimeout: DefaultShutdownTimeout,
	}

	cfg.ServiceID = DefaultServiceID
	cfg.DefaultWorkerRuntime = DefaultWorkerRuntime
	cfg.OutboxInterval = DefaultOutboxInterval

	fs := flag.NewFlagSet("agentteams-service", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&cfg.ListenAddr, "listen-addr", cfg.ListenAddr, "TCP listen address (env "+EnvListenAddr+")")
	fs.StringVar(&cfg.UDS, "uds", cfg.UDS, "Unix socket path for the internal health face, empty to disable (env "+EnvUDS+")")
	fs.BoolVar(&cfg.Enabled, "enabled", cfg.Enabled, "serve the public worker API (G1 dual mode; env "+EnvEnabled+")")
	fs.StringVar(&cfg.ServiceID, "service-id", cfg.ServiceID, "instance identity for lease/audit (env "+EnvServiceID+")")
	fs.StringVar(&cfg.ResDSN, "res-dsn", cfg.ResDSN, "MySQL DSN for the res schema, required when enabled (env "+EnvResDSN+")")
	fs.StringVar(&cfg.OpsDSN, "ops-dsn", cfg.OpsDSN, "MySQL DSN for the ops schema, required when enabled (env "+EnvOpsDSN+")")
	fs.StringVar(&cfg.ControllerURL, "controller-url", cfg.ControllerURL, "controller internal API base URL, required when enabled (env "+EnvControllerURL+")")
	fs.StringVar(&cfg.InternalToken, "internal-token", cfg.InternalToken, "service<->controller shared secret, required when enabled (env "+EnvInternalToken+")")
	fs.StringVar(&cfg.DefaultWorkerRuntime, "default-worker-runtime", cfg.DefaultWorkerRuntime, "runtime applied when a create request omits it (env "+EnvDefaultWorkerRuntime+")")
	fs.StringVar(&cfg.DefaultModel, "default-model", cfg.DefaultModel, "model applied when a create request omits it (env "+EnvDefaultModel+")")
	fs.DurationVar(&cfg.OutboxInterval, "outbox-interval", cfg.OutboxInterval, "outbox dispatcher poll period (env "+EnvOutboxInterval+")")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log level: debug|info|warn|error (env "+EnvLogLevel+")")
	fs.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", cfg.ShutdownTimeout, "graceful shutdown deadline (env "+EnvShutdownTimeout+")")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	// Flags explicitly set on the command line win over the environment.
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	if v, ok := envIfNotSet(explicit, "listen-addr", EnvListenAddr); ok {
		cfg.ListenAddr = v
	}
	if v, ok := envIfNotSet(explicit, "uds", EnvUDS); ok {
		cfg.UDS = v
	}
	if v, ok := envIfNotSet(explicit, "enabled", EnvEnabled); ok {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			cfg.Enabled = true
		case "0", "false", "no", "off":
			cfg.Enabled = false
		default:
			return nil, fmt.Errorf("%s=%q: must be a boolean (true|false|1|0)", EnvEnabled, v)
		}
	}
	if v, ok := envIfNotSet(explicit, "service-id", EnvServiceID); ok {
		cfg.ServiceID = v
	}
	if v, ok := envIfNotSet(explicit, "res-dsn", EnvResDSN); ok {
		cfg.ResDSN = v
	}
	if v, ok := envIfNotSet(explicit, "ops-dsn", EnvOpsDSN); ok {
		cfg.OpsDSN = v
	}
	if v, ok := envIfNotSet(explicit, "controller-url", EnvControllerURL); ok {
		cfg.ControllerURL = v
	}
	if v, ok := envIfNotSet(explicit, "internal-token", EnvInternalToken); ok {
		cfg.InternalToken = v
	}
	if v, ok := envIfNotSet(explicit, "default-worker-runtime", EnvDefaultWorkerRuntime); ok {
		cfg.DefaultWorkerRuntime = v
	}
	if v, ok := envIfNotSet(explicit, "default-model", EnvDefaultModel); ok {
		cfg.DefaultModel = v
	}
	if v, ok := envIfNotSet(explicit, "outbox-interval", EnvOutboxInterval); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("%s=%q: %w", EnvOutboxInterval, v, err)
		}
		cfg.OutboxInterval = d
	}
	if v, ok := envIfNotSet(explicit, "log-level", EnvLogLevel); ok {
		cfg.LogLevel = v
	}
	if v, ok := envIfNotSet(explicit, "shutdown-timeout", EnvShutdownTimeout); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("%s=%q: %w", EnvShutdownTimeout, v, err)
		}
		cfg.ShutdownTimeout = d
	}

	return cfg, nil
}

// envIfNotSet returns the environment value unless the matching flag was set
// explicitly. An empty variable is treated as unset.
func envIfNotSet(explicit map[string]bool, flagName, envName string) (string, bool) {
	if explicit[flagName] {
		return "", false
	}
	v, ok := os.LookupEnv(envName)
	return v, ok && v != ""
}

// Validate checks the resolved configuration. main prints the error to
// stderr and exits 2.
//
// Disabled mode (G1 mode 1) needs no database or controller: the process
// exits 0 before touching either, so only the log level and shutdown
// timeout are validated. Enabled mode requires DSNs plus the controller
// connection (58 §6 双模式).
func (c *Config) Validate() error {
	if c.Enabled {
		var missing []string
		if c.ResDSN == "" {
			missing = append(missing, EnvResDSN)
		}
		if c.OpsDSN == "" {
			missing = append(missing, EnvOpsDSN)
		}
		if len(missing) > 0 {
			return fmt.Errorf("database DSN is required: %s", strings.Join(missing, ", "))
		}
		if c.ControllerURL == "" {
			return fmt.Errorf("controller URL is required: %s", EnvControllerURL)
		}
		if c.InternalToken == "" {
			return fmt.Errorf("internal token is required: %s", EnvInternalToken)
		}
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid log level %q (want debug|info|warn|error)", c.LogLevel)
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("shutdown timeout must be positive, got %s", c.ShutdownTimeout)
	}
	return nil
}
