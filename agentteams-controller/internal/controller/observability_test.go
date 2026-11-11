package controller

import (
	"encoding/json"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/observability"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil/mocks"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNewQwenPawContainerReadsControllerTracingDefaults(t *testing.T) {
	scheme := newControllerTestScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{true, false} {
		data, _ := json.Marshal(map[string]any{"revision": "central-revision", "enabled": enabled, "captureContent": true, "collectorEndpoint": "http://collector:4318", "collectorHeaders": "Authorization=Bearer%20ingest", "secretKey": "query-secret-never-worker"})
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: observability.SecretName, Namespace: "default"}, Data: map[string][]byte{"config.json": data}}
		k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		deps := MemberDeps{EnvBuilder: mocks.NewMockEnvBuilder(), ObservabilityReader: k8s}
		member := MemberContext{Name: "w", RuntimeName: "w", Namespace: "default", Spec: v1beta1.WorkerSpec{Runtime: "qwenpaw", Env: map[string]string{"KEEP": "yes", "AGENTTEAMS_OTEL_ENABLED": "stale", "OTEL_EXPORTER_OTLP_HEADERS": "old"}}}
		env, err := buildMemberWorkerEnv(t.Context(), deps, member, &service.WorkerProvisionResult{})
		if err != nil {
			t.Fatal(err)
		}
		if env["AGENTTEAMS_OTEL_ENABLED"] != map[bool]string{true: "true", false: "false"}[enabled] || env["AGENTTEAMS_OTEL_CONFIG_REVISION"] != "central-revision" || env["KEEP"] != "yes" {
			t.Fatalf("unexpected projection: %+v", env)
		}
		if enabled && env["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] != "http://collector:4318/v1/traces" {
			t.Fatal("new Worker has no Collector endpoint")
		}
		if !enabled && env["OTEL_EXPORTER_OTLP_HEADERS"] != "" {
			t.Fatal("disabled collection retained stale authentication")
		}
		encoded, _ := json.Marshal(env)
		if strings.Contains(string(encoded), "query-secret") {
			t.Fatal("query credentials entered Worker env")
		}
	}
}
