package server

import (
	"context"
	"encoding/json"
	"path"
	"testing"
	"time"

	v1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil/mocks"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAssistantHumanCanDisableOfflineLeader(t *testing.T) {
	h, _, _ := assistantFixture(t, "qwenpaw")
	b := mocks.NewMockWorkerBackend()
	b.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
		return &backend.WorkerResult{Status: backend.StatusStopped}, nil
	}
	h.assistant.deps.Backend = backend.NewRegistry([]backend.WorkerBackend{b})
	user := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "alice", MatrixUserID: "@alice:test"}
	w := assistantCall(h.assistant, user, "GET", "context?team=alpha", nil)
	var view map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	w = assistantCall(h.assistant, user, "PUT", "context?team=alpha", map[string]any{"owner": view["owner"], "revision": view["revision"], "enabled": false})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestAssistantManagerRestartFencesOldExecutionWithoutSpecChange(t *testing.T) {
	h, _ := coordinationFixture(t)
	human := &v1.Human{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"}, Spec: v1.HumanSpec{PermissionLevel: 1}, Status: v1.HumanStatus{MatrixUserID: "@alice:test", ManagerRooms: map[string]string{"default": "!alice:test"}}}
	if err := h.deps.Client.Create(t.Context(), human); err != nil {
		t.Fatal(err)
	}
	b := mocks.NewMockWorkerBackend()
	instance := "first-start"
	b.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
		return &backend.WorkerResult{Status: backend.StatusRunning, InstanceID: instance, Runtime: "qwenpaw"}, nil
	}
	h.deps.Backend = backend.NewRegistry([]backend.WorkerBackend{b})
	assistant := NewAssistantHandler(h.deps)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "default"}
	before, err := assistant.routeForRecipient(t.Context(), caller, "", "default", "alice")
	if err != nil {
		t.Fatal(err)
	}
	instance = "second-start"
	after, err := assistant.routeForRecipient(t.Context(), caller, "", "default", "alice")
	if err != nil || before.owner == after.owner || before.room != after.room || before.prefix != after.prefix {
		t.Fatal(before, after, err)
	}
	w := assistantCall(assistant, caller, "PUT", "context?manager=default&user=alice", map[string]any{"owner": before.owner, "revision": "", "enabled": false})
	if w.Code != 409 {
		t.Fatal("restarted Manager accepted previous executor", w.Code, w.Body.String())
	}
}

func TestAssistantDefaultsToThreeHoursAndReturnsFreshnessWindow(t *testing.T) {
	h, caller, _ := assistantFixture(t, "qwenpaw")
	w := assistantCall(h.assistant, caller, "GET", "context?team=alpha", nil)
	var view struct {
		Context     assistantContext `json:"context"`
		WindowStart time.Time        `json:"windowStart"`
		WindowEnd   time.Time        `json:"windowEnd"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if !view.Context.Enabled || view.Context.EverySeconds != 10800 || view.WindowEnd.Sub(view.WindowStart) != 3*time.Hour {
		t.Fatal(view)
	}
}

func TestAssistantSkipRecordsCheckWithoutSendingOrLosingLastMessage(t *testing.T) {
	for _, runtime := range []string{"openclaw", "qwenpaw", "hermes", "deepseek-harness", "openhuman", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			h, caller, route := assistantFixture(t, runtime)
			state := assistantContext{Enabled: true, EverySeconds: 10800, Summary: "interests", LastMessageID: "$previous", LastRun: time.Now().Add(-4 * time.Hour).UTC().Format(time.RFC3339Nano)}
			contextRevision, err := h.assistant.save(t.Context(), path.Join(route.prefix, "context.json"), state, "")
			if err != nil {
				t.Fatal(err)
			}
			requestID := assistantRequestID(state)
			e, err := h.resolveAssistant(t.Context(), coordinationEvent{Kind: "assistant_digest", DeliveryID: route.owner, OriginEventID: requestID, SourceRoute: map[string]any{"team": "alpha"}}, caller)
			if err != nil {
				t.Fatal(err)
			}
			h.assistant.send = func(context.Context, string, string, string, string, map[string]any) (string, error) {
				t.Fatal("skip sent a Matrix message")
				return "", nil
			}
			input := map[string]any{"owner": route.owner, "requestId": requestID, "contextRevision": contextRevision, "skip": true, "reason": "no new verified updates", "summary": "interests; checked this window"}
			for range 2 {
				w := assistantCall(h.assistant, caller, "POST", "messages?team=alpha", input)
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				var result map[string]any
				_ = json.Unmarshal(w.Body.Bytes(), &result)
				if result["status"] != "skipped" || result["messageId"] != nil || result["sentAt"] != nil {
					t.Fatal(result)
				}
			}
			after, err := h.assistant.contextState(t.Context(), route)
			if err != nil || after.LastRun == state.LastRun || after.LastMessageID != "$previous" || after.Summary != "interests; checked this window" || assistantRequestID(after) == requestID {
				t.Fatal(after, err)
			}
			// Repair a crash between durable outcome and context checkpoint.
			b, _ := json.Marshal(state)
			if err := h.deps.OSS.PutObject(t.Context(), path.Join(route.prefix, "context.json"), b); err != nil {
				t.Fatal(err)
			}
			d := coordinationDelivery{Event: e, Status: "running", NativeID: "matrix:$wake"}
			h.advance(t.Context(), &d, func() error { return nil })
			if d.Status != "completed" || d.UserEventID != "" {
				t.Fatal(d.Status, d.UserEventID, d.Error)
			}
			repaired, _ := h.assistant.contextState(t.Context(), route)
			if repaired.LastRun != after.LastRun || repaired.LastMessageID != "$previous" {
				t.Fatal("outcome checkpoint was not repaired", repaired)
			}
			// A completed no-update check waits for the next interval.
			h.scheduleAssistants(t.Context())
			entries, _, err := h.deps.OSS.ListObjectsRecursive(t.Context(), coordinationPending, "", 100)
			if err != nil || len(entries) != 0 {
				t.Fatal("skipped check immediately scheduled again", entries, err)
			}
			input["skip"], input["text"] = false, "changed into a reminder"
			if w := assistantCall(h.assistant, caller, "POST", "messages?team=alpha", input); w.Code != 409 {
				t.Fatal("skip receipt was reused to send", w.Code, w.Body.String())
			}
		})
	}
}
