package server

import (
	"context"
	"encoding/json"
	"path"
	"testing"
	"time"
)

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
