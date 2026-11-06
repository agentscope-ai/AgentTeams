package server

import (
	"net/http"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func TestAttentionResolve_ApprovalClearsStallState(t *testing.T) {
	for _, state := range []string{"blocked", "frozen", "paused", ""} {
		t.Run(state, func(t *testing.T) {
			store := ossfake.NewMemory()
			record := pendingApprovalRecord()
			extra := map[string]any{"attention": []any{record}}
			if state != "" {
				extra["decision_status"] = state
				record["stall_count"] = 3
				extra["decision_status_history"] = []any{map[string]any{"from": "open", "to": state}}
			}
			seedResolveBench(t, store, nil)
			putTaskMeta(store, "alpha-team", "t1", resolveTaskMeta(extra))
			mx := &resolveFakeMatrix{}
			h := newResolveTestHandler(t, store, mx, teamWithHumans("alpha-team", "alice"))
			response := resolveAttentionTestRequest(t, h, "p1", "t1", `{"resolution":"approve"}`, aliceCaller)
			if response.Code != http.StatusOK {
				t.Fatalf("resolve=%d %s", response.Code, response.Body)
			}
			meta := readResolveTaskMeta(t, store)
			resolved := meta["attention"].([]any)[0].(map[string]any)
			if state == "" {
				if _, exists := meta["decision_status"]; exists {
					t.Fatal("legacy record gained protocol state")
				}
				if _, exists := resolved["stall_count"]; exists {
					t.Fatal("legacy record gained stall counter")
				}
			} else {
				want := state
				if state == "blocked" || state == "frozen" {
					want = "open"
				}
				if meta["decision_status"] != want || intFromAny(resolved["stall_count"]) != 0 {
					t.Fatalf("state=%v stall=%v, want %s/0", meta["decision_status"], resolved["stall_count"], want)
				}
				history := meta["decision_status_history"].([]any)
				count := 1
				if want == "open" {
					count = 2
				}
				if len(history) != count {
					t.Fatalf("history=%v", history)
				}
				if want == "open" {
					entry := history[1].(map[string]any)
					if entry["from"] != state || entry["to"] != "open" || entry["source"] != "attention:approval" || entry["actor"] != "human:alice" {
						t.Fatalf("history entry=%v", entry)
					}
				}
			}
			before := len(mx.sentMessages())
			retry := resolveAttentionTestRequest(t, h, "p1", "t1", `{"resolution":"approve"}`, aliceCaller)
			if retry.Code != 200 || len(mx.sentMessages()) != before {
				t.Fatalf("retry=%d notifications=%d", retry.Code, len(mx.sentMessages()))
			}
			after := readResolveTaskMeta(t, store)
			if state != "" && len(after["decision_status_history"].([]any)) != len(meta["decision_status_history"].([]any)) {
				t.Fatal("retry duplicated unfreeze history")
			}
		})
	}
}

func TestAttentionResolve_NonHumanCannotBorrowHumanRosterName(t *testing.T) {
	for _, role := range []string{authpkg.RoleTeamLeader, authpkg.RoleWorker, authpkg.RoleManager, authpkg.RoleAdmin} {
		t.Run(role, func(t *testing.T) {
			store := ossfake.NewMemory()
			seedResolveBench(t, store, []any{pendingApprovalRecord()})
			h := newResolveTestHandler(t, store, &resolveFakeMatrix{}, teamWithHumans("alpha-team", "alice"))
			caller := &authpkg.CallerIdentity{Role: role, Username: "alice", Team: "alpha-team", Teams: []string{"alpha-team"}}
			rec := resolveAttentionTestRequest(t, h, "p1", "t1", `{"resolution":"approve"}`, caller)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("role=%s status=%d body=%s", role, rec.Code, rec.Body)
			}
			if readResolveTaskMeta(t, store)["attention"].([]any)[0].(map[string]any)["resolved"] == true {
				t.Fatal("non-human resolved the human approval")
			}
		})
	}
}
