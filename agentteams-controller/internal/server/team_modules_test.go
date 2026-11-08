package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestTeamModulesAggregation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	team := &v1beta1.Team{}
	team.Name = "alpha"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "w1", Role: "worker"},
		{Name: "w2", Role: "worker"},
		{Name: "missing", Role: "worker"},
	}
	w1 := &v1beta1.Worker{}
	w1.Name = "w1"
	w1.Namespace = "default"
	w2 := &v1beta1.Worker{}
	w2.Name = "w2"
	w2.Namespace = "default"

	cl := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(team, w1, w2).Build()
	rh := &ResourceHandler{client: cl, namespace: "default"}

	w1srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"teamharness","name":"TeamHarness","version":"0.1.0","loaded":true}]`))
	}))
	defer w1srv.Close()
	w2srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"teamharness","name":"TeamHarness","version":"0.2.0","loaded":true}]`))
	}))
	defer w2srv.Close()

	p := newWorkerPluginsProxy(rh, "embedded", "unused-")
	p.workerBaseURL = func(name string, _ map[string]string) string {
		switch name {
		case "w1":
			return w1srv.URL
		case "w2":
			return w2srv.URL
		default:
			return "http://127.0.0.1:1"
		}
	}
	h := rh.getTeamModulesWithProxy(p, "embedded")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/teams/alpha/modules", nil)
	req.SetPathValue("name", "alpha")
	rec := httptest.NewRecorder()
	h(rec, req.WithContext(context.Background()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out teamModulesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Team != "alpha" || len(out.Modules) != 1 {
		t.Fatalf("unexpected response: %+v", out)
	}
	m := out.Modules[0]
	if m.Module != "teamharness" || m.Consistent {
		t.Fatalf("expected inconsistent teamharness, got %+v", m)
	}
	if m.Versions["0.1.0"] != 1 || m.Versions["0.2.0"] != 1 {
		t.Fatalf("versions=%v", m.Versions)
	}
	if len(out.Errors) != 1 || out.Errors[0].Worker != "missing" {
		t.Fatalf("errors=%v", out.Errors)
	}
}

func TestTeamModulesKubeMode(t *testing.T) {
	rh := &ResourceHandler{}
	p := newWorkerPluginsProxy(rh, "kubernetes", "")
	h := rh.getTeamModulesWithProxy(p, "kubernetes")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/teams/alpha/modules", nil)
	req.SetPathValue("name", "alpha")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestTeamModulesInconsistentWhenMemberMissingModule(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	team := &v1beta1.Team{}
	team.Name = "alpha"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "w1", Role: "worker"}, {Name: "w2", Role: "worker"}}
	w1 := &v1beta1.Worker{}
	w1.Name = "w1"
	w1.Namespace = "default"
	w2 := &v1beta1.Worker{}
	w2.Name = "w2"
	w2.Namespace = "default"
	cl := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(team, w1, w2).Build()
	rh := &ResourceHandler{client: cl, namespace: "default"}

	w1srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"teamharness","version":"0.1.0","loaded":true}]`))
	}))
	defer w1srv.Close()
	w2srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer w2srv.Close()
	p := newWorkerPluginsProxy(rh, "embedded", "unused-")
	p.workerBaseURL = func(name string, _ map[string]string) string {
		if name == "w1" {
			return w1srv.URL
		}
		return w2srv.URL
	}
	h := rh.getTeamModulesWithProxy(p, "embedded")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/teams/alpha/modules", nil)
	req.SetPathValue("name", "alpha")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var out teamModulesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Modules) != 1 || out.Modules[0].Consistent {
		t.Fatalf("expected inconsistent module, got %+v", out)
	}
}
