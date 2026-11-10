package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Test fixture identities go through the production authz and HTTP routing;
// Kubernetes resources/object storage are in-memory. Langfuse, Collector,
// the model and the QwenPaw analysis processes are real external services.
type traceAcceptanceAuth struct{}

func (*traceAcceptanceAuth) Authenticate(_ context.Context, token string) (*authpkg.CallerIdentity, error) {
	switch token {
	case "acceptance-admin":
		return &authpkg.CallerIdentity{Role: authpkg.RoleAdmin}, nil
	case "acceptance-manager":
		return &authpkg.CallerIdentity{Role: authpkg.RoleManager}, nil
	case "acceptance-leader":
		return &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Team: "tracing-team"}, nil
	case "acceptance-foreign":
		return &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Team: "other"}, nil
	}
	return nil, fmt.Errorf("invalid acceptance token")
}

func TestQwenPawLangfuseAcceptance(t *testing.T) {
	base := os.Getenv("AGENTTEAMS_ACCEPTANCE_LANGFUSE_URL")
	if base == "" {
		t.Skip("opt-in live Langfuse and model acceptance")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	artifacts := filepath.Join(root, "output/native-acceptance")
	batches, err := filepath.Glob(filepath.Join(artifacts, "batch-*.bin"))
	if err != nil || len(batches) == 0 {
		t.Fatal("run capture_qwenpaw_live.py first")
	}
	collector := os.Getenv("AGENTTEAMS_ACCEPTANCE_COLLECTOR_URL")
	if collector == "" {
		t.Fatal("AGENTTEAMS_ACCEPTANCE_COLLECTOR_URL required")
	}
	for _, file := range batches {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest("POST", strings.TrimRight(collector, "/")+"/v1/traces", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-protobuf")
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("Collector rejected native OTLP batch: %d", resp.StatusCode)
		}
	}
	store := ossfake.NewMemory()
	putProject(store, "teams/tracing-team/shared/projects/tracing-acceptance/meta.json", map[string]any{"project_id": "tracing-acceptance", "team_id": "tracing-team", "status": "completed", "plan_type": "dag", "tasks": []map[string]any{{"task_id": "full-session-acceptance", "assigned_to": "qwenpaw-full-session", "status": "completed"}}})
	team := team("tracing-team")
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "qwenpaw-full-session"}}
	h := newProjectTestHandler(t, store, team)
	if err := corev1.AddToScheme(h.client.Scheme()); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Create(t.Context(), &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "qwenpaw-full-session", Namespace: "default"}, Spec: v1beta1.WorkerSpec{Runtime: "qwenpaw"}}); err != nil {
		t.Fatal(err)
	}
	mw := authpkg.NewMiddleware(&traceAcceptanceAuth{}, nil, authpkg.NewAuthorizer(), h.client, "default")
	server := NewHTTPServer(":0", ServerDeps{Client: h.client, OSS: h.oss, Namespace: "default", AuthMw: mw})
	httpServer := httptest.NewUnstartedServer(server.Mux)
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer.Listener = listener
	httpServer.Start()
	defer httpServer.Close()
	call := func(method, path, token string, body []byte) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, httpServer.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, data
	}
	settings, _ := json.Marshal(map[string]any{"backend": "langfuse", "enabled": true, "captureContent": true, "mode": "collector", "langfuseUrl": base, "publicKey": "pk-lf-at-local-demo", "secretKey": "sk-lf-at-local-demo", "projectId": "at-workers", "collectorEndpoint": collector})
	if status, body := call("PUT", "/api/v1/observability/config", "acceptance-admin", settings); status != 200 || bytes.Contains(body, []byte("sk-lf-")) {
		t.Fatalf("configuration save failed: %d", status)
	}
	path := "/api/v1/projects/tracing-acceptance/tasks/full-session-acceptance/traces?team=tracing-team&includeRelated=true"
	var bundle taskTraceBundle
	deadline := time.Now().Add(90 * time.Second)
	for {
		status, body := call("GET", path, "acceptance-leader", nil)
		if status != 200 {
			t.Fatalf("Controller query failed: %d %s", status, body)
		}
		if err := json.Unmarshal(body, &bundle); err != nil {
			t.Fatal(err)
		}
		if len(bundle.Traces) >= 2 && len(bundle.Evidence) > 0 {
			if err := os.WriteFile(filepath.Join(artifacts, "controller-bundle.json"), body, 0o600); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native Worker spans did not become visible in Langfuse")
		}
		time.Sleep(time.Second)
	}
	if status, _ := call("GET", path, "acceptance-foreign", nil); status != 404 {
		t.Fatalf("foreign Leader returned %d", status)
	}
	agt := os.Getenv("AGENTTEAMS_ACCEPTANCE_AGT")
	modelEnv := os.Getenv("AGENTTEAMS_ACCEPTANCE_MODEL_ENV")
	image := os.Getenv("AGENTTEAMS_ACCEPTANCE_QWENPAW_IMAGE")
	network := os.Getenv("AGENTTEAMS_ACCEPTANCE_MODEL_NETWORK")
	if agt == "" || modelEnv == "" || image == "" || network == "" {
		t.Fatal("set acceptance AGT, MODEL_ENV, QWENPAW_IMAGE and MODEL_NETWORK for real Manager/Leader analysis")
	}
	endpoint := fmt.Sprintf("http://host.docker.internal:%d", listener.Addr().(*net.TCPAddr).Port)
	for _, role := range []string{"manager", "leader"} {
		ctx, cancel := context.WithTimeout(t.Context(), 210*time.Second)
		command := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", network, "-v", root+":/src:ro", "-v", artifacts+":/artifacts", "-v", agt+":/tmp/agt:ro", "--env-file", modelEnv, "-e", "ACCEPTANCE_ROLE="+role, "-e", "AGENTTEAMS_CONTROLLER_URL="+endpoint, "-e", "AGENTTEAMS_AUTH_TOKEN=acceptance-"+role, "--entrypoint", "/opt/venv/qwenpaw/bin/python", image, "/src/examples/observability/analyze_task.py")
		output, err := command.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("native %s analysis failed: %v %s", role, err, output)
		}
		var report struct {
			Diagnosis struct {
				Evidence []traceEvidence `json:"evidence"`
			} `json:"diagnosis"`
		}
		data, err := os.ReadFile(filepath.Join(artifacts, "diagnosis-"+role+".json"))
		if err != nil || json.Unmarshal(data, &report) != nil {
			t.Fatalf("invalid %s diagnosis", role)
		}
		matched := false
		for _, item := range report.Diagnosis.Evidence {
			for _, actual := range bundle.Evidence {
				if item.TraceID == actual.TraceID && item.SpanID == actual.SpanID {
					matched = true
				}
			}
		}
		if !matched {
			t.Fatalf("%s did not cite an actual failure span", role)
		}
		t.Logf("%s read task APIs and cited actual Langfuse error evidence", role)
	}
}
