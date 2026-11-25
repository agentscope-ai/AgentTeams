package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAssistantBusinessRequiresSummaryWithoutErasingSavedBinding(t *testing.T) {
	coord, caller, route := assistantFixture(t, "qwenpaw")
	h := coord.assistant
	write := map[string]any{"owner": route.owner, "revision": "", "summary": "仓库 example/repo"}
	w := assistantCall(h, caller, "PUT", "business?team=alpha", write)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var saved map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &saved)
	for _, payload := range []map[string]any{
		{"owner": route.owner, "revision": saved["revision"], "repository": "example/other"},
		{"owner": route.owner, "revision": saved["revision"], "summary": nil},
	} {
		if w = assistantCall(h, caller, "PUT", "business?team=alpha", payload); w.Code != 400 {
			t.Fatal("malformed binding must fail", w.Code, w.Body.String())
		}
	}
	w = assistantCall(h, caller, "GET", "business?team=alpha", nil)
	var current map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &current)
	if current["summary"] != saved["summary"] || current["revision"] != saved["revision"] {
		t.Fatal("rejected input changed the saved binding", current)
	}
	write["revision"], write["summary"] = current["revision"], ""
	if w = assistantCall(h, caller, "PUT", "business?team=alpha", write); w.Code != 200 {
		t.Fatal("explicit clear must remain available", w.Code, w.Body.String())
	}
}

func TestAssistantRecentContextExcludesDisplayAndInternalControl(t *testing.T) {
	coord, caller, _ := assistantFixture(t, "qwenpaw")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			t.Error(r.URL.Path)
		}
		messages := []any{}
		for i, content := range []map[string]any{
			{"msgtype": "m.text", "body": "用户的页面需求"},
			{"msgtype": "m.notice", "body": "工具调用结果", "org.agentteams.progress_only": true},
			{"msgtype": "m.notice", "body": "内部规划", "org.agentteams.coordination": map[string]any{"kind": "handoff"}},
			{"msgtype": "m.text", "body": "已完成页面并提交 PR"},
		} {
			messages = append(messages, map[string]any{"event_id": string(rune('a' + i)), "sender": "@lead:test", "type": "m.room.message", "content": content})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"chunk": messages})
	}))
	defer server.Close()
	creds := &service.FileCredentialStore{Dir: t.TempDir()}
	if err := creds.Save(t.Context(), "lead-cr", &service.WorkerCredentials{MatrixToken: "fixture-token"}); err != nil {
		t.Fatal(err)
	}
	coord.deps.Provisioner = service.NewProvisioner(service.ProvisionerConfig{Creds: creds})
	coord.deps.MatrixConfig = matrix.Config{ServerURL: server.URL}
	var team v1.Team
	if err := coord.deps.Client.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "alpha"}, &team); err != nil {
		t.Fatal(err)
	}
	team.Status.AssistantRecipient = service.TeamAssistantRecipient("@alice:test", "")
	if err := coord.deps.Client.Update(t.Context(), &team); err != nil {
		t.Fatal(err)
	}
	w := assistantCall(NewAssistantHandler(coord.deps), caller, "GET", "context?team=alpha", nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var view struct {
		Recent []map[string]string `json:"recentMessages"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if len(view.Recent) != 2 || view.Recent[0]["text"] != "已完成页面并提交 PR" || view.Recent[1]["text"] != "用户的页面需求" {
		t.Fatal("display-only messages leaked into assistant context", view.Recent)
	}
}
