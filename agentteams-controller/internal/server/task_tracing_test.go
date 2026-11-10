package server

import (
	"encoding/json"
	"fmt"
	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func tracingFixture(t *testing.T) *ProjectHandler {
	t.Helper()
	store := ossfake.NewMemory()
	for _, name := range []string{"alpha", "other"} {
		putProject(store, "teams/"+name+"/shared/projects/p/meta.json", map[string]any{
			"project_id": "p", "team_id": name, "plan_type": "dag", "tasks": []map[string]any{
				{"task_id": "up", "status": "completed"},
				{"task_id": "work", "status": "in_progress", "depends_on": []string{"up"}},
				{"task_id": "next", "status": "planned", "depends_on": []string{"work"}},
			},
		})
	}
	alpha := team("alpha")
	alpha.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "worker-cr"}}
	other := team("other")
	other.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "foreign"}}
	h := newProjectTestHandler(t, store, alpha, other)
	if err := corev1.AddToScheme(h.client.Scheme()); err != nil {
		t.Fatal(err)
	}
	for _, worker := range []*v1beta1.Worker{
		{ObjectMeta: metav1.ObjectMeta{Name: "worker-cr", Namespace: "default"}, Spec: v1beta1.WorkerSpec{WorkerName: "worker", Runtime: "qwenpaw"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "default"}},
	} {
		if err := h.client.Create(t.Context(), worker); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func taskTraceRequest(team string) *http.Request {
	r := httptest.NewRequest("GET", "/api/v1/projects/p/tasks/work/traces?team="+team+"&includeRelated=true", nil)
	r.SetPathValue("id", "p")
	r.SetPathValue("taskId", "work")
	return withCaller(r, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Team: "alpha", Username: "leader"})
}

func saveTraceBackend(t *testing.T, h *ProjectHandler, base string) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"backend": "langfuse", "langfuseUrl": base, "publicKey": "pk", "secretKey": "secret", "revision": "test"})
	if err := h.client.Create(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: observabilitySecretName, Namespace: "default"}, Data: map[string][]byte{"config.json": data}}); err != nil {
		t.Fatal(err)
	}
}

func TestTaskTracesAllPagesAndRelatedEvidence(t *testing.T) {
	h := tracingFixture(t)
	ids := []string{strings.Repeat("a", 32), strings.Repeat("b", 32)}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, _ := r.BasicAuth()
		if user != "pk" || password != "secret" {
			t.Error("missing backend credentials")
		}
		if r.URL.Query().Has("fromTimestamp") {
			t.Error("task queries must include old rounds")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/public/traces" {
			page := r.URL.Query().Get("page")
			index := 0
			if page == "2" {
				index = 1
			}
			task := []string{"up", "work"}[index]
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{
				"id": ids[index], "userId": "worker", "tags": []string{"agentteams", "team:alpha", "project:p", "task:" + task},
				"metadata": map[string]any{"agentteams.team.id": "alpha", "agentteams.project.id": "p", "agentteams.task.id": task},
			}}, "meta": map[string]any{"totalPages": 2}})
			return
		}
		if r.URL.Path == "/api/public/observations" {
			page := r.URL.Query().Get("page")
			level := "DEFAULT"
			if page == "2" {
				level = "ERROR"
			}
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{
				"id": r.URL.Query().Get("traceId") + page, "traceId": r.URL.Query().Get("traceId"),
				"name": "read_file", "level": level, "endTime": "2026-11-01T00:00:01Z",
			}}, "meta": map[string]any{"totalPages": 2}})
			return
		}
		http.NotFound(w, r)
	}))
	defer backend.Close()
	saveTraceBackend(t, h, backend.URL)
	rec := httptest.NewRecorder()
	h.GetTaskTraces(rec, taskTraceRequest("alpha"))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result taskTraceBundle
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.RetrievalComplete || len(result.Traces) != 2 || len(result.Evidence) != 2 {
		t.Fatalf("incomplete bundle: %s", rec.Body.String())
	}
	for _, trace := range result.Traces {
		if len(trace.Observations) != 2 {
			t.Fatalf("observation pagination lost: %+v", trace)
		}
	}
	if !strings.Contains(rec.Body.String(), "next") {
		t.Fatal("missing task without trace")
	}
}

func TestTaskTracesRejectScopeBeforeBackend(t *testing.T) {
	h := tracingFixture(t)
	for _, name := range []string{"other", "missing"} {
		rec := httptest.NewRecorder()
		h.GetTaskTraces(rec, taskTraceRequest(name))
		if rec.Code != 404 {
			t.Fatalf("team=%s code=%d body=%s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestTaskTracesBackendFailureNeverLooksComplete(t *testing.T) {
	h := tracingFixture(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "secret backend failure", 500) }))
	defer backend.Close()
	saveTraceBackend(t, h, backend.URL)
	rec := httptest.NewRecorder()
	h.GetTaskTraces(rec, taskTraceRequest("alpha"))
	if rec.Code != 502 || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestListTeamTasksScoped(t *testing.T) {
	h := tracingFixture(t)
	for _, name := range []string{"alpha", "other"} {
		r := withCaller(httptest.NewRequest("GET", "/api/v1/tasks?team="+name, nil), &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Team: "alpha"})
		rec := httptest.NewRecorder()
		h.ListTasks(rec, r)
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Tasks []json.RawMessage `json:"tasks"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		want := 0
		if name == "alpha" {
			want = 3
		}
		if len(body.Tasks) != want {
			t.Fatal(fmt.Sprintf("team=%s body=%s", name, rec.Body.String()))
		}
	}
}
