package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func policyFixture(t *testing.T) *ResourceHandler {
	t.Helper()
	t.Setenv("MODEL_PROBE_ALLOW_HOSTS", "")
	t.Setenv("MODEL_PROBE_ALLOW_PRIVATE_NETWORK", "0")
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	k := fake.NewClientBuilder().WithScheme(scheme).Build()
	return NewResourceHandler(k, "default", nil, "", nil).WithAPIReader(k)
}

func policyRequest(h *ResourceHandler, method, body string, role string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/v1/gateway/model-probe-policy", strings.NewReader(body))
	if role != "" {
		r = withCaller(r, &authpkg.CallerIdentity{Role: role})
	}
	w := httptest.NewRecorder()
	if method == "GET" {
		h.GetModelProbePolicy(w, r)
	} else {
		h.PutModelProbePolicy(w, r)
	}
	return w
}

func policyView(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("policy: %d %s", w.Code, w.Body.String())
	}
	var v map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return v
}

func TestModelProbePolicyAdminOnly(t *testing.T) {
	h := policyFixture(t)
	for _, method := range []string{"GET", "PUT"} {
		for _, role := range []string{"", authpkg.RoleManager, authpkg.RoleTeamLeader, authpkg.RoleWorker, authpkg.RoleHuman} {
			expected := 403
			if role == "" {
				expected = 401
			}
			if w := policyRequest(h, method, `{"allowedHosts":[],"allowPrivateNetwork":false}`, role); w.Code != expected {
				t.Fatalf("%s %s: %d", method, role, w.Code)
			}
		}
	}
}

func TestModelProbePolicyPersistsAndOverridesEnvironment(t *testing.T) {
	h := policyFixture(t)
	t.Setenv("MODEL_PROBE_ALLOW_HOSTS", "legacy.local")
	t.Setenv("MODEL_PROBE_ALLOW_PRIVATE_NETWORK", "1")
	initial := policyView(t, policyRequest(h, "GET", "", authpkg.RoleAdmin))
	if initial["source"] != "environment" || initial["allowPrivateNetwork"] != true {
		t.Fatal(initial)
	}
	saved := policyView(t, policyRequest(h, "PUT", `{"allowedHosts":[" GPU.LOCAL ","gpu.local","172.16.50.89"],"allowPrivateNetwork":false,"revision":""}`, authpkg.RoleAdmin))
	if saved["source"] != "persistent" || saved["revision"] == "" {
		t.Fatal(saved)
	}
	restarted := NewResourceHandler(h.client, "default", nil, "", nil).WithAPIReader(h.referenceReader())
	got := policyView(t, policyRequest(restarted, "GET", "", authpkg.RoleAdmin))
	if len(got["allowedHosts"].([]any)) != 2 || got["revision"] != saved["revision"] || got["allowPrivateNetwork"] != false {
		t.Fatal(got)
	}
	if w := policyRequest(h, "PUT", `{"allowedHosts":[],"allowPrivateNetwork":true,"revision":""}`, authpkg.RoleAdmin); w.Code != 409 {
		t.Fatalf("stale write: %d", w.Code)
	}
}

func TestModelProbePolicyChangesTakeEffectWithoutRestart(t *testing.T) {
	h := policyFixture(t)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-only" {
			t.Error("wrong probe request")
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer upstream.Close()
	input, _ := json.Marshal(map[string]any{"action": "probe", "input": map[string]string{"model": "test-model", "type": "custom", "baseUrl": upstream.URL + "/v1", "token": "test-only"}})
	gateway := &gatewayAssets{gateway: &assetGateway{}, resources: h}
	probe := func() *httptest.ResponseRecorder { return assetRequest(t, gateway, "models", string(input)) }
	if w := probe(); w.Code != 400 || calls != 0 {
		t.Fatalf("unapproved: %d calls%d", w.Code, calls)
	}
	saved := policyView(t, policyRequest(h, "PUT", `{"allowedHosts":["127.0.0.1"],"allowPrivateNetwork":false,"revision":""}`, authpkg.RoleAdmin))
	if w := probe(); w.Code != 200 || !strings.Contains(w.Body.String(), `"success":true`) || calls != 1 {
		t.Fatalf("approval ignored: %d %s calls%d", w.Code, w.Body.String(), calls)
	}
	revoke, _ := json.Marshal(map[string]any{"allowedHosts": []string{}, "allowPrivateNetwork": false, "revision": saved["revision"]})
	policyView(t, policyRequest(h, "PUT", string(revoke), authpkg.RoleAdmin))
	if w := probe(); w.Code != 400 || calls != 1 {
		t.Fatalf("revocation ignored: %d calls%d", w.Code, calls)
	}
}

func TestModelProbePolicyRejectsInvalidHosts(t *testing.T) {
	for _, host := range []string{"http://172.16.50.89/v1", "172.16.50.89:8080", "*.local", "10.0.0.0/8", "bad host", "-invalid.local"} {
		t.Run(host, func(t *testing.T) {
			h := policyFixture(t)
			body, _ := json.Marshal(map[string]any{"allowedHosts": []string{host}, "allowPrivateNetwork": false, "revision": ""})
			if w := policyRequest(h, "PUT", string(body), authpkg.RoleAdmin); w.Code != 400 {
				t.Fatalf("invalid host accepted: %d", w.Code)
			}
		})
	}
	for _, body := range []string{`{}`, `{"allowedHosts":null}`, `{"allowedHosts":[],"unknown":true}`, `{"allowedHosts":[],"allowPrivateNetwork":"true"}`} {
		h := policyFixture(t)
		if w := policyRequest(h, "PUT", body, authpkg.RoleAdmin); w.Code != 400 {
			t.Fatalf("invalid body accepted: %s: %d", body, w.Code)
		}
	}
}

func TestModelProbePolicyCorruptionFailsClosed(t *testing.T) {
	h := policyFixture(t)
	cm := &corev1.ConfigMap{}
	cm.Name, cm.Namespace, cm.Data = modelProbePolicyName, "default", map[string]string{"policy.json": "broken"}
	if err := h.client.Create(t.Context(), cm); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MODEL_PROBE_ALLOW_PRIVATE_NETWORK", "1")
	if w := policyRequest(h, "GET", "", authpkg.RoleAdmin); w.Code != 503 {
		t.Fatalf("corruption fell back: %d", w.Code)
	}
	input := `{"action":"probe","input":{"model":"test","type":"custom","token":"test","baseUrl":"http://127.0.0.1:1/v1"}}`
	if w := assetRequest(t, &gatewayAssets{gateway: &assetGateway{}, resources: h}, "models", input); w.Code != 503 {
		t.Fatalf("probe bypassed corrupt policy: %d", w.Code)
	}
}

func TestModelProbePolicyAddressSemantics(t *testing.T) {
	policy := modelProbePolicy{AllowedHosts: []string{"::1", "172.16.50.89"}}
	for _, host := range []string{"0:0:0:0:0:0:0:1", "::1", "172.16.50.89", "api.openai.com"} {
		if !policy.hostAllowed(host) {
			t.Fatalf("expected allowed: %s", host)
		}
	}
	for _, host := range []string{"localhost.", "127.0.0.1", "169.254.169.254", "other.local", "172.16.50.90"} {
		if policy.hostAllowed(host) {
			t.Fatalf("unapproved host allowed: %s", host)
		}
	}
	policy.AllowPrivateNetwork = true
	if !policy.hostAllowed("172.16.50.90") {
		t.Fatal("legacy broad private mode not preserved")
	}
}

func TestModelProbePolicyConcurrentSavesDoNotOverwrite(t *testing.T) {
	h := policyFixture(t)
	saved := policyView(t, policyRequest(h, "PUT", `{"allowedHosts":[],"allowPrivateNetwork":false,"revision":""}`, authpkg.RoleAdmin))
	statuses := make(chan int, 2)
	for _, host := range []string{"first.local", "second.local"} {
		go func(host string) {
			body, _ := json.Marshal(map[string]any{"allowedHosts": []string{host}, "allowPrivateNetwork": false, "revision": saved["revision"]})
			statuses <- policyRequest(h, "PUT", string(body), authpkg.RoleAdmin).Code
		}(host)
	}
	first, second := <-statuses, <-statuses
	if !((first == 200 && second == 409) || (first == 409 && second == 200)) {
		t.Fatalf("concurrent writes: %d %d", first, second)
	}
}

type staleModelPolicyClient struct{ client.Client }

func (c staleModelPolicyClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if _, ok := object.(*corev1.ConfigMap); ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, key.Name)
	}
	return c.Client.Get(ctx, key, object, opts...)
}

func TestModelProbePolicyUsesDirectReaderAfterSave(t *testing.T) {
	h := policyFixture(t)
	direct := h.client
	h.client = staleModelPolicyClient{Client: direct}
	h.WithAPIReader(direct)
	saved := policyView(t, policyRequest(h, "PUT", `{"allowedHosts":["gpu.local"],"allowPrivateNetwork":false,"revision":""}`, authpkg.RoleAdmin))
	view := policyView(t, policyRequest(h, "GET", "", authpkg.RoleAdmin))
	if view["revision"] != saved["revision"] || view["source"] != "persistent" {
		t.Fatal("read used stale cache", view)
	}
}

type failedModelPolicyReader struct{ client.Reader }

func (c failedModelPolicyReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("API storage unavailable")
}

func TestModelProbePolicyStorageOutageIsNotAnEnvironmentFallback(t *testing.T) {
	h := policyFixture(t)
	h.WithAPIReader(failedModelPolicyReader{Reader: h.referenceReader()})
	t.Setenv("MODEL_PROBE_ALLOW_PRIVATE_NETWORK", "1")
	for _, method := range []string{"GET", "PUT"} {
		if w := policyRequest(h, method, `{"allowedHosts":[],"allowPrivateNetwork":true,"revision":""}`, authpkg.RoleAdmin); w.Code != 503 {
			t.Fatalf("%s unavailable policy: %d", method, w.Code)
		}
	}
	input := `{"action":"probe","input":{"model":"test","token":"test","baseUrl":"http://127.0.0.1:1/v1"}}`
	if w := assetRequest(t, &gatewayAssets{gateway: &assetGateway{}, resources: h}, "models", input); w.Code != 503 {
		t.Fatalf("probe bypassed unavailable policy: %d", w.Code)
	}
}

func TestModelProbePolicyReportsResolvedBlockedHost(t *testing.T) {
	h := policyFixture(t)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer upstream.Close()
	g := &assetGateway{responses: map[string]any{"GET providers/saved": map[string]any{"name": "saved", "type": "openai", "tokens": []string{"stored-secret"}, "rawConfigs": map[string]any{"openaiCustomUrl": upstream.URL + "/v1"}}}}
	response := assetRequest(t, &gatewayAssets{gateway: g, resources: h}, "models", `{"action":"probe","input":{"providerName":"saved","model":"test","baseUrl":"https://browser-override.example/v1","token":"browser-secret"}}`)
	var body map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	if response.Code != 400 || body["code"] != "model_probe_host_not_allowed" || body["host"] != "127.0.0.1" || calls != 0 {
		t.Fatalf("wrong blocked host: %d %s calls%d", response.Code, response.Body.String(), calls)
	}
	if strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "browser-override") {
		t.Fatal("blocked metadata leaked or trusted browser overrides")
	}
}
