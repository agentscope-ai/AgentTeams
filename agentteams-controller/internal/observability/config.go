// Package observability owns the Controller's persisted tracing configuration.
package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const SecretName = "agentteams-observability"

func Read(ctx context.Context, reader client.Reader, namespace string) (map[string]any, *corev1.Secret, error) {
	secret := &corev1.Secret{}
	err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: SecretName}, secret)
	if apierrors.IsNotFound(err) {
		return map[string]any{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	config := map[string]any{}
	if json.Unmarshal(secret.Data["config.json"], &config) != nil {
		return nil, nil, fmt.Errorf("invalid stored observability configuration")
	}
	return config, secret, nil
}

// WorkerEnv projects collection settings only. Query credentials never enter
// the Worker environment. A saved disabled configuration overrides old env.
func WorkerEnv(config map[string]any) map[string]string {
	text := func(key string) string { value, _ := config[key].(string); return value }
	if text("revision") == "" {
		return nil
	}
	env := map[string]string{
		"AGENTTEAMS_OTEL_ENABLED": "false", "AGENTTEAMS_OTEL_CONFIG_REVISION": text("revision"),
		"AGENTTEAMS_OTEL_CAPTURE_CONTENT": "false", "OTEL_EXPORTER_OTLP_ENDPOINT": "",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
		"OTEL_EXPORTER_OTLP_HEADERS": "", "OTEL_EXPORTER_OTLP_TRACES_HEADERS": "",
	}
	if config["enabled"] == true {
		env["AGENTTEAMS_OTEL_ENABLED"] = "true"
		env["AGENTTEAMS_OTEL_CAPTURE_CONTENT"] = fmt.Sprint(config["captureContent"] == true)
		endpoint := strings.TrimRight(text("collectorEndpoint"), "/")
		env["OTEL_EXPORTER_OTLP_ENDPOINT"] = endpoint
		env["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] = endpoint + "/v1/traces"
		env["OTEL_EXPORTER_OTLP_HEADERS"] = text("collectorHeaders")
		env["OTEL_EXPORTER_OTLP_TRACES_HEADERS"] = text("collectorHeaders")
	}
	return env
}
