package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func getAttention(t *testing.T, h *ProjectHandler, projectID, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/projects/"+projectID+"/attention"+query,
		nil,
	)
	req.SetPathValue("id", projectID)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectAttention(rec, req)
	return rec
}

func decodeAttention(t *testing.T, rec *httptest.ResponseRecorder) attentionResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp attentionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

// seedAttentionProject: team-scoped project p1 with two tasks (t1/t2), plus
// the task metas. Neither task carries an attention list yet — tests add
// them.
func seedAttentionProject(t *testing.T) (*ossfake.Memory, *ProjectHandler) {
	t.Helper()
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active",
		"plan_type": "dag", "team_id": "alpha-team",
		"tasks": []any{
			map[string]any{"task_id": "t1", "title": "Task One", "status": "active"},
			map[string]any{"task_id": "t2", "title": "Task Two", "status": "planned"},
		},
	})
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "title": "Task One", "status": "active",
	})
	putProject(store, "teams/alpha-team/shared/tasks/t2/meta.json", map[string]any{
		"task_id": "t2", "project_id": "p1", "title": "Task Two", "status": "planned",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))
	return store, h
}

func TestGetProjectAttention_UnresolvedFilter(t *testing.T) {
	store, h := seedAttentionProject(t)
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "title": "Task One", "status": "active",
		"attention": []any{
			map[string]any{"kind": "approval", "question": "Proceed with deploy?", "attempt": 1,
				"requestedAt": "2026-10-25T09:00:00Z", "resolved": false, "eventId": "ev_a1"},
			map[string]any{"kind": "decision", "question": "Which branch?", "attempt": 1,
				"requestedAt": "2026-10-25T08:00:00Z", "resolved": true, "eventId": "ev_d1"},
		},
	})

	resp := decodeAttention(t, getAttention(t, h, "p1", ""))
	if resp.Count != 1 || resp.Items[0].Kind != "approval" || resp.Items[0].Resolved {
		t.Fatalf("default must return unresolved only: %+v", resp.Items)
	}
	all := decodeAttention(t, getAttention(t, h, "p1", "?include_resolved=1"))
	if all.Count != 2 {
		t.Fatalf("include_resolved=1 must return all: %+v", all.Items)
	}
}

func TestGetProjectAttention_AggregationAndSort(t *testing.T) {
	store, h := seedAttentionProject(t)
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "title": "Task One", "status": "active",
		"attention": []any{
			map[string]any{"kind": "decision", "question": "branch?", "attempt": 2,
				"requestedAt": "2026-10-25T10:00:00Z", "resolved": false},
			map[string]any{"kind": "approval", "question": "deploy?", "attempt": 1,
				"requestedAt": "2026-10-25T10:00:00Z", "resolved": false},
		},
	})
	putProject(store, "teams/alpha-team/shared/tasks/t2/meta.json", map[string]any{
		"task_id": "t2", "project_id": "p1", "title": "Task Two", "status": "planned",
		"attention": []any{
			map[string]any{"kind": "escalation", "question": "blocked", "attempt": 1,
				"requestedAt": "2026-10-25T11:00:00Z", "resolved": false},
		},
	})
	// Foreign project + task in the same team: must not leak into p1.
	putProject(store, "teams/alpha-team/shared/projects/p2/meta.json", map[string]any{
		"project_id": "p2", "title": "Beta", "status": "active",
		"plan_type": "dag", "team_id": "alpha-team",
		"tasks": []any{map[string]any{"task_id": "t3", "title": "Task Three", "status": "active"}},
	})
	putProject(store, "teams/alpha-team/shared/tasks/t3/meta.json", map[string]any{
		"task_id": "t3", "project_id": "p2", "title": "Task Three", "status": "active",
		"attention": []any{
			map[string]any{"kind": "approval", "question": "foreign", "attempt": 1,
				"requestedAt": "2026-10-25T12:00:00Z", "resolved": false},
		},
	})

	resp := decodeAttention(t, getAttention(t, h, "p1", ""))
	if resp.Count != 3 {
		t.Fatalf("count=%d want 3 (no foreign leak): %+v", resp.Count, resp.Items)
	}
	// Newest first; the 10:00 tie on t1 breaks by kind (approval < decision).
	want := []struct {
		taskID, kind string
		attempt      int
	}{
		{"t2", "escalation", 1},
		{"t1", "approval", 1},
		{"t1", "decision", 2},
	}
	for i, w := range want {
		got := resp.Items[i]
		if got.TaskID != w.taskID || got.Kind != w.kind || got.Attempt != w.attempt {
			t.Fatalf("position %d = %+v, want %+v", i, got, w)
		}
	}
}

func TestGetProjectAttention_MissingTaskMetaSkipped(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active",
		"plan_type": "dag", "team_id": "alpha-team",
		"tasks": []any{
			map[string]any{"task_id": "t1", "title": "Task One", "status": "active"},
			map[string]any{"task_id": "t-gone", "title": "Gone", "status": "active"},
		},
	})
	// t1 meta without title: the item must fall back to the graph node title.
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "active",
		"attention": []any{
			map[string]any{"kind": "approval", "question": "q", "attempt": 1,
				"requestedAt": "2026-10-25T09:00:00Z", "resolved": false},
		},
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	resp := decodeAttention(t, getAttention(t, h, "p1", ""))
	if resp.Count != 1 || resp.Items[0].TaskID != "t1" {
		t.Fatalf("missing task meta must be skipped: %+v", resp.Items)
	}
	if resp.Items[0].TaskTitle != "Task One" || resp.Items[0].TaskStatus != "active" {
		t.Fatalf("node title/status fallback: %+v", resp.Items[0])
	}
}

func TestGetProjectAttention_EmptyItemsNotNull(t *testing.T) {
	_, h := seedAttentionProject(t) // tasks with no attention list
	rec := getAttention(t, h, "p1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("empty items must serialize as [] not null: %s", rec.Body.String())
	}
}

func TestGetProjectAttention_UnknownProject404(t *testing.T) {
	_, h := seedAttentionProject(t)
	rec := getAttention(t, h, "p-missing", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetProjectAttention_Ambiguous409(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active",
		"plan_type": "dag", "team_id": "alpha-team", "tasks": []any{},
	})
	putProject(store, "teams/beta-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Beta", "status": "active",
		"plan_type": "dag", "team_id": "beta-team", "tasks": []any{},
	})
	h := newProjectTestHandler(t, store, team("alpha-team"), team("beta-team"))

	rec := getAttention(t, h, "p1", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d want 409 body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetProjectAttention_InvalidLimit400(t *testing.T) {
	_, h := seedAttentionProject(t)
	for _, q := range []string{"?limit=0", "?limit=-3", "?limit=abc"} {
		rec := getAttention(t, h, "p1", q)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query=%q status=%d want 400 body=%s", q, rec.Code, rec.Body.String())
		}
	}
}

func TestGetProjectAttention_OptionsPassthrough(t *testing.T) {
	store, h := seedAttentionProject(t)
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "title": "Task One", "status": "active",
		"attention": []any{
			map[string]any{
				"kind": "approval", "question": "Proceed?", "attempt": 1,
				"requestedAt": "2026-10-25T09:00:00Z", "resolved": false,
				"eventId": "ev_opt",
				"options": []any{
					map[string]any{"id": "proceed_once", "label": "Proceed once"},
					map[string]any{"id": "reject", "label": "Reject"},
				},
				"suggested": "proceed_once",
				"expiresAt": "2026-10-25T12:00:00Z",
			},
			map[string]any{"kind": "decision", "question": "q", "attempt": 1,
				"requestedAt": "2026-10-25T08:00:00Z", "resolved": false},
		},
	})

	rec := getAttention(t, h, "p1", "")
	resp := decodeAttention(t, rec)
	if resp.Count != 2 {
		t.Fatalf("count=%d want 2: %+v", resp.Count, resp.Items)
	}
	full := resp.Items[0] // 09:00 sorts first
	if full.EventID != "ev_opt" || full.Suggested != "proceed_once" ||
		full.ExpiresAt != "2026-10-25T12:00:00Z" || len(full.Options) != 2 ||
		full.Options[0].ID != "proceed_once" || full.Options[0].Label != "Proceed once" ||
		full.Options[1].ID != "reject" || full.Options[1].Label != "Reject" {
		t.Fatalf("optional fields not passed through: %+v", full)
	}
	if bare := resp.Items[1]; bare.EventID != "" || bare.Options != nil ||
		bare.Suggested != "" || bare.ExpiresAt != "" {
		t.Fatalf("absent optional fields must be empty: %+v", bare)
	}

	// And omitted from the wire entirely (omitempty).
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("wire decode: %v", err)
	}
	items := raw["items"].([]any)
	bare := items[1].(map[string]any)
	for _, k := range []string{"event_id", "options", "suggested", "expires_at"} {
		if _, present := bare[k]; present {
			t.Fatalf("absent field %q must be omitted from wire: %v", k, bare)
		}
	}
}

func TestGetProjectAttention_LoopTasks(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/pl1/meta.json", map[string]any{
		"project_id": "pl1", "title": "Loop", "status": "active",
		"plan_type": "loop", "team_id": "alpha-team",
		"tasks": []any{},
		"loop": map[string]any{
			"goal": "g", "stop_condition": "s", "current_iteration": 1,
			"max_iterations": 5, "status": "running",
			"tasks": []any{map[string]any{"task_id": "it1", "title": "Iter 1", "status": "active"}},
		},
	})
	putProject(store, "teams/alpha-team/shared/tasks/it1/meta.json", map[string]any{
		"task_id": "it1", "project_id": "pl1", "title": "Iter 1", "status": "active",
		"attention": []any{
			map[string]any{"kind": "decision", "question": "keep going?", "attempt": 1,
				"requestedAt": "2026-10-25T09:00:00Z", "resolved": false},
		},
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	resp := decodeAttention(t, getAttention(t, h, "pl1", ""))
	if resp.Count != 1 || resp.Items[0].TaskID != "it1" ||
		resp.Items[0].TaskTitle != "Iter 1" {
		t.Fatalf("loop task source: %+v", resp.Items)
	}
}
