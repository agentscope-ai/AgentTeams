package config

import (
	"strings"
	"testing"
	"time"
)

// clearServiceEnv blanks every AGENTTEAMS_SERVICE_* variable so tests do not
// depend on the ambient environment (empty values are treated as unset).
func clearServiceEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		EnvListenAddr, EnvUDS, EnvEnabled, EnvServiceID, EnvResDSN, EnvOpsDSN,
		EnvControllerURL, EnvInternalToken, EnvDefaultWorkerRuntime, EnvDefaultModel,
		EnvOutboxInterval, EnvLogLevel, EnvShutdownTimeout,
	} {
		t.Setenv(k, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearServiceEnv(t)

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load(nil) error: %v", err)
	}
	if cfg.ListenAddr != DefaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, DefaultListenAddr)
	}
	if cfg.LogLevel != DefaultLogLevel {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, DefaultLogLevel)
	}
	if cfg.ShutdownTimeout != DefaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %v, want %v", cfg.ShutdownTimeout, DefaultShutdownTimeout)
	}
	if cfg.UDS != "" {
		t.Errorf("UDS = %q, want empty (disabled)", cfg.UDS)
	}
	if cfg.ResDSN != "" || cfg.OpsDSN != "" {
		t.Errorf("DSNs must have no default, got res=%q ops=%q", cfg.ResDSN, cfg.OpsDSN)
	}
}

func TestLoadEnvOverride(t *testing.T) {
	clearServiceEnv(t)
	t.Setenv(EnvListenAddr, ":9090")
	t.Setenv(EnvUDS, "/tmp/ats.sock")
	t.Setenv(EnvResDSN, "u:p@tcp(127.0.0.1:3306)/res")
	t.Setenv(EnvOpsDSN, "u:p@tcp(127.0.0.1:3306)/ops")
	t.Setenv(EnvControllerURL, "http://127.0.0.1:8090")
	t.Setenv(EnvInternalToken, "tok")
	t.Setenv(EnvLogLevel, "debug")
	t.Setenv(EnvShutdownTimeout, "30s")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.ListenAddr != ":9090" {
		t.Errorf("ListenAddr = %q, want :9090", cfg.ListenAddr)
	}
	if cfg.UDS != "/tmp/ats.sock" {
		t.Errorf("UDS = %q, want /tmp/ats.sock", cfg.UDS)
	}
	if cfg.ResDSN != "u:p@tcp(127.0.0.1:3306)/res" {
		t.Errorf("ResDSN = %q", cfg.ResDSN)
	}
	if cfg.OpsDSN != "u:p@tcp(127.0.0.1:3306)/ops" {
		t.Errorf("OpsDSN = %q", cfg.OpsDSN)
	}
	if cfg.ControllerURL != "http://127.0.0.1:8090" {
		t.Errorf("ControllerURL = %q", cfg.ControllerURL)
	}
	if cfg.InternalToken != "tok" {
		t.Errorf("InternalToken = %q", cfg.InternalToken)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.ShutdownTimeout != 30*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 30s", cfg.ShutdownTimeout)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate with both DSNs set: %v", err)
	}
}

func TestLoadFlagBeatsEnv(t *testing.T) {
	clearServiceEnv(t)
	t.Setenv(EnvListenAddr, ":9090")
	t.Setenv(EnvResDSN, "env-res-dsn")

	cfg, err := Load([]string{"-listen-addr", ":7070", "-res-dsn", "flag-res-dsn"})
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.ListenAddr != ":7070" {
		t.Errorf("ListenAddr = %q, want :7070 (flag beats env)", cfg.ListenAddr)
	}
	if cfg.ResDSN != "flag-res-dsn" {
		t.Errorf("ResDSN = %q, want flag-res-dsn (flag beats env)", cfg.ResDSN)
	}
	if cfg.OpsDSN != "" {
		t.Errorf("OpsDSN = %q, want empty", cfg.OpsDSN)
	}
}

func TestLoadBadShutdownTimeout(t *testing.T) {
	clearServiceEnv(t)
	t.Setenv(EnvShutdownTimeout, "not-a-duration")
	if _, err := Load(nil); err == nil {
		t.Fatal("Load with invalid AGENTTEAMS_SERVICE_SHUTDOWN_TIMEOUT: want error, got nil")
	}
}

func TestValidateDisabledNeedsNothing(t *testing.T) {
	clearServiceEnv(t)

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Enabled {
		t.Fatal("Enabled must default to false (G1 mode 1)")
	}
	// Disabled mode (G1 mode 1): no DSNs, no controller — Validate passes.
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate disabled with no DSNs: %v", err)
	}
	if cfg.ServiceID != DefaultServiceID {
		t.Errorf("ServiceID = %q, want %q", cfg.ServiceID, DefaultServiceID)
	}
	if cfg.DefaultWorkerRuntime != DefaultWorkerRuntime {
		t.Errorf("DefaultWorkerRuntime = %q, want %q", cfg.DefaultWorkerRuntime, DefaultWorkerRuntime)
	}
	if cfg.OutboxInterval != DefaultOutboxInterval {
		t.Errorf("OutboxInterval = %v, want %v", cfg.OutboxInterval, DefaultOutboxInterval)
	}
}

func TestValidateEnabledRequiresEverything(t *testing.T) {
	clearServiceEnv(t)

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	cfg.Enabled = true
	cfg.LogLevel = DefaultLogLevel
	cfg.ShutdownTimeout = DefaultShutdownTimeout

	err = cfg.Validate()
	if err == nil {
		t.Fatal("Validate enabled with no DSNs: want error, got nil")
	}
	if !strings.Contains(err.Error(), "database DSN is required") {
		t.Errorf("error %q must contain %q", err.Error(), "database DSN is required")
	}
	if !strings.Contains(err.Error(), EnvResDSN) || !strings.Contains(err.Error(), EnvOpsDSN) {
		t.Errorf("error %q must name both missing env vars", err.Error())
	}

	cfg.ResDSN = "u:p@tcp(h)/res"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), EnvOpsDSN) {
		t.Errorf("only ResDSN set: want error naming %s, got %v", EnvOpsDSN, err)
	}

	cfg = &Config{}
	cfg.Enabled = true
	cfg.OpsDSN = "u:p@tcp(h)/ops"
	cfg.LogLevel = DefaultLogLevel
	cfg.ShutdownTimeout = DefaultShutdownTimeout
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), EnvResDSN) {
		t.Errorf("only OpsDSN set: want error naming %s, got %v", EnvResDSN, err)
	}

	cfg.ResDSN = "u:p@tcp(h)/res"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), EnvControllerURL) {
		t.Errorf("DSNs set, no controller: want error naming %s, got %v", EnvControllerURL, err)
	}
	cfg.ControllerURL = "http://c"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), EnvInternalToken) {
		t.Errorf("no token: want error naming %s, got %v", EnvInternalToken, err)
	}
	cfg.InternalToken = "t"
	if err := cfg.Validate(); err != nil {
		t.Errorf("enabled with everything set: %v", err)
	}
}

func TestLoadEnabledEnv(t *testing.T) {
	clearServiceEnv(t)
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvServiceID, "svc-1")
	t.Setenv(EnvDefaultWorkerRuntime, "qwenpaw")
	t.Setenv(EnvDefaultModel, "model-x")
	t.Setenv(EnvOutboxInterval, "250ms")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if !cfg.Enabled {
		t.Error("Enabled = false, want true")
	}
	if cfg.ServiceID != "svc-1" {
		t.Errorf("ServiceID = %q, want svc-1", cfg.ServiceID)
	}
	if cfg.DefaultWorkerRuntime != "qwenpaw" {
		t.Errorf("DefaultWorkerRuntime = %q, want qwenpaw", cfg.DefaultWorkerRuntime)
	}
	if cfg.DefaultModel != "model-x" {
		t.Errorf("DefaultModel = %q, want model-x", cfg.DefaultModel)
	}
	if cfg.OutboxInterval != 250*time.Millisecond {
		t.Errorf("OutboxInterval = %v, want 250ms", cfg.OutboxInterval)
	}

	clearServiceEnv(t)
	t.Setenv(EnvEnabled, "banana")
	if _, err := Load(nil); err == nil {
		t.Error("invalid ENABLED value: want error, got nil")
	}
}

func TestValidateBadLogLevel(t *testing.T) {
	cfg := &Config{
		ResDSN:          "u:p@tcp(h)/res",
		OpsDSN:          "u:p@tcp(h)/ops",
		LogLevel:        "verbose",
		ShutdownTimeout: DefaultShutdownTimeout,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate with LogLevel=verbose: want error, got nil")
	}
}

func TestValidateNonPositiveTimeout(t *testing.T) {
	cfg := &Config{
		ResDSN:   "u:p@tcp(h)/res",
		OpsDSN:   "u:p@tcp(h)/ops",
		LogLevel: DefaultLogLevel,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate with zero ShutdownTimeout: want error, got nil")
	}
}
