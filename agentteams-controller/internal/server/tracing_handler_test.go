package server

import (
	"encoding/json"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTraceQueryRejectsInvalidBoundsAndPaging(t *testing.T) {
	h := tracingFixture(t)
	for _, query := range []string{"since=yesterday", "since=2026-11-11T08:00:00", "until=bad", "since=2026-11-12T00:00:00Z&until=2026-11-11T00:00:00Z", "limit=0", "limit=101", "worker=one&agent=two"} {
		r := taskTraceRequest("alpha")
		r.URL, _ = r.URL.Parse("/api/v1/traces?" + query)
		rec := httptest.NewRecorder()
		h.ListTraces(rec, r)
		if rec.Code != 400 {
			t.Fatalf("query %s: expected 400, got %d", query, rec.Code)
		}
	}
}

func TestTraceQueryBoundsAndLimitPreserveScope(t *testing.T) {
	h := tracingFixture(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("fromTimestamp") != "2026-11-11T00:00:00Z" || q.Get("toTimestamp") != "2026-11-11T02:00:00Z" || q.Get("userId") != "worker" {
			t.Errorf("wrong backend query: %v", q)
		}
		rows := []map[string]any{}
		for _, id := range []string{"first", "second", "outside", "foreign"} {
			worker, at := "worker", "2026-11-11T01:00:00Z"
			if id == "foreign" {
				worker = "foreign"
			}
			if id == "outside" {
				at = "2026-11-11T03:00:00Z"
			}
			rows = append(rows, map[string]any{"id": id, "userId": worker, "timestamp": at, "tags": []string{"team:alpha"}, "metadata": map[string]any{"agentteams.team.id": "alpha"}})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": rows, "meta": map[string]any{"totalPages": 1}})
	}))
	defer backend.Close()
	saveTraceBackend(t, h, backend.URL)
	r := taskTraceRequest("alpha")
	r.URL, _ = r.URL.Parse("/api/v1/traces?agent=worker-cr&since=2026-11-11T00:00:00Z&until=2026-11-11T02:00:00Z&limit=1&page=2")
	rec := httptest.NewRecorder()
	h.ListTraces(rec, r)
	var result struct {
		Data []struct{ ID string }
		Meta struct{ TotalItems, TotalPages int }
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &result) != nil || len(result.Data) != 1 || result.Data[0].ID != "second" || result.Meta.TotalItems != 2 || result.Meta.TotalPages != 2 {
		t.Fatalf("incorrect filtered page: %d %s", rec.Code, rec.Body.String())
	}
}

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
