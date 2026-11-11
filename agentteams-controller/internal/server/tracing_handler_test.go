package server

import (
	"encoding/json"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTracingAPIUsesRuntimeIdentityAndRejectsForeignRows(t *testing.T) {
	h := tracingFixture(t)
	id := strings.Repeat("a", 32)
	foreign := strings.Repeat("b", 32)
	observationReads := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		row := func(id, worker string) map[string]any {
			return map[string]any{"id": id, "userId": worker, "sessionId": "session", "tags": []string{"team:alpha", "task:work", "project:p"}, "metadata": map[string]any{"agentteams.team.id": "alpha", "agentteams.project.id": "p", "agentteams.task.id": "work"}}
		}
		if r.URL.Path == "/api/public/traces" {
			if r.URL.Query().Get("userId") != "worker" {
				t.Error("CR identity was not translated to runtime identity")
			}
			if r.URL.Query().Has("fromTimestamp") {
				t.Error("whole-session query used a time cutoff")
			}
			json.NewEncoder(w).Encode(map[string]any{"data": []any{row(id, "worker"), row(foreign, "foreign")}, "meta": map[string]any{"totalPages": 1}})
		} else if r.URL.Path == "/api/public/traces/"+foreign {
			json.NewEncoder(w).Encode(row(foreign, "foreign"))
		} else if r.URL.Path == "/api/public/observations" {
			observationReads++
			json.NewEncoder(w).Encode(map[string]any{"data": []any{}, "meta": map[string]any{"totalPages": 0}})
		} else {
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	saveTraceBackend(t, h, backend.URL)
	r := taskTraceRequest("alpha")
	r.URL, _ = r.URL.Parse("/api/v1/traces?worker=worker-cr&session=session")
	rec := httptest.NewRecorder()
	h.ListTraces(rec, r)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), foreign) || !strings.Contains(rec.Body.String(), `"totalItems":1`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	r.SetPathValue("traceId", foreign)
	rec = httptest.NewRecorder()
	h.GetTrace(rec, r)
	if rec.Code != 404 || observationReads != 0 {
		t.Fatalf("foreign trace content was fetched: %d reads=%d", rec.Code, observationReads)
	}
}

func TestTraceIdentityConflictsCannotBecomeStandalone(t *testing.T) {
	row := map[string]any{"userId": "worker", "metadata": map[string]any{"agentteams.team.id": "alpha"}, "tags": []any{"team:other"}}
	if traceIsVisible(row, map[string]traceWorkerAccess{"worker": {RuntimeName: "worker", Teams: map[string]bool{"": true}}}) {
		t.Fatal("conflicting team identity was treated as standalone")
	}
}

func TestTraceWorkerScopePreservesExplicitHumanAssignments(t *testing.T) {
	h := tracingFixture(t)
	scope, err := h.traceWorkers(t.Context(), &authpkg.CallerIdentity{Role: authpkg.RoleHuman, AccessibleWorkers: []string{"worker-cr"}})
	if err != nil || scope["worker"].Teams == nil || scope["foreign"].Teams != nil {
		t.Fatalf("incorrect explicit scope: %+v %v", scope, err)
	}
}
