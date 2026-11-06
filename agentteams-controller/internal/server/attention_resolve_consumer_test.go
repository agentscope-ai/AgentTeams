package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

// consumerScript drives the shipped Python consumer (#1506's
// _protocol_guard_for_accept) against the task meta the real Go resolve
// handler persisted. Exit codes: 0 accepted, 3 rejected by the protocol
// guard, anything else is an infrastructure failure.
const consumerScript = `
import json, os, sys

plugin_root, meta_path = sys.argv[1], sys.argv[2]
sys.path.insert(0, plugin_root)
os.environ["TEAMHARNESS_PROTOCOL_ENFORCE"] = "1"

from mcp.lifecycle import review  # noqa: E402

task = json.load(open(meta_path, encoding="utf-8"))
try:
    review._protocol_guard_for_accept(task, True)
except review.ProtocolGuardError as exc:
    print("REJECT " + exc.error_code + ": " + exc.message)
    sys.exit(3)
print("ACCEPT")
sys.exit(0)
`

// TestAttentionResolve_DashboardApprovalConsumedByProtocolGuard is the
// cross-PR acceptance for #1515 ↔ #1506: the real ResolveAttention handler
// closes the approval as an authenticated roster human, and the shipped
// Python consumer must then accept the r1 result for approve and refuse it
// for deny. It asserts the server-owned markers (resolvedRole,
// resolvedVia, resolvedActor) rather than display fields, and runs the
// plugin's own guard (no reimplementation).
func TestAttentionResolve_DashboardApprovalConsumedByProtocolGuard(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available: cross-language acceptance needs it")
	}
	pluginRoot, err := filepath.Abs(filepath.Join("..", "..", "..", "plugins", "teamharness"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(pluginRoot, "mcp", "lifecycle", "review.py")); err != nil {
		t.Skipf("python consumer not present at %s: %v", pluginRoot, err)
	}

	cases := []struct {
		name       string
		resolution string
		frozen     bool
		wantAccept bool
	}{
		{"approve", "approve", false, true},
		{"deny", "deny", false, false},
		// The stall-driven frozen state is released by the same trusted
		// approval record: accept must pass, deny must still refuse.
		{"frozen_approve", "approve", true, true},
		{"frozen_deny", "deny", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := ossfake.NewMemory()
			putProject(store, resolveProjectKey, map[string]any{
				"project_id": "p1", "title": "P1", "status": "active",
				"plan_type": "dag", "team_id": "alpha-team",
				"tasks": []map[string]any{{"task_id": "t1", "title": "T1", "status": "in_progress", "depends_on": []string{}}},
			})
			metaFields := map[string]any{
				"attention":      []any{pendingApprovalRecord()},
				"decision_class": "r1",
				"assigned_to":    "worker:bob",
				"review":         map[string]any{"verdict": "pass"},
			}
			if tc.frozen {
				metaFields["decision_status"] = "frozen"
			}
			putTaskMeta(store, "alpha-team", "t1", resolveTaskMeta(metaFields))

			h := newResolveTestHandler(t, store, &resolveFakeMatrix{}, teamWithHumans("alpha-team", "alice"))
			body := `{"resolution":"` + tc.resolution + `","comment":"combined acceptance"}`
			rec := resolveAttentionTestRequest(t, h, "p1", "t1", body, aliceCaller)
			if rec.Code != 200 {
				t.Fatalf("resolve %s: status=%d body=%s", tc.resolution, rec.Code, rec.Body.String())
			}

			meta := readResolveTaskMeta(t, store)
			attention, _ := meta["attention"].([]any)
			if len(attention) != 1 {
				t.Fatalf("attention = %#v", meta["attention"])
			}
			record, _ := attention[0].(map[string]any)
			// The authority markers are server-written, never payload fields.
			if record["resolvedRole"] != "human" {
				t.Fatalf("resolvedRole=%v, want human", record["resolvedRole"])
			}
			if record["resolvedVia"] != "dashboard" {
				t.Fatalf("resolvedVia=%v, want dashboard", record["resolvedVia"])
			}
			if record["resolvedActor"] != "human:alice" {
				t.Fatalf("resolvedActor=%v, want human:alice", record["resolvedActor"])
			}

			raw, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			metaPath := filepath.Join(t.TempDir(), "meta.json")
			if err := os.WriteFile(metaPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command("python3", "-c", consumerScript, pluginRoot, metaPath)
			cmd.Env = append(os.Environ(), "TEAMHARNESS_PROTOCOL_ENFORCE=1")
			out, runErr := cmd.CombinedOutput()
			text := string(out)

			if tc.wantAccept {
				if runErr != nil {
					t.Fatalf("consumer rejected an approved r1 result: %v\n%s", runErr, text)
				}
				if !strings.Contains(text, "ACCEPT") {
					t.Fatalf("consumer output = %q, want ACCEPT", text)
				}
				return
			}
			if runErr == nil {
				t.Fatalf("consumer accepted a denied approval: %q", text)
			}
			if !strings.Contains(text, "approval_required") {
				t.Fatalf("consumer failed for the wrong reason: %q", text)
			}
		})
	}
}
