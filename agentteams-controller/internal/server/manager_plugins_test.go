package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestManagerPlugins(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mgr := &v1beta1.Manager{}
	mgr.Name = "default"
	mgr.Namespace = "default"
	mgr.Spec.Runtime = "qwenpaw"
	cl := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(mgr).Build()
	rh := &ResourceHandler{client: cl, namespace: "default"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"agentteams-matrix","version":"0.1.0","loaded":true,"enabled":true}]`))
	}))
	defer srv.Close()

	h := (&managerPluginsHandler{
		rh:       rh,
		kubeMode: "embedded",
		baseURL:  func(string) string { return srv.URL },
		p:        newWorkerPluginsProxy(rh, "embedded", ""),
	}).serve
	req := httptest.NewRequest(http.MethodGet, "/api/v1/managers/default/plugins", nil)
	req.SetPathValue("name", "default")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() == "" || rec.Body.String()[0] != '[' {
		t.Fatalf("expected projected array, got %s", rec.Body.String())
	}
}

func TestManagerPluginsRuntimeGuard(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1beta1.AddToScheme(scheme)
	mgr := &v1beta1.Manager{}
	mgr.Name = "default"
	mgr.Namespace = "default"
	mgr.Spec.Runtime = "openclaw"
	cl := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(mgr).Build()
	rh := &ResourceHandler{client: cl, namespace: "default"}
	h := (&managerPluginsHandler{
		rh:       rh,
		kubeMode: "embedded",
		baseURL:  func(string) string { return "http://127.0.0.1:1" },
		p:        newWorkerPluginsProxy(rh, "embedded", ""),
	}).serve
	req := httptest.NewRequest(http.MethodGet, "/api/v1/managers/default/plugins", nil)
	req.SetPathValue("name", "default")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}
