package server

import (
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"net/http"
	"testing"
)

func TestResolveAttention_ExpectedEventBindsDisplayedQuestion(t *testing.T) {
	for _, resolved := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "resolved"}[resolved], func(t *testing.T) {
			store := ossfake.NewMemory()
			entry := pendingDecisionRecord()
			entry["resolved"] = resolved
			if resolved {
				entry["resolution"] = "ship-a"
				entry["resolvedBy"] = "alice"
			}
			seedResolveBench(t, store, []any{entry})
			mx := &resolveFakeMatrix{}
			h := newResolveTestHandler(t, store, mx, teamWithHumans("alpha-team", "alice"))
			response := resolveAttentionTestRequest(t, h, "p1", "t1", `{"resolution":"ship-a","expectedEventId":"older-question"}`, aliceCaller)
			if response.Code != http.StatusConflict || readResolveCode(t, response) != "attention_changed" {
				t.Fatalf("stale answer=%d %s", response.Code, response.Body)
			}
			if len(mx.sentMessages()) != 0 {
				t.Fatal("stale answer emitted a room message")
			}
			if readResolveTaskRecord(t, store)["resolved"] != resolved {
				t.Fatal("stale answer changed the record")
			}
			for range 2 {
				valid := resolveAttentionTestRequest(t, h, "p1", "t1", `{"resolution":"ship-a","expectedEventId":"att-pending-1"}`, aliceCaller)
				if valid.Code != http.StatusOK {
					t.Fatalf("matching answer/replay=%d %s", valid.Code, valid.Body)
				}
			}
			want := 1
			if resolved {
				want = 0
			}
			if len(mx.sentMessages()) != want {
				t.Fatalf("messages=%d", len(mx.sentMessages()))
			}
		})
	}
}

func TestResolveAttention_NewQuestionDuringCASIsNotAnswered(t *testing.T) {
	store := ossfake.NewMemory()
	old := pendingDecisionRecord()
	seedResolveBench(t, store, []any{old})
	newer := pendingDecisionRecord()
	newer["eventId"] = "newer-question"
	newer["question"] = "Different decision?"
	racing := &conflictOSS{mcLikeOSS: &mcLikeOSS{Memory: store}, external: map[string]any{"attention": []any{old, newer}}}
	mx := &resolveFakeMatrix{}
	h := newResolveTestHandlerWithStore(t, racing, mx, teamWithHumans("alpha-team", "alice"))
	response := resolveAttentionTestRequest(t, h, "p1", "t1", `{"resolution":"ship-a","expectedEventId":"att-pending-1"}`, aliceCaller)
	if response.Code != http.StatusConflict || readResolveCode(t, response) != "attention_changed" {
		t.Fatalf("racing answer=%d %s", response.Code, response.Body)
	}
	if racing.calls != 1 {
		t.Fatalf("stale answer retried write %d times", racing.calls)
	}
	for _, raw := range readResolveTaskMeta(t, store)["attention"].([]any) {
		if raw.(map[string]any)["resolved"] == true {
			t.Fatal("a different question was answered")
		}
	}
	if len(mx.sentMessages()) != 0 {
		t.Fatal("stale answer notified the room")
	}
}
