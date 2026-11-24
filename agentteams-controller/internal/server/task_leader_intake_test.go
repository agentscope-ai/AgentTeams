package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func leaderIntakeFixture(t *testing.T) *ProjectHandler {
	t.Helper()
	h, _ := taskCreateFixture(t)
	var team v1.Team
	if err := h.client.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "alpha"}, &team); err != nil {
		t.Fatal(err)
	}
	team.Spec.Admin = &v1.TeamAdminSpec{Name: "human", MatrixUserID: "@human:test"}
	team.Status.AssistantRecipient = service.TeamAssistantRecipient("@human:test", "")
	if err := h.client.Update(t.Context(), &team); err != nil {
		t.Fatal(err)
	}
	human := &v1.Human{ObjectMeta: metav1.ObjectMeta{Name: "human", Namespace: "default"}, Status: v1.HumanStatus{MatrixUserID: "@human:test"}}
	if err := h.client.Create(t.Context(), human); err != nil {
		t.Fatal(err)
	}
	taskCreateSourceServer(t, h, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer leader-intake-token" {
			t.Errorf("source read used another actor's credential")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"event_id": "$confirmation", "room_id": "!leader-dm:test", "sender": "@human:test", "type": "m.room.message", "content": map[string]any{"msgtype": "m.text", "body": "Confirm analysis and a Draft PR for repository Issue #1."}})
	})
	creds := &service.FileCredentialStore{Dir: t.TempDir()}
	if err := creds.Save(t.Context(), "lead-cr", &service.WorkerCredentials{MatrixToken: "leader-intake-token"}); err != nil {
		t.Fatal(err)
	}
	h.coordination.deps.Provisioner = service.NewProvisioner(service.ProvisionerConfig{Creds: creds})
	return h
}

func leaderCreate(t *testing.T, h *ProjectHandler, role, actor, user, room, leader string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"title": "Issue #1", "spec": "Analyze, implement, independently review and test.", "leader": leader})
	r := httptest.NewRequest("POST", "/api/v1/tasks", strings.NewReader(string(body)))
	r.Header.Set("X-AgentTeams-Origin-Event-ID", "$confirmation")
	r.Header.Set("X-AgentTeams-Requester-User-ID", user)
	r.Header.Set("X-AgentTeams-Requester-Room-ID", room)
	w := httptest.NewRecorder()
	h.CreateTask(w, withCaller(r, &authpkg.CallerIdentity{Role: role, Username: actor, Team: "alpha"}))
	return w
}

func TestLeaderRegistersOwnAdminConfirmationAndReusesItsReceipt(t *testing.T) {
	h := leaderIntakeFixture(t)
	first := leaderCreate(t, h, authpkg.RoleTeamLeader, "lead-cr", "@human:test", "!leader-dm:test", "lead-cr")
	if first.Code != 201 {
		t.Fatalf("Leader intake: %d %s", first.Code, first.Body.String())
	}
	var result struct {
		Task taskResource `json:"task"`
	}
	if json.Unmarshal(first.Body.Bytes(), &result) != nil || !strings.Contains(result.Task.Spec, "Confirm analysis and a Draft PR") {
		t.Fatal("authoritative Human confirmation was lost")
	}
	if !strings.Contains(first.Body.String(), `"status":"inline"`) {
		t.Fatal("Leader creation did not admit inline planning")
	}
	key := "teams/alpha/shared/tasks/" + result.Task.ID + "/meta.json"
	meta, _ := h.coordination.read(t.Context(), key)
	if meta["task_notifications"] != nil {
		t.Fatal("inline intake queued a self-notification")
	}
	if _, err := h.oss.GetObject(t.Context(), coordinationPending+coordinationString(meta, "handoff_id")+".json"); err == nil {
		t.Fatal("inline intake created a self-handoff outbox event")
	}
	again := leaderCreate(t, h, authpkg.RoleTeamLeader, "lead-cr", "@human:test", "!leader-dm:test", "lead-cr")
	if again.Code != 200 || !strings.Contains(again.Body.String(), result.Task.ID) {
		t.Fatalf("duplicate confirmation: %d %s", again.Code, again.Body.String())
	}
}

func TestLeaderIntakeRejectsOtherSendersRoomsRolesAndDispatchTargets(t *testing.T) {
	for _, tc := range []struct{ name, role, actor, user, room, target string }{
		{"another Human", authpkg.RoleTeamLeader, "lead-cr", "@other:test", "!leader-dm:test", "lead-cr"},
		{"Agent digest", authpkg.RoleTeamLeader, "lead-cr", "@lead:test", "!leader-dm:test", "lead-cr"},
		{"another room", authpkg.RoleTeamLeader, "lead-cr", "@human:test", "!source:test", "lead-cr"},
		{"another target", authpkg.RoleTeamLeader, "lead-cr", "@human:test", "!leader-dm:test", "worker-cr"},
		{"Worker", authpkg.RoleWorker, "worker-cr", "@human:test", "!leader-dm:test", "lead-cr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := leaderIntakeFixture(t)
			response := leaderCreate(t, h, tc.role, tc.actor, tc.user, tc.room, tc.target)
			if response.Code != 403 {
				t.Fatalf("unexpected authority: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestNativeLeaderCreateUsesSourceFenceAndRejectsReplacedProcess(t *testing.T) {
	h := leaderIntakeFixture(t)
	worker, err := h.coordination.worker(t.Context(), "lead-cr")
	if err != nil {
		t.Fatal(err)
	}
	store := &service.WorkerMatrixStore{Storage: h.oss}
	if _, _, err := store.Start(t.Context(), worker.Spec.EffectiveWorkerName(worker.Name), service.WorkerStart{InstanceID: "intake-instance", ProcessID: "canonical-process", Runtime: "codex"}, ""); err != nil {
		t.Fatal(err)
	}
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"}
	args := map[string]any{"action": "create", "leader": "lead-cr", "title": "Issue #1", "_nativeSource": map[string]any{"eventId": "$confirmation", "roomId": "!leader-dm:test", "userId": "@human:test"}}
	w, first := callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
	if w.Code != 200 || first["mode"] != "execute" {
		t.Fatalf("native Leader intake: %d %s", w.Code, w.Body.String())
	}
	again, blocked := callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
	if again.Code != 200 || blocked["mode"] != "verify" {
		t.Fatal("unknown creation was replayable", blocked)
	}
	_, revision, err := store.Load(t.Context(), worker.Spec.EffectiveWorkerName(worker.Name))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Start(t.Context(), worker.Spec.EffectiveWorkerName(worker.Name), service.WorkerStart{InstanceID: "replacement", ProcessID: "different-process", Runtime: "codex"}, revision); err != nil {
		t.Fatal(err)
	}
	w, _ = callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
	if w.Code != 409 {
		t.Fatalf("old process admitted: %d %s", w.Code, w.Body.String())
	}
}

func TestLeaderIntakeReadsTheActualHumanEventBeforeRegistration(t *testing.T) {
	h := leaderIntakeFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"event_id": "$confirmation", "room_id": "!leader-dm:test", "sender": "@lead:test", "type": "m.room.message", "content": map[string]any{"msgtype": "m.text", "body": "Agent-authored digest"}})
	}))
	t.Cleanup(server.Close)
	h.coordination.deps.MatrixConfig.ServerURL = server.URL
	w := leaderCreate(t, h, authpkg.RoleTeamLeader, "lead-cr", "@human:test", "!leader-dm:test", "lead-cr")
	if w.Code != 403 {
		t.Fatalf("Agent-authored source admitted: %d %s", w.Code, w.Body.String())
	}
}

func TestLeaderIntakeCompletionReturnsThroughItsOwnDispatchIdentity(t *testing.T) {
	h := leaderIntakeFixture(t)
	response := leaderCreate(t, h, authpkg.RoleTeamLeader, "lead-cr", "@human:test", "!leader-dm:test", "lead-cr")
	if response.Code != 201 {
		t.Fatal(response.Body.String())
	}
	var result struct {
		Task taskResource `json:"task"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &result)
	key := "teams/alpha/shared/tasks/" + result.Task.ID + "/meta.json"
	task, _ := h.coordination.read(t.Context(), key)
	task["status"] = "completed"
	body, _ := json.Marshal(task)
	if err := h.oss.PutObject(t.Context(), key, body); err != nil {
		t.Fatal(err)
	}
	if err := h.oss.PutObject(t.Context(), "teams/alpha/shared/tasks/"+result.Task.ID+"/result.md", []byte("Verified final result")); err != nil {
		t.Fatal(err)
	}
	event, err := h.coordination.resolve(t.Context(), coordinationEvent{Kind: "main_task_completed", MainTaskID: result.Task.ID}, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"})
	if err != nil || event.External || event.RecipientName != "lead-cr" || event.RecipientKind != "worker" || event.FinalReport != "Verified final result" || coordinationString(event.RequesterRoute, "target_session") != "!leader-dm:test" {
		t.Fatalf("Leader completion route: %+v %v", event, err)
	}
}
