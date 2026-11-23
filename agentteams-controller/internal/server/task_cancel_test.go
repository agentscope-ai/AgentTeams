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
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func callMainCancellation(h *ProjectHandler, caller *authpkg.CallerIdentity, reason string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"reason": reason})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/main/cancel", strings.NewReader(string(body)))
	r.SetPathValue("id", "main")
	w := httptest.NewRecorder()
	h.CancelMainTask(w, withCaller(r, caller))
	return w
}

func TestMainCancellationFencesPlanAndCancelsOnlyUnfinishedExecutions(t *testing.T) {
	h, store, caller, _ := taskPlanFixture(t)
	if w := callPlan(h, caller, planRequest()); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w := callMainCancellation(h, caller, "User stopped this request")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"projection":"applied"`) {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	main, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
	if main["status"] != "cancelled" {
		t.Fatal("main not fenced")
	}
	view := publicTaskResource("main", "alpha", main)
	if view.Cancellation == nil || view.Cancellation.ID == "" || view.Cancellation.Reason != "User stopped this request" || view.CancellationProjection != "applied" {
		t.Fatal("public GET loses cancellation facts")
	}
	for _, item := range main["tasks"].([]any) {
		node := item.(map[string]any)
		if node["status"] != "cancelled" {
			t.Fatalf("node still active: %+v", node)
		}
		id := node["task_id"].(string)
		if node["step_id"] == "C" {
			continue
		} // Never dispatched; no execution record is fabricated.
		child, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+id+"/meta.json")
		if child["status"] != "cancelled" {
			t.Fatal("execution not cancelled")
		}
	}
	before, _ := store.GetObject(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
	if retry := callMainCancellation(h, caller, "User stopped this request"); retry.Code != 200 {
		t.Fatal(retry.Body.String())
	}
	after, _ := store.GetObject(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
	if string(before) != string(after) {
		t.Fatal("retry changed immutable cancellation or history")
	}
	if changed := callMainCancellation(h, caller, "Different reason"); changed.Code != 409 {
		t.Fatal("cancellation rewritten")
	}
	if retry := callPlan(h, caller, planRequest()); retry.Code != 200 || !strings.Contains(retry.Body.String(), `"progress":"stopped"`) {
		t.Fatal("cancelled plan restarted")
	}
}

func TestMainCancellationPreservesAcceptedSubmissionAndArtifacts(t *testing.T) {
	h, store, caller, submission := taskReviewFixture(t)
	if w := callTaskReview(h, caller, submission, "Accepted evidence", true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	key := "teams/alpha/shared/tasks/t1/meta.json"
	before, _ := store.GetObject(t.Context(), key)
	if w := callMainCancellation(h, caller, "Stop before final delivery"); w.Code != 200 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	after, _ := store.GetObject(t.Context(), key)
	if string(before) != string(after) {
		t.Fatal("accepted child, review or artifact registry changed")
	}
	if w := callTaskReview(h, caller, submission, "Accepted evidence", true); w.Code != 200 {
		t.Fatal("saved acceptance receipt became inaccessible")
	}
}

type cancellationChildFailure struct {
	*ossfake.Memory
	fail bool
}

func (s *cancellationChildFailure) PutObjectIfMatchStrict(ctx context.Context, key string, body []byte, etag string) error {
	if s.fail && strings.Contains(key, "/exec-") {
		return errors.New("injected child cancellation failure")
	}
	return s.Memory.PutObjectIfMatchStrict(ctx, key, body, etag)
}

func TestMainCancellationRecoversAfterCommittedFenceAndChildFailure(t *testing.T) {
	h, store, caller, _ := taskPlanFixture(t)
	if w := callPlan(h, caller, planRequest()); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	fault := &cancellationChildFailure{Memory: store, fail: true}
	h.oss = fault
	w := callMainCancellation(h, caller, "Stop")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"projection":"pending"`) {
		t.Fatalf("missing pending receipt: %d %s", w.Code, w.Body.String())
	}
	main, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
	if main["status"] != "cancelled" {
		t.Fatal("partial failure lost cancellation fence")
	}
	fault.fail = false
	h.recoverTaskNotifications(t.Context())
	w = callMainCancellation(h, caller, "Stop")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"projection":"applied"`) {
		t.Fatal(w.Body.String())
	}
}

func TestMainCancellationRejectsWorkerAndEmptyReasonWithoutMutation(t *testing.T) {
	for _, scenario := range []string{"worker", "empty", "completed"} {
		t.Run(scenario, func(t *testing.T) {
			h, store, caller, _ := taskPlanFixture(t)
			reason := "Stop"
			want := 403
			if scenario == "worker" {
				caller.Role = authpkg.RoleWorker
			}
			if scenario == "empty" {
				reason = " "
				want = 400
			}
			if scenario == "completed" {
				main, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
				main["status"] = "completed"
				putTask(store, "teams/alpha/shared/tasks/main/meta.json", main)
				want = 409
			}
			before, _ := store.GetObject(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
			w := callMainCancellation(h, caller, reason)
			if w.Code != want {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			after, _ := store.GetObject(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
			if string(before) != string(after) {
				t.Fatal("rejected request mutated task")
			}
		})
	}
}

func TestMainCancellationFencesOldWorkerWritesButPreservesPublishedFiles(t *testing.T) {
	h, store, worker, artifact := taskSubmitFixture(t)
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "operator"}
	if w := callMainCancellation(h, admin, "Stop"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, action := range []string{"begin", "submit", "files"} {
		body := `{"summary":"Late candidate","artifacts":["` + artifact + `"]}`
		if action == "begin" {
			body = `{}`
		}
		if action == "files" {
			body = "Late file"
		}
		r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/"+action+"?name=late.txt&kind=output", strings.NewReader(body))
		r.SetPathValue("id", "t1")
		r.Header.Set("X-AgentTeams-Process-ID", "process-1")
		w := httptest.NewRecorder()
		switch action {
		case "begin":
			h.BeginTask(w, withCaller(r, worker))
		case "submit":
			h.SubmitTask(w, withCaller(r, worker))
		case "files":
			h.PublishTaskFile(w, withCaller(r, worker))
		}
		if w.Code != 409 {
			t.Fatalf("late %s: %d %s", action, w.Code, w.Body.String())
		}
	}
	child, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/t1/meta.json")
	if child["task_submission"] != nil {
		t.Fatal("late candidate persisted")
	}
	index := coordinationMap(child, "task_files")
	files := coordinationMap(index, "artifacts")
	if files[artifact] == nil || len(files) != 1 {
		t.Fatal("published artifacts changed")
	}
	if _, err := store.GetObject(t.Context(), "teams/alpha/shared/tasks/t1/meta.json"); err != nil {
		t.Fatal(err)
	}
}

func TestMainCancellationReceiptCannotBeForgedThroughMetadata(t *testing.T) {
	for _, field := range []string{"task_cancellation", "task_cancellation_applied"} {
		next, _ := json.Marshal(map[string]any{field: true})
		w := httptest.NewRecorder()
		if validateTaskCancellationCommit(w, []byte(`{}`), next) || w.Code != 403 {
			t.Fatal("forged cancellation accepted")
		}
	}
}
