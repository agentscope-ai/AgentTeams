package observability

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRuntimeSettingsContainOnlyCollectionConfiguration(t *testing.T) {
	config := map[string]any{"revision": "r1", "enabled": true, "captureContent": true,
		"collectorEndpoint": "http://collector:4318", "collectorHeaders": "ingest-secret",
		"secretKey": "query-secret", "publicKey": "query-public"}
	settings := RuntimeSettings(config)
	if settings == nil || !settings.Enabled || !settings.CaptureContent || settings.CollectorEndpoint != "http://collector:4318" {
		t.Fatalf("unexpected collection configuration: %+v", settings)
	}
	data, _ := json.Marshal(settings)
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), "query-") {
		t.Fatal("collection record contains credentials")
	}
	config["enabled"] = false
	settings = RuntimeSettings(config)
	if settings.Enabled || settings.CaptureContent || settings.CollectorEndpoint != "" || settings.Revision != "r1" {
		t.Fatal("disabled record retains collection settings")
	}
	if RuntimeSettings(map[string]any{}) != nil {
		t.Fatal("unconfigured Controller must retain compatibility")
	}
}

func TestContentCaptureDefaultsOnAndHonorsExplicitDisable(t *testing.T) {
	config := map[string]any{"revision": "r-default", "enabled": true}
	if !RuntimeSettings(config).CaptureContent || WorkerEnv(config)["AGENTTEAMS_OTEL_CAPTURE_CONTENT"] != "true" {
		t.Fatal("missing capture setting must use the global enabled default")
	}
	config["captureContent"] = false
	if RuntimeSettings(config).CaptureContent || WorkerEnv(config)["AGENTTEAMS_OTEL_CAPTURE_CONTENT"] != "false" {
		t.Fatal("explicit global disable was not honored")
	}
}
