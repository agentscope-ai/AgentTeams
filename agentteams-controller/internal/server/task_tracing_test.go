package server

import (
	"context"
	"encoding/json"
	"fmt"
	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func TestLangfuseQueryAllowsSlowSuccessfulPage(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(11 * time.Second):
			json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		case <-r.Context().Done():
		}
	}))
	defer backend.Close()
	var result map[string]any
	if err := (langfuseQuery{LangfuseURL: backend.URL}).get(t.Context(), "observations", &result); err != nil {
		t.Fatalf("a successful page within the task query budget was rejected: %v", err)
	}
}

func TestLangfuseQueryHonorsEarlierCallerDeadline(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer backend.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	var result map[string]any
	if err := (langfuseQuery{LangfuseURL: backend.URL}).get(ctx, "observations", &result); err == nil {
		t.Fatal("a cancelled query must not return a complete result")
	}
}

func TestTracePagesReadConcurrentlyWithoutReordering(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page != "1" {
			entered <- page
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": page}}, "meta": map[string]any{"totalPages": 3}})
	}))
	defer func() { close(release); backend.Close() }()
	done := make(chan error, 1)
	go func() {
		rows, err := (langfuseQuery{LangfuseURL: backend.URL}).readPages(t.Context(), "observations", map[string][]string{})
		if err == nil && (len(rows) != 3 || str(rows[0]["id"]) != "1" || str(rows[1]["id"]) != "2" || str(rows[2]["id"]) != "3") {
			err = fmt.Errorf("page ordering changed: %v", rows)
		}
		done <- err
	}()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Error("independent observation pages were serialized")
			return
		}
	}
	// Release both requests while retaining the deferred close for failure paths.
	release <- struct{}{}
	release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

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
			if r.URL.Query().Get("fields") != "core,io" {
				t.Error("trace discovery must avoid duplicate observation and metrics aggregation")
			}
			var filters []map[string]any
			_ = json.Unmarshal([]byte(r.URL.Query().Get("filter")), &filters)
			projectFiltered := false
			for _, filter := range filters {
				if filter["column"] == "tags" {
					projectFiltered = filter["operator"] == "all of" && fmt.Sprint(filter["value"]) == "[agentteams project:p]"
				}
			}
			if !projectFiltered {
				t.Error("Task project restriction was omitted from structured filter")
			}
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
			var filters []struct {
				Value []string `json:"value"`
			}
			if err := json.Unmarshal([]byte(r.URL.Query().Get("filter")), &filters); err != nil || len(filters) != 1 {
				t.Error("missing trace batch filter")
				return
			}
			rows := []any{}
			for _, id := range filters[0].Value {
				rows = append(rows, map[string]any{
					"id": id + page, "traceId": id, "name": "read_file", "level": level, "endTime": "2026-11-01T00:00:01Z",
				})
			}
			json.NewEncoder(w).Encode(map[string]any{"data": rows, "meta": map[string]any{"totalPages": 2}})
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

func TestMainTaskTracesIncludeReplacedNodesOnlyWithAuthoritativeOwnership(t *testing.T) {
	h := tracingFixture(t)
	put := func(id string, record map[string]any) {
		t.Helper()
		body, _ := json.Marshal(record)
		if err := h.oss.PutObject(t.Context(), "teams/alpha/shared/tasks/"+id+"/meta.json", body); err != nil {
			t.Fatal(err)
		}
	}
	put("main", map[string]any{
		"task_id": "main", "task_kind": "main", "team_id": "alpha", "status": "completed",
		"tasks": []map[string]any{{"task_id": "current", "status": "completed"}},
	})
	for _, id := range []string{"retired", "wrong-main", "wrong-team", "wrong-project", "wrong-kind"} {
		child := map[string]any{"task_id": id, "main_task_id": "main", "task_kind": "execution", "team_id": "alpha", "status": "cancelled", "title": "Historical test"}
		switch id {
		case "wrong-main":
			child["main_task_id"] = "other"
		case "wrong-team":
			child["team_id"] = "other"
		case "wrong-project":
			child["project_id"] = "other"
		case "wrong-kind":
			child["task_kind"] = "main"
		}
		put(id, child)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rows := []any{}
		if r.URL.Path == "/api/public/traces" {
			for i, id := range []string{"main", "current", "retired", "wrong-main", "wrong-team", "wrong-project", "wrong-kind", "missing", "../retired"} {
				rows = append(rows, map[string]any{"id": fmt.Sprintf("%032x", i+1), "userId": "worker", "metadata": map[string]any{
					"agentteams.main_task.id": "main", "agentteams.task.id": id, "agentteams.team.id": "alpha",
				}})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": rows, "meta": map[string]any{"totalPages": 1}})
	}))
	defer backend.Close()
	saveTraceBackend(t, h, backend.URL)
	request := withCaller(httptest.NewRequest("GET", "/api/v1/tasks/main/traces?team=alpha", nil), &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Team: "alpha", Username: "leader"})
	request.SetPathValue("id", "main")
	response := httptest.NewRecorder()
	h.GetMainTaskTraces(response, request)
	var bundle taskTraceBundle
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &bundle) != nil {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	if len(bundle.Traces) != 3 || len(bundle.Tasks) != 3 {
		t.Fatalf("historical node missing or foreign node admitted: traces=%d tasks=%d", len(bundle.Traces), len(bundle.Tasks))
	}
	if bundle.Traces[2].TaskID != "retired" || bundle.Tasks[2].Status != "cancelled" {
		t.Fatalf("historical identity/status not preserved: %+v", bundle.Tasks)
	}
}

func TestMainTaskTracesWithoutProjectIncludesExecutionNodes(t *testing.T) {
	h := tracingFixture(t)
	body, _ := json.Marshal(map[string]any{
		"task_id": "main", "task_kind": "main", "status": "in_progress", "team_id": "alpha", "room_id": "!main:test",
		"tasks": []map[string]any{{"task_id": "exec-one", "status": "completed"}, {"task_id": "exec-two", "status": "in_progress", "depends_on": []string{"exec-one"}}},
	})
	if err := h.oss.PutObject(t.Context(), "teams/alpha/shared/tasks/main/meta.json", body); err != nil {
		t.Fatal(err)
	}
	calls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Has("fromTimestamp") {
			t.Error("main Task query omitted old rounds")
		}
		if r.URL.Path == "/api/public/traces" {
			if !strings.Contains(r.URL.Query().Get("filter"), "main_task:main") {
				t.Error("main Task filter missing")
			}
			rows := []any{}
			for i, task := range []string{"main", "exec-one", "exec-two", "unrelated"} {
				rows = append(rows, map[string]any{"id": fmt.Sprintf("%032x", i+1), "userId": "worker",
					"metadata": map[string]any{"agentteams.main_task.id": "main", "agentteams.task.id": task, "agentteams.team.id": "alpha"}})
			}
			rows = append(rows, map[string]any{"id": strings.Repeat("e", 32), "userId": "worker", "metadata": map[string]any{"agentteams.room.id": "!main:test", "agentteams.team.id": "alpha"}})
			rows = append(rows, map[string]any{"id": strings.Repeat("f", 32), "userId": "worker", "metadata": map[string]any{"agentteams.main_task.id": "foreign-main", "agentteams.room.id": "!main:test", "agentteams.team.id": "alpha"}})
			json.NewEncoder(w).Encode(map[string]any{"data": rows, "meta": map[string]any{"totalPages": 1}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []any{}, "meta": map[string]any{"totalPages": 1}})
	}))
	defer backend.Close()
	saveTraceBackend(t, h, backend.URL)
	request := httptest.NewRequest("GET", "/api/v1/tasks/main/traces?team=alpha", nil)
	request.SetPathValue("id", "main")
	request = withCaller(request, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Team: "alpha", Username: "leader"})
	response := httptest.NewRecorder()
	h.GetMainTaskTraces(response, request)
	if response.Code != 200 {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	var bundle taskTraceBundle
	if err := json.Unmarshal(response.Body.Bytes(), &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.ProjectID != "" || bundle.TaskID != "main" || len(bundle.Traces) != 4 || len(bundle.Tasks) != 3 || !bundle.RetrievalComplete {
		t.Fatalf("unexpected main Task bundle: %+v", bundle)
	}
	roomCorrelated := false
	for _, entry := range bundle.Traces {
		if entry.Trace["id"] == strings.Repeat("e", 32) {
			roomCorrelated = entry.TaskID == "main" && entry.Correlation == "task_room"
		}
	}
	if !roomCorrelated {
		t.Fatal("missing authoritative Task-room correlation")
	}
	before := calls
	denied := httptest.NewRecorder()
	request = withCaller(request, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Team: "other", Username: "foreign"})
	h.GetMainTaskTraces(denied, request)
	if denied.Code == 200 || calls != before {
		t.Fatalf("foreign Task reached backend: %d", denied.Code)
	}
}

func TestTraceObservationBatchesPreserveAllPagesAndRejectForeignRows(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		var calls atomic.Int32
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			var filters []struct {
				Value []string `json:"value"`
			}
			if err := json.Unmarshal([]byte(r.URL.Query().Get("filter")), &filters); err != nil || len(filters) != 1 {
				t.Error("missing trace filter")
				return
			}
			ids := filters[0].Value
			if len(ids) > 100 {
				t.Error("unbounded trace batch")
			}
			midpoint := len(ids) / 2
			if r.URL.Query().Get("page") == "1" {
				ids = ids[:midpoint]
			} else {
				ids = ids[midpoint:]
			}
			rows := []any{}
			for _, id := range ids {
				if foreign {
					id = "outside-scope"
				}
				rows = append(rows, map[string]any{"id": id, "traceId": id})
			}
			json.NewEncoder(w).Encode(map[string]any{"data": rows, "meta": map[string]any{"totalPages": 2}})
		}))
		ids := []string{}
		for i := 0; i < 250; i++ {
			ids = append(ids, fmt.Sprintf("%032x", i+1))
		}
		result, err := (langfuseQuery{LangfuseURL: backend.URL}).readTraceObservations(t.Context(), ids)
		backend.Close()
		if foreign {
			if err == nil || result != nil {
				t.Fatal("foreign observation accepted")
			}
			continue
		}
		if err != nil || len(result) != 250 || calls.Load() != 6 {
			t.Fatalf("lost batch or page: traces=%d calls=%d err=%v", len(result), calls.Load(), err)
		}
		for _, id := range ids {
			if len(result[id]) != 1 {
				t.Fatalf("missing observation for %s", id)
			}
		}
	}
}
