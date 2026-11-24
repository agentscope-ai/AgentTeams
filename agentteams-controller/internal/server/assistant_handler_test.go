package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"k8s.io/apimachinery/pkg/types"
)

func TestAssistantDeliverySurvivesRetryAndLeaderReplacement(t *testing.T) {
	coord, _ := coordinationFixture(t)
	var team v1.Team
	key := types.NamespacedName{Namespace: "default", Name: "alpha"}
	if err := coord.deps.Client.Get(context.Background(), key, &team); err != nil {
		t.Fatal(err)
	}
	team.Spec.Admin = &v1.TeamAdminSpec{Name: "alice", MatrixUserID: "@alice:test"}
	team.Status.LeaderDMRoomID = "!assistant:test"
	if err := coord.deps.Client.Update(context.Background(), &team); err != nil {
		t.Fatal(err)
	}
	h := NewAssistantHandler(coord.deps)
	sends := 0
	h.send = func(_ context.Context, role, name, room, txn string, content map[string]any) (string, error) {
		sends++
		if room != "!assistant:test" || content["org.agentteams.assistant_reminder"] == nil {
			t.Fatal("wrong route or missing reminder marker")
		}
		return "$digest:test", nil
	}
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"}
	request := func(method, payload string) *httptest.ResponseRecorder {
		r := withCaller(httptest.NewRequest(method, "/api/v1/assistant?team=alpha", strings.NewReader(payload)), caller)
		w := httptest.NewRecorder()
		if method == "GET" {
			h.Context(w, r)
		} else {
			h.Send(w, r)
		}
		return w
	}
	w := request("GET", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var view map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	owner := view["owner"].(string)
	message, _ := json.Marshal(map[string]any{"owner": owner, "requestId": "daily-20261121", "text": "团队今日摘要", "title": "研发团队助手", "summary": "关注团队协作与 Nacos"})
	if w = request("POST", strings.Replace(string(message), owner, owner+":stale", 1)); w.Code != 409 || sends != 0 {
		t.Fatal("stale executor was allowed to deliver", w.Code)
	}
	for range 2 {
		w = request("POST", string(message))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "$digest:test") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if sends != 1 {
		t.Fatal("retry sent twice", sends)
	}
	// Restarting the handler must reuse the durable receipt.
	h = NewAssistantHandler(coord.deps)
	h.send = func(context.Context, string, string, string, string, map[string]any) (string, error) {
		t.Fatal("restart resent message")
		return "", nil
	}
	if w = request("POST", string(message)); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	changed := strings.Replace(string(message), "团队今日摘要", "不同内容", 1)
	if w = request("POST", changed); w.Code != 409 {
		t.Fatal("payload replacement accepted", w.Code)
	}
	// Replacing the role holder rejects the old Leader even if its token is cached.
	if err := coord.deps.Client.Get(context.Background(), key, &team); err != nil {
		t.Fatal(err)
	}
	team.Spec.WorkerMembers[0].Name = "worker-cr"
	if err := coord.deps.Client.Update(context.Background(), &team); err != nil {
		t.Fatal(err)
	}
	if w = request("POST", string(message)); w.Code != 403 {
		t.Fatal("old Leader retained write permission", w.Code)
	}
	caller.Username = "worker-cr"
	w = request("GET", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "关注团队协作与 Nacos") {
		t.Fatal("role context lost on handoff", w.Code, w.Body.String())
	}
}

func TestAssistantRejectsOrdinaryWorker(t *testing.T) {
	coord, _ := coordinationFixture(t)
	h := NewAssistantHandler(coord.deps)
	r := withCaller(httptest.NewRequest("GET", "/api/v1/assistant?team=alpha", nil), &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "worker-cr", Team: "alpha"})
	w := httptest.NewRecorder()
	h.Context(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}
