package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
	"time"

	v1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"k8s.io/apimachinery/pkg/types"
)

func assistantFixture(t *testing.T, runtime string) (*CoordinationHandler, *authpkg.CallerIdentity, assistantRoute) {
	t.Helper()
	h, _ := coordinationFixture(t)
	var team v1.Team
	key := types.NamespacedName{Namespace: "default", Name: "alpha"}
	if err := h.deps.Client.Get(t.Context(), key, &team); err != nil {
		t.Fatal(err)
	}
	team.Spec.Admin = &v1.TeamAdminSpec{Name: "alice", MatrixUserID: "@alice:test"}
	team.Status.LeaderDMRoomID = "!assistant:test"
	if err := h.deps.Client.Update(t.Context(), &team); err != nil {
		t.Fatal(err)
	}
	var worker v1.Worker
	if err := h.deps.Client.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "lead-cr"}, &worker); err != nil {
		t.Fatal(err)
	}
	worker.Spec.Runtime = runtime
	if err := h.deps.Client.Update(t.Context(), &worker); err != nil {
		t.Fatal(err)
	}
	h.assistant = NewAssistantHandler(h.deps)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"}
	route, err := h.assistant.routeFor(t.Context(), caller, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	return h, caller, route
}

func TestAssistantUsesOneTeamsProtocolForEveryHarness(t *testing.T) {
	for _, runtime := range []string{"openclaw", "qwenpaw", "hermes", "deepseek-harness", "openhuman", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			h, caller, route := assistantFixture(t, runtime)
			state, err := h.assistant.contextState(t.Context(), route)
			if err != nil {
				t.Fatal(err)
			}
			input := coordinationEvent{Kind: "assistant_digest", DeliveryID: route.owner, OriginEventID: assistantRequestID(state), SourceRoute: map[string]any{"team": "alpha"}, Text: "untrusted", TaskID: "must-not-become-a-task"}
			first, err := h.enqueue(t.Context(), input, caller)
			if err != nil {
				t.Fatal(err)
			}
			retry, err := h.enqueue(t.Context(), input, caller)
			if err != nil || !retry.Reused || first.EventID != retry.EventID {
				t.Fatal("duplicate Teams wake", retry, err)
			}
			operatorRequest := withCaller(httptest.NewRequest("GET", "/api/v1/coordination/events/"+first.EventID, nil), &authpkg.CallerIdentity{Role: authpkg.RoleAdmin})
			operatorRequest.SetPathValue("id", first.EventID)
			operatorResponse := httptest.NewRecorder()
			h.GetEvent(operatorResponse, operatorRequest)
			if operatorResponse.Code != 200 {
				t.Fatal("operator cannot inspect its scheduled delivery", operatorResponse.Code)
			}
			var delivery coordinationDelivery
			b, err := h.deps.OSS.GetObject(t.Context(), coordinationPending+first.EventID+".json")
			if err != nil || json.Unmarshal(b, &delivery) != nil {
				t.Fatal("missing durable admission", err)
			}
			if delivery.Transport != "matrix" || delivery.Event.TaskID != "" || delivery.Event.Text == "untrusted" {
				t.Fatal("runtime-specific or caller supplied execution", delivery)
			}
			wakes, reminders := 0, 0
			h.assistant.send = func(_ context.Context, kind, name, room, txn string, content map[string]any) (string, error) {
				if room != route.room {
					t.Fatal("wrong conversation")
				}
				if content["org.agentteams.coordination"] != nil {
					wakes++
					if content["msgtype"] != "m.notice" {
						t.Fatal("background wake can notify the Human", content)
					}
					if content["m.mentions"] == nil {
						t.Fatal("Teams wake lost recipient mention")
					}
					return "$wake", nil
				}
				reminders++
				return "$reminder", nil
			}
			h.advance(t.Context(), &delivery, func() error { return nil })
			h.advance(t.Context(), &delivery, func() error { return nil })
			if wakes != 1 || delivery.Status != "running" {
				t.Fatal("wake duplicated or reported as user delivery", wakes, delivery.Status)
			}
			message, _ := json.Marshal(assistantReceipt{Owner: route.owner, RequestID: input.OriginEventID, Text: "已核实的摘要", Summary: "用户上下文"})
			r := withCaller(httptest.NewRequest("POST", "/api/v1/assistant/messages?team=alpha", strings.NewReader(string(message))), caller)
			w := httptest.NewRecorder()
			h.assistant.Send(w, r)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			h.advance(t.Context(), &delivery, func() error { return nil })
			if delivery.Status != "completed" || delivery.UserEventID != "$reminder" || reminders != 1 {
				t.Fatal("missing real send receipt", delivery)
			}
		})
	}
}

func TestAssistantDisableInvalidatesQueuedWake(t *testing.T) {
	h, caller, route := assistantFixture(t, "qwenpaw")
	state, _ := h.assistant.contextState(t.Context(), route)
	e, err := h.resolveAssistant(t.Context(), coordinationEvent{Kind: "assistant_digest", DeliveryID: route.owner, OriginEventID: assistantRequestID(state), SourceRoute: map[string]any{"team": "alpha"}}, caller)
	if err != nil {
		t.Fatal(err)
	}
	state.Enabled = false
	b, _ := json.Marshal(state)
	if err := h.deps.OSS.PutObject(t.Context(), path.Join(route.prefix, "context.json"), b); err != nil {
		t.Fatal(err)
	}
	h.assistant.send = func(context.Context, string, string, string, string, map[string]any) (string, error) {
		t.Fatal("disabled wake was sent")
		return "", nil
	}
	d := coordinationDelivery{Event: e, Status: "queued"}
	h.advance(t.Context(), &d, func() error { return nil })
	if d.Status != "ignored" {
		t.Fatal("disabled work remains executable", d.Status)
	}
}

func TestAssistantControllerSchedulesOnlyDueWorkOnce(t *testing.T) {
	h, _, route := assistantFixture(t, "qwenpaw")
	state := assistantContext{Enabled: true, EverySeconds: 30, LastRun: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	write := func() {
		b, _ := json.Marshal(state)
		if err := h.deps.OSS.PutObject(t.Context(), path.Join(route.prefix, "context.json"), b); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		entries, _, err := h.deps.OSS.ListObjectsRecursive(t.Context(), coordinationPending, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	write()
	h.scheduleAssistants(t.Context())
	if count() != 0 {
		t.Fatal("scheduled before due time")
	}
	state.LastRun = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	write()
	h.scheduleAssistants(t.Context())
	h.scheduleAssistants(t.Context())
	if count() != 1 {
		t.Fatal("scheduler duplicated the same role/user cycle", count())
	}
}
