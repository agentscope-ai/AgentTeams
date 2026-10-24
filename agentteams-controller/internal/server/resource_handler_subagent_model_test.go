package server

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"k8s.io/apimachinery/pkg/types"
)

// subagentModel pointer semantics on the L1 (owner/admin) path: an explicit
// value sets it, omission (or explicit null) keeps the current value, and
// an explicit empty string clears it back to the team/default inheritance.
func TestUpdateWorker_SubagentModelL1SetOmitClear(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	ctx := context.Background()
	key := types.NamespacedName{Name: "alpha-dev", Namespace: "default"}
	getSlot := func() string {
		var w v1beta1.Worker
		if err := handler.client.Get(ctx, key, &w); err != nil {
			t.Fatalf("get worker: %v", err)
		}
		return w.Spec.SubagentModel
	}

	rec := l2UpdateRequest(t, handler, "alpha-dev", `{"subagentModel":"qwen3.6-flash"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("set: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := getSlot(); got != "qwen3.6-flash" {
		t.Fatalf("set: SubagentModel=%q, want qwen3.6-flash", got)
	}

	rec = l2UpdateRequest(t, handler, "alpha-dev", `{}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("omit: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := getSlot(); got != "qwen3.6-flash" {
		t.Fatalf("omit: SubagentModel=%q, want the previous value kept", got)
	}

	rec = l2UpdateRequest(t, handler, "alpha-dev", `{"subagentModel":null}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("null: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := getSlot(); got != "qwen3.6-flash" {
		t.Fatalf("null: SubagentModel=%q, want the previous value kept", got)
	}

	rec = l2UpdateRequest(t, handler, "alpha-dev", `{"subagentModel":""}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := getSlot(); got != "" {
		t.Fatalf("clear: SubagentModel=%q, want empty (inheritance restored)", got)
	}
}

// An explicit clear must stay L1-only: scoped callers are rejected on the
// field even when the value is the empty string (the pointer is present).
func TestUpdateWorker_SubagentModelL2ClearRejected(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"}}

	rec := l2UpdateRequest(t, handler, "alpha-dev", `{"subagentModel":""}`, caller)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (scoped caller must not clear), got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("subagentModel")) {
		t.Fatalf("error should name the offending field, got: %s", rec.Body.String())
	}
}
