package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

func TestVerifiedTaskNotificationDoesNotWaitForChatHistory(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	id := strings.Repeat("b", 32)
	input := &service.WorkerMatrixInput{RoomID: "!task:test", EventID: "$ready", Input: service.WorkerMatrixContent{Sender: "@developer:test", Content: map[string]any{"org.agentteams.coordination": map[string]any{"channel_meta": map[string]any{"teamharness_coordination": map[string]any{"id": id}}}}}}
	state := &service.WorkerMatrixState{Rooms: map[string]service.WorkerMatrixRoom{input.RoomID: {Gaps: []string{"missing-chat"}}}}
	receipt := coordinationDelivery{Event: coordinationEvent{ID: id, Protocol: "task/v1", Kind: "main_task_ready", Actor: input.Input.Sender, Recipient: "@leader:test", RoomID: input.RoomID}, Transport: "matrix", NativeID: "matrix:" + input.EventID, Status: "running"}
	save := func() {
		body, _ := json.Marshal(receipt)
		if err := f.storage.PutObject(t.Context(), coordinationPending+id+".json", body); err != nil {
			t.Fatal(err)
		}
	}
	save()
	mode, err := f.h.messageRecoveryMode(t.Context(), "@leader:test", state, input, taskExecution{})
	if err != nil || mode != "resume" {
		t.Fatalf("verified Task notification waited for chat: %s %v", mode, err)
	}
	// Unresolved native operations remain fenced even for a valid Task notification.
	mode, err = f.h.messageRecoveryMode(t.Context(), "@leader:test", state, input, taskExecution{Checkpoint: &taskCheckpoint{Operation: &taskOperation{State: "unknown"}}})
	if err != nil || mode != "verify" {
		t.Fatalf("unknown operation escaped verification: %s %v", mode, err)
	}
	for _, change := range []func(){func() { receipt.NativeID = "matrix:$other" }, func() { receipt.Event.Recipient = "@other:test" }, func() { receipt.Status = "failed" }} {
		original := receipt
		change()
		save()
		mode, err = f.h.messageRecoveryMode(t.Context(), "@leader:test", state, input, taskExecution{})
		if err != nil || mode != "backfill" {
			t.Fatalf("unverified Task source bypassed history: %s %v", mode, err)
		}
		receipt = original
	}
	input.Input.Content = map[string]any{"body": "continue"}
	save()
	mode, err = f.h.messageRecoveryMode(t.Context(), "@leader:test", state, input, taskExecution{})
	if err != nil || mode != "backfill" {
		t.Fatalf("ordinary chat bypassed history: %s %v", mode, err)
	}
}

func TestVerifiedTaskClaimResponseDoesNotRestoreChatHistoryGate(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	id := strings.Repeat("c", 32)
	input, _, err := f.h.store.InputSnapshot(t.Context(), "runtime-alice", f.key)
	if err != nil {
		t.Fatal(err)
	}
	input.Input.Content["org.agentteams.coordination"] = map[string]any{"channel_meta": map[string]any{"teamharness_coordination": map[string]any{"id": id}}}
	body, _ := json.Marshal(input)
	if err := f.storage.PutObject(t.Context(), f.objectKey(), body); err != nil {
		t.Fatal(err)
	}
	// The fixture's Worker has no Matrix account; bind the receipt to that
	// exact identity just as the normal admission path does.
	receipt := coordinationDelivery{Event: coordinationEvent{ID: id, Protocol: "task/v1", Kind: "main_task_ready", Actor: input.Input.Sender, RoomID: input.RoomID}, Transport: "matrix", NativeID: "matrix:" + input.EventID, Status: "running"}
	body, _ = json.Marshal(receipt)
	if err := f.storage.PutObject(t.Context(), coordinationPending+id+".json", body); err != nil {
		t.Fatal(err)
	}
	state, etag, err := f.h.store.Load(t.Context(), "runtime-alice")
	if err != nil {
		t.Fatal(err)
	}
	state.Rooms[input.RoomID] = service.WorkerMatrixRoom{Through: input.EventID, Gaps: []string{"missing-chat"}}
	body, _ = json.Marshal(state)
	if err := f.storage.PutObjectIfMatchStrict(t.Context(), "agents/runtime-alice/runtime/matrix/state.json", body, etag); err != nil {
		t.Fatal(err)
	}
	response := f.expect("claim", nil, 200)
	if string(response["recovery"]) != `"resume"` {
		t.Fatalf("successful Task claim restored the chat-history gate: %s", response["recovery"])
	}
}
