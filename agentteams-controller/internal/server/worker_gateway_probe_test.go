package server

import (
	"context"
	"encoding/json"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func probeFixture(t *testing.T, gateway string) *workerGatewayProbe {
	t.Helper()
	worker := &v1beta1.Worker{}
	worker.Name = "alice"
	worker.Namespace = "default"
	worker.Spec.Model = "saved-model"
	worker.Spec.WorkerName = "runtime-alice"
	worker.Spec.McpServers = []v1beta1.MCPServer{{Name: "github", URL: gateway + "/mcp-servers/github/mcp"}}
	return &workerGatewayProbe{client: fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).WithObjects(worker).Build(), namespace: "default", gatewayURL: gateway, healthStore: ossfake.NewMemory(), key: func(context.Context, string) (string, error) { return "worker-secret", nil }}
}

func runProbe(h *workerGatewayProbe, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	req.SetPathValue("name", "alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestGatewayModelProbeQueuesHarnessRequestWithoutCallingProvider(t *testing.T) {
	calls := 0
	gateway := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer gateway.Close()
	h := probeFixture(t, gateway.URL)
	rec := runProbe(h, `{"kind":"model","model":"injected","url":"http://invalid"}`)
	if rec.Code != 202 || calls != 0 || !strings.Contains(rec.Body.String(), `"queued":true`) {
		t.Fatal(rec.Code, rec.Body.String(), calls)
	}
	data, err := h.healthStore.GetObject(context.Background(), "agents/runtime-alice/runtime/health-request.json")
	if err != nil || strings.Contains(string(data), "injected") || strings.Contains(string(data), "invalid") {
		t.Fatal(string(data), err)
	}
}

func TestGatewayProbeMCPInitializationAndTools(t *testing.T) {
	methods := []string{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		method, _ := body["method"].(string)
		methods = append(methods, method)
		if r.Header.Get("Authorization") != "Bearer worker-secret" {
			t.Error("missing worker identity")
		}
		if method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "session")
			w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05"}}`))
			return
		}
		if r.Header.Get("Mcp-Session-Id") != "session" {
			t.Error("missing MCP session")
		}
		if method == "notifications/initialized" {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"tools\":[]}}\n\n"))
	}))
	defer gateway.Close()
	rec := runProbe(probeFixture(t, gateway.URL), `{"kind":"mcp","server":"github"}`)
	if strings.Join(methods, ",") != "initialize,notifications/initialized,tools/list" || !strings.Contains(rec.Body.String(), `"success":true`) {
		t.Fatalf("%v %s", methods, rec.Body.String())
	}
}

func TestGatewayModelProbeDoesNotFallBackWhenHarnessStorageIsUnavailable(t *testing.T) {
	h := probeFixture(t, "http://127.0.0.1:1/private")
	h.healthStore = nil
	rec := runProbe(h, `{"kind":"model"}`)
	if rec.Code != 503 {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func TestGatewayProbeRejectsUnknownServerAndScopedCaller(t *testing.T) {
	h := probeFixture(t, "http://gateway")
	rec := runProbe(h, `{"kind":"mcp","server":"unknown"}`)
	if rec.Code != 400 {
		t.Fatal(rec.Code)
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"kind":"model"}`))
	req.SetPathValue("name", "alice")
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), &authpkg.CallerIdentity{Role: authpkg.RoleHuman}))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatal(rec.Code)
	}
}
