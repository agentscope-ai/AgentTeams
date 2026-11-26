package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type assetGateway struct {
	gateway.Client
	calls     []string
	responses map[string]any
	fail      string
	payloads  map[string]map[string]any
}

func (g *assetGateway) ManageMCP(_ context.Context, method, path string, body any) (any, error) {
	g.calls = append(g.calls, method+" "+path)
	if method+" "+path == g.fail {
		return nil, assetError(502, "gateway unavailable")
	}
	if value, ok := g.responses[method+" "+path]; ok {
		return value, nil
	}
	if path == "/v1/wasm-plugins/transformer" {
		return map[string]any{"imageRepository": "oci://transformer"}, nil
	}
	return nil, nil
}

func (g *assetGateway) ManageAI(_ context.Context, method, resource, name string, body json.RawMessage) ([]byte, int, error) {
	if len(body) > 0 {
		if g.payloads == nil {
			g.payloads = map[string]map[string]any{}
		}
		g.payloads[method+" "+resource+"/"+name] = decodeObject(body)
	}
	g.calls = append(g.calls, method+" "+resource+"/"+name)
	if method+" "+resource+"/"+name == g.fail {
		return nil, 502, nil
	}
	if value, ok := g.responses[method+" "+resource+"/"+name]; ok {
		data, _ := json.Marshal(map[string]any{"data": value})
		return data, 200, nil
	}
	if resource == "routes" && method == "GET" {
		return []byte(`{"data":[]}`), 200, nil
	}
	return []byte(`{"data":{"name":"example"}}`), 200, nil
}

func TestGatewayModelUpdatePreservesStoredCredential(t *testing.T) {
	g := &assetGateway{responses: map[string]any{"GET providers/example": map[string]any{"name": "example", "type": "openai", "rawConfigs": map[string]any{"apiTokens": []string{"stored-secret"}}}}}
	w := assetRequest(t, &gatewayAssets{gateway: g}, "models", `{"action":"save","update":true,"input":{"name":"example","rawConfigs":{"modelMapping":{"alias":"model"}}}}`)
	tokens, _ := g.payloads["PUT providers/example"]["tokens"].([]any)
	if w.Code != 200 || len(tokens) != 1 || tokens[0] != "stored-secret" || strings.Contains(w.Body.String(), "stored-secret") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestGatewayModelProbeUsesStoredEndpointAndCredential(t *testing.T) {
	t.Setenv("MODEL_PROBE_ALLOW_PRIVATE_NETWORK", "1")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer stored-secret" || r.URL.Path != "/v1/chat/completions" {
			t.Error("wrong upstream credential or path")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "real-model" {
			t.Error("model alias was not resolved")
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer upstream.Close()
	g := &assetGateway{responses: map[string]any{"GET providers/example": map[string]any{"name": "example", "type": "openai", "tokens": []string{"stored-secret"}, "rawConfigs": map[string]any{"openaiCustomUrl": upstream.URL + "/v1", "modelMapping": map[string]string{"team-chat": "real-model"}}}}}
	w := assetRequest(t, &gatewayAssets{gateway: g}, "models", `{"action":"probe","input":{"providerName":"example","model":"team-chat","baseUrl":"http://attacker","token":"injected"}}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":true`) || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestManagedMCPRevokeFailureRemainsDurableAndCannotRegrantOnSave(t *testing.T) {
	store := ossfake.NewMemory()
	mcpPutRegistry(t, store, "docs", `{"name":"docs","managed":true,"url":"http://gateway.test/mcp-servers/docs","transport":"streaminghttp","gatewayState":"ready","authorizedWorkers":["a"],"connection":{"kind":"proxy"},"verification":{"success":true}}`)
	g := &assetGateway{fail: "DELETE /v1/mcpServer/consumers"}
	h := &gatewayAssets{gateway: g, resources: NewResourceHandler(nil, "default", nil, "", nil).WithOSS(store)}
	w := assetRequest(t, h, "mcps", `{"action":"unbind","name":"docs","worker":"a"}`)
	data, _ := store.GetObject(context.Background(), "mcp-servers/docs.json")
	var record managedMCP
	_ = json.Unmarshal(data, &record)
	if !strings.Contains(w.Body.String(), `"success":false`) || !contains(record.PendingRemovalWorkers, "a") {
		t.Fatalf("%s %s", w.Body.String(), data)
	}
}

func TestManagedMCPVerificationDiscoversToolsWithoutBusinessCall(t *testing.T) {
	methods := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		method := textField(body["method"])
		methods = append(methods, method)
		if r.Header.Get("Authorization") != "Bearer probe-secret" {
			t.Error("probe credential missing")
		}
		if method == "notifications/initialized" {
			w.WriteHeader(202)
			return
		}
		var result any = map[string]any{"tools": []any{map[string]any{"name": "read", "description": "credential: probe-secret", "inputSchema": map[string]any{"type": "object"}}}}
		if method == "initialize" {
			result = map[string]any{"protocolVersion": "2025-03-26", "serverInfo": map[string]any{"name": "test", "version": "1"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": body["id"], "result": result})
	}))
	defer upstream.Close()
	record := &managedMCP{URL: upstream.URL, Connection: mcpConnection{Timeout: 1000}, Transport: "streaminghttp"}
	record.Probe.Key = "probe-secret"
	result := verifyMCP(context.Background(), record, "", nil)
	data, _ := json.Marshal(result)
	if result["success"] != true || strings.Join(methods, ",") != "initialize,notifications/initialized,tools/list" || strings.Contains(string(data), "probe-secret") {
		t.Fatalf("%s calls=%v", data, methods)
	}
}

func TestUnsupportedGatewayAllowsCRFallbackOnlyForCapabilityError(t *testing.T) {
	for _, resource := range []string{"models", "mcps"} {
		w := assetRequest(t, &gatewayAssets{}, resource, `{"action":"capabilities"}`)
		if w.Code != 501 || !strings.Contains(w.Body.String(), `"fallbackAllowed":true`) {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	g := &assetGateway{fail: "POST providers/"}
	w := assetRequest(t, &gatewayAssets{gateway: g}, "models", `{"action":"save","input":{"name":"example","type":"openai","tokens":["secret"]}}`)
	if w.Code != 502 || !strings.Contains(w.Body.String(), `"fallbackAllowed":false`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func assetRequest(t *testing.T, h *gatewayAssets, resource, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	r.SetPathValue("asset", resource)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestGatewayModelSaveOwnsRouteCreation(t *testing.T) {
	g := &assetGateway{}
	h := &gatewayAssets{gateway: g}
	w := assetRequest(t, h, "models", `{"action":"save","input":{"name":"example","type":"openai","tokens":["secret"],"rawConfigs":{"modelMapping":{"team-chat":"real-model"}}}}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ready":true`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if strings.Join(g.calls, ",") != "POST providers/,GET routes/,POST routes/" {
		t.Fatalf("calls=%v", g.calls)
	}
	if strings.Contains(w.Body.String(), "secret") {
		t.Fatal("credential leaked")
	}
}

func TestGatewayModelRouteFailureIsNotSuccess(t *testing.T) {
	g := &assetGateway{fail: "POST routes/"}
	w := assetRequest(t, &gatewayAssets{gateway: g}, "models", `{"action":"save","input":{"name":"example","type":"openai","tokens":["secret"],"rawConfigs":{"modelMapping":{"team-chat":"real-model"}}}}`)
	if w.Code != 502 || !strings.Contains(w.Body.String(), "route") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestManagedMCPUsesSameRegistryAndHidesSecrets(t *testing.T) {
	store := ossfake.NewMemory()
	g := &assetGateway{}
	h := &gatewayAssets{gateway: g, resources: NewResourceHandler(nil, "default", nil, "", nil).WithOSS(store), gatewayURL: "http://gateway.test"}
	w := assetRequest(t, h, "mcps", `{"action":"save","input":{"name":"docs","connection":{"kind":"proxy","backendUrl":"http://backend.test/mcp","transport":"streaminghttp","auth":{"type":"bearer","secret":"upstream-secret"},"headers":[],"query":[],"tools":[],"timeout":15000}}}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "upstream-secret") || strings.Contains(w.Body.String(), `"probe"`) {
		t.Fatal("secret exposed")
	}
	if !strings.Contains(w.Body.String(), `gateway.test/mcp-servers/docs/mcp`) {
		t.Fatal("gateway URL must include the Streamable HTTP endpoint")
	}
	data, err := store.GetObject(context.Background(), "mcp-servers/docs.json")
	if err != nil || !strings.Contains(string(data), "upstream-secret") {
		t.Fatal("registry missing credentials")
	}
	if !strings.Contains(strings.Join(g.calls, ","), "PUT /v1/mcpServer") {
		t.Fatal(g.calls)
	}
}

func TestWildcardMCPApprovalValidation(t *testing.T) {
	for _, tc := range []struct {
		runtime, tools, mode string
		valid                bool
	}{
		{"codex", "*", "ask", true}, {"codex", "*", "allow", true}, {"codex", "*", "deny", false},
		{"codex", "create_branch", "allow", false}, {"codex", "*", "unknown", false}, {"qwenpaw", "*", "allow", false},
	} {
		server := v1beta1.MCPServer{Name: "docs", Approvals: &v1beta1.MCPApprovals{Tools: tc.tools, Mode: tc.mode}}
		if err := validateMCPApprovals(tc.runtime, []v1beta1.MCPServer{server}); (err == nil) != tc.valid {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

func TestManagedMCPBindOnlyRequestedWorkerAndPreservesOtherServers(t *testing.T) {
	store := ossfake.NewMemory()
	mcpPutRegistry(t, store, "docs", `{"name":"docs","managed":true,"url":"http://gateway.test/mcp-servers/docs","transport":"streaminghttp","gatewayState":"ready","authorizedWorkers":["b"],"connection":{"kind":"proxy"},"verification":{"success":true}}`)
	k := fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).WithObjects(mcpTestWorker("a", "", "other"), mcpTestWorker("b", "")).Build()
	g := &assetGateway{}
	h := &gatewayAssets{gateway: g, resources: NewResourceHandler(k, "default", nil, "", nil).WithOSS(store)}
	w := assetRequest(t, h, "mcps", `{"action":"bind","name":"docs","worker":"a"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":true`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var a, b v1beta1.Worker
	_ = k.Get(context.Background(), client.ObjectKey{Name: "a", Namespace: "default"}, &a)
	_ = k.Get(context.Background(), client.ObjectKey{Name: "b", Namespace: "default"}, &b)
	if len(a.Spec.McpServers) != 2 || len(b.Spec.McpServers) != 0 {
		t.Fatalf("a=%v b=%v", a.Spec.McpServers, b.Spec.McpServers)
	}
	if a.Spec.McpServers[1].Transport != "http" {
		t.Fatal("asset streaminghttp must map to CRD http transport")
	}
	if len(g.calls) != 1 {
		t.Fatalf("per-Worker binding touched other workers: %v", g.calls)
	}
}

func TestManagedMCPGrantFailureKeepsWorkerConfig(t *testing.T) {
	store := ossfake.NewMemory()
	mcpPutRegistry(t, store, "docs", `{"name":"docs","managed":true,"url":"http://gateway.test/mcp-servers/docs","transport":"streaminghttp","gatewayState":"ready","authorizedWorkers":[],"connection":{"kind":"proxy"},"verification":{"success":true}}`)
	k := fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).WithObjects(mcpTestWorker("a", "", "other")).Build()
	h := &gatewayAssets{gateway: &assetGateway{fail: "PUT /v1/mcpServer/consumers"}, resources: NewResourceHandler(k, "default", nil, "", nil).WithOSS(store)}
	w := assetRequest(t, h, "mcps", `{"action":"bind","name":"docs","worker":"a"}`)
	var a v1beta1.Worker
	_ = k.Get(context.Background(), client.ObjectKey{Name: "a", Namespace: "default"}, &a)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":false`) || len(a.Spec.McpServers) != 1 {
		t.Fatalf("%d %s %v", w.Code, w.Body.String(), a.Spec.McpServers)
	}
}

func TestManagedMCPRequiresVerificationBeforeGrant(t *testing.T) {
	store := ossfake.NewMemory()
	mcpPutRegistry(t, store, "docs", `{"name":"docs","managed":true,"gatewayState":"ready","authorizedWorkers":[],"connection":{"kind":"proxy"}}`)
	g := &assetGateway{}
	w := assetRequest(t, &gatewayAssets{gateway: g, resources: NewResourceHandler(nil, "default", nil, "", nil).WithOSS(store)}, "mcps", `{"action":"bind","name":"docs","worker":"a"}`)
	if w.Code != 409 || len(g.calls) > 0 {
		t.Fatalf("%d calls=%v", w.Code, g.calls)
	}
}
