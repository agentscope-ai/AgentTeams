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

func TestNewWorkerContainerReadsControllerTracingDefaults(t *testing.T) {
	scheme := newControllerTestScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, runtime := range []string{"qwenpaw", "hermes", "openclaw", "deepseek-harness", "codex", "openhuman"} {
		for _, enabled := range []bool{true, false} {
			data, _ := json.Marshal(map[string]any{"revision": "central-revision", "enabled": enabled, "captureContent": true, "collectorEndpoint": "http://collector:4318", "collectorHeaders": "Authorization=Bearer%20ingest", "secretKey": "query-secret-never-worker"})
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: observability.SecretName, Namespace: "default"}, Data: map[string][]byte{"config.json": data}}
			k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
			deps := MemberDeps{EnvBuilder: mocks.NewMockEnvBuilder(), ObservabilityReader: k8s}
			member := MemberContext{Name: "w", RuntimeName: "w", Namespace: "default", Spec: v1beta1.WorkerSpec{Runtime: runtime, Env: map[string]string{"KEEP": "yes", "AGENTTEAMS_OTEL_ENABLED": "stale", "OTEL_EXPORTER_OTLP_HEADERS": "old"}}}
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
}

func TestTeamProjectionKeepsCentralObservabilitySettings(t *testing.T) {
	team := &v1beta1.Team{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "default"},
		Spec: v1beta1.TeamSpec{WorkerMembers: []v1beta1.TeamWorkerRef{{Name: "worker", Role: "team_leader"}}}}
	worker := newWorker("worker", v1beta1.WorkerSpec{Runtime: "openclaw", Model: "qwen-plus"})
	worker.Status.MatrixUserID = "@worker:matrix.local"
	rig := newTeamReconcileRig(t, team, worker)
	scheme := newControllerTestScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"revision": "central", "enabled": true, "collectorEndpoint": "http://collector:4318"})
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: observability.SecretName, Namespace: "default"}, Data: map[string][]byte{"config.json": data}}
	rig.r.ObservabilityReader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	if _, _, err := rig.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	request, ok := runtimeConfigCallFor(rig.deployer.Calls.DeployMemberRuntimeConfig, "worker")
	if !ok || request.Observability == nil || request.Observability.Revision != "central" {
		t.Fatal("team projection dropped central tracing configuration")
	}
}

func TestManagerContainerReceivesCentralTracingWithoutQueryCredentials(t *testing.T) {
	scheme := newControllerTestScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, runtime := range []string{"qwenpaw", "hermes", "deepseek-harness", "claudecode", "codex"} {
		for _, enabled := range []bool{true, false} {
			data, _ := json.Marshal(map[string]any{"revision": "manager-central", "enabled": enabled, "captureContent": false, "collectorEndpoint": "http://collector:4318", "secretKey": "query-secret-never-manager"})
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: observability.SecretName, Namespace: "default"}, Data: map[string][]byte{"config.json": data}}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
			b := mocks.NewMockWorkerBackend()
			r := &ManagerReconciler{ObservabilityReader: reader, Provisioner: mocks.NewMockManagerProvisioner(), EnvBuilder: mocks.NewMockManagerEnvBuilder()}
			m := &v1beta1.Manager{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: v1beta1.ManagerSpec{Runtime: runtime, Env: map[string]string{"KEEP": "yes"}}}
			if _, err := r.createManagerContainer(t.Context(), &managerScope{manager: m, provResult: &service.ManagerProvisionResult{MatrixToken: "fixture"}}, b); err != nil {
				t.Fatal(err)
			}
			env := b.Calls.CreateReqs[0].Env
			if env["AGENTTEAMS_OTEL_ENABLED"] != map[bool]string{true: "true", false: "false"}[enabled] || env["AGENTTEAMS_OTEL_CONFIG_REVISION"] != "manager-central" || env["KEEP"] != "yes" {
				t.Fatalf("%s: incomplete Manager tracing environment", runtime)
			}
			if enabled && env["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] != "http://collector:4318/v1/traces" {
				t.Fatal("Manager Collector endpoint absent")
			}
			encoded, _ := json.Marshal(env)
			if strings.Contains(string(encoded), "query-secret") {
				t.Fatal("query credential entered Manager container")
			}
		}
	}
}
