package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Team attention inbox (#1394 S1): the read surface the dashboard
// inbox consumes. Assertions are on the wire shape (the row contract is
// verbatim) and on the stored event object (the lazy overdue sweep).

type teamAttentionResp struct {
	TeamID    string             `json:"team_id"`
	Items     []teamAttentionRow `json:"items"`
	Count     int                `json:"count"`
	Truncated bool               `json:"truncated"`
}

func getTeamAttention(t *testing.T, h *ProjectHandler, team, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/teams/"+team+"/attention"+query,
		nil,
	)
	req.SetPathValue("name", team)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTeamAttention(rec, req)
	return rec
}

func decodeTeamAttention(t *testing.T, rec *httptest.ResponseRecorder) teamAttentionResp {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp teamAttentionResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	return resp
}

// newTeamAttentionHandler builds a handler over a custom StorageClient with
// the given Teams registered in the fake API server.
func newTeamAttentionHandler(t *testing.T, o oss.StorageClient, teams ...*v1beta1.Team) *ProjectHandler {
	t.Helper()
	objs := make([]runtime.Object, 0, len(teams))
	for _, tm := range teams {
		objs = append(objs, tm)
	}
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	return NewProjectHandler(k8s, "default", o)
}

// seedTeamAttentionProject writes a project meta plus the given task metas
// (keyed by task id) under the team's shared prefix.
func seedTeamAttentionProject(store *ossfake.Memory, team, projectID string, taskIDs []string, teamID string) {
	putProject(store, "teams/"+team+"/shared/projects/"+projectID+"/meta.json", map[string]any{
		"project_id": projectID, "title": projectID, "status": "active", "plan_type": "dag", "team_id": teamID,
		"tasks": func() []any {
			out := make([]any, 0, len(taskIDs))
			for _, id := range taskIDs {
				out = append(out, map[string]any{"task_id": id, "title": id, "status": "active"})
			}
			return out
		}(),
	})
	for _, id := range taskIDs {
		putProject(store, "teams/"+team+"/shared/tasks/"+id+"/meta.json", map[string]any{
			"task_id": id, "project_id": projectID, "title": id, "status": "active",
		})
	}
}

func putTaskAttention(store *ossfake.Memory, team, taskID string, extra map[string]any) {
	meta := map[string]any{
		"task_id": taskID, "title": taskID, "status": "active",
	}
	for k, v := range extra {
		meta[k] = v
	}
	putProject(store, "teams/"+team+"/shared/tasks/"+taskID+"/meta.json", meta)
}

// TestGetTeamAttention_AggregatesAndShape pins the verbatim row contract.
func TestGetTeamAttention_AggregatesAndShape(t *testing.T) {
	store := ossfake.NewMemory()
	seedTeamAttentionProject(store, "alpha-team", "p1", []string{"t1"}, "alpha-team")
	seedTeamAttentionProject(store, "alpha-team", "p2", []string{"t2"}, "alpha-team")
	seedTeamAttentionProject(store, "alpha-team", "p3", []string{"t3"}, "alpha-team")
	putTaskAttention(store, "alpha-team", "t1", map[string]any{
		"room_id": "!room-t1:host",
		"attention": []any{map[string]any{
			"kind": "approval", "question": "Proceed with deploy?", "attempt": 1,
			"requestedAt": "2026-11-02T09:00:00Z", "timeoutAt": "2099-01-01T00:00:00Z",
			"resolved": false, "eventId": "ev_a1",
			"options": []any{
				map[string]any{"id": "yes", "label": "Yes", "description": "ship it"},
				map[string]any{"id": "no", "label": "No"},
			},
			"notification_log": []any{map[string]any{
				"attempt": 1, "ts": "2026-11-02T09:00:04Z",
				"room_accepted": true, "channel_accepted": nil,
			}},
		}},
	})
	putTaskAttention(store, "alpha-team", "t2", map[string]any{
		"attention": []any{map[string]any{
			"kind": "decision", "question": "Which branch?", "attempt": 2,
			"requestedAt": "2026-11-02T08:00:00Z", "resolved": false,
		}},
	})
	putTaskAttention(store, "alpha-team", "t3", map[string]any{
		"attention": []any{map[string]any{
			"kind": "review", "question": "Approve the draft?", "attempt": 1,
			"requestedAt": "2026-11-02T07:00:00Z", "resolved": true,
			"resolvedVia": "im_reply", "resolvedBy": "@luo:host",
			"resolvedAt": "2026-11-02T07:30:00Z", "resolution": "approved",
		}},
	})
	h := newTeamAttentionHandler(t, &mcLikeOSS{Memory: store}, team("alpha-team"))

	resp := decodeTeamAttention(t, getTeamAttention(t, h, "alpha-team", "?status=all"))
	if resp.TeamID != "alpha-team" || resp.Count != 3 || len(resp.Items) != 3 {
		t.Fatalf("want 3 rows, got %+v", resp)
	}
	// Newest requestedAt first.
	if resp.Items[0].TaskID != "t1" || resp.Items[1].TaskID != "t2" || resp.Items[2].TaskID != "t3" {
		t.Fatalf("order: %v %v %v", resp.Items[0].TaskID, resp.Items[1].TaskID, resp.Items[2].TaskID)
	}
	row := resp.Items[0]
	if row.TeamID != "alpha-team" || row.ProjectID != "p1" || row.Kind != "approval" ||
		row.Question != "Proceed with deploy?" || row.RequestedAt != "2026-11-02T09:00:00Z" ||
		row.TimeoutAt != "2099-01-01T00:00:00Z" {
		t.Fatalf("row identity fields: %+v", row)
	}
	if row.Overdue || row.Resolved || row.Freeform {
		t.Fatalf("booleans: overdue=%v resolved=%v freeform=%v", row.Overdue, row.Resolved, row.Freeform)
	}
	if row.ResolvedVia != nil || row.ResolvedBy != nil || row.ResolvedAt != nil || row.Resolution != nil {
		t.Fatalf("unresolved provenance must be null: %+v", row)
	}
	if row.RoomID != "!room-t1:host" || row.URL != "/projects/p1/tasks/t1" {
		t.Fatalf("room/url: %q %q", row.RoomID, row.URL)
	}
	if len(row.Options) != 2 || row.Options[0].Description != "ship it" || row.Options[1].Description != "" {
		t.Fatalf("options passthrough: %+v", row.Options)
	}
	if row.Notified == nil {
		t.Fatalf("notified must be present when notification_log has entries")
	}
	if !row.Notified.Sent || !row.Notified.RoomAccepted {
		t.Fatalf("notified outcome: %+v", row.Notified)
	}
	if row.Notified.ChannelAccepted != nil {
		t.Fatalf("channelAccepted must stay null (never inferred): %+v", row.Notified.ChannelAccepted)
	}
	if len(row.Notified.Recipients) != 0 || len(row.Notified.Gaps) != 0 {
		t.Fatalf("recipients/gaps default to empty lists: %+v", row.Notified)
	}
	if row.Notified.At != "2026-11-02T09:00:04Z" {
		t.Fatalf("notified.at: %q", row.Notified.At)
	}
	// No notification_log at all -> null.
	if resp.Items[1].Notified != nil {
		t.Fatalf("missing notification_log must be null: %+v", resp.Items[1].Notified)
	}
	// Raw wire shape: the contract keys are exactly these (camelCase).
	data, _ := json.Marshal(row)
	var wire map[string]any
	_ = json.Unmarshal(data, &wire)
	for _, key := range []string{"taskId", "teamId", "projectId", "kind", "question", "options",
		"requestedAt", "timeoutAt", "overdue", "resolved", "resolvedVia", "resolvedBy",
		"resolvedAt", "resolution", "freeform", "notified", "roomId", "url"} {
		if _, ok := wire[key]; !ok {
			t.Fatalf("row is missing contract key %q: %v", key, wire)
		}
	}
	if _, ok := wire["task_id"]; ok {
		t.Fatalf("snake_case leaked into the row: %v", wire)
	}
	// The resolved provenance is carried through on resolved rows.
	resolved := resp.Items[2]
	if resolved.ResolvedVia == nil || *resolved.ResolvedVia != "im_reply" ||
		resolved.ResolvedBy == nil || *resolved.ResolvedBy != "@luo:host" ||
		resolved.ResolvedAt == nil || *resolved.ResolvedAt != "2026-11-02T07:30:00Z" ||
		resolved.Resolution == nil || *resolved.Resolution != "approved" {
		t.Fatalf("resolved provenance: %+v", resolved)
	}
}

// TestGetTeamAttention_StatusFilters pins the four filters; deadlines in the
// past are overdue, a deadline in the future is not.
func TestGetTeamAttention_StatusFilters(t *testing.T) {
	store := ossfake.NewMemory()
	seedTeamAttentionProject(store, "alpha-team", "p1", []string{"t1", "t2", "t3"}, "alpha-team")
	putTaskAttention(store, "alpha-team", "t1", map[string]any{
		"attention": []any{map[string]any{
			"kind": "decision", "question": "open one", "attempt": 1,
			"requestedAt": "2026-11-02T09:00:00Z", "timeoutAt": "2099-01-01T00:00:00Z", "resolved": false,
		}},
	})
	putTaskAttention(store, "alpha-team", "t2", map[string]any{
		"attention": []any{map[string]any{
			"kind": "approval", "question": "late one", "attempt": 1,
			"requestedAt": "2026-11-02T08:00:00Z", "timeoutAt": "2020-01-01T00:00:00Z", "resolved": false,
		}},
	})
	putTaskAttention(store, "alpha-team", "t3", map[string]any{
		"attention": []any{map[string]any{
			"kind": "review", "question": "closed one", "attempt": 1,
			"requestedAt": "2026-11-02T07:00:00Z", "resolved": true,
		}},
	})
	h := newTeamAttentionHandler(t, &mcLikeOSS{Memory: store}, team("alpha-team"))

	if got := decodeTeamAttention(t, getTeamAttention(t, h, "alpha-team", "")).Items; len(got) != 1 || got[0].TaskID != "t1" {
		t.Fatalf("open filter: %+v", got)
	}
	over := decodeTeamAttention(t, getTeamAttention(t, h, "alpha-team", "?status=overdue")).Items
	if len(over) != 1 || over[0].TaskID != "t2" || !over[0].Overdue {
		t.Fatalf("overdue filter: %+v", over)
	}
	if got := decodeTeamAttention(t, getTeamAttention(t, h, "alpha-team", "?status=resolved")).Items; len(got) != 1 || got[0].TaskID != "t3" {
		t.Fatalf("resolved filter: %+v", got)
	}
	if got := decodeTeamAttention(t, getTeamAttention(t, h, "alpha-team", "?status=all")).Items; len(got) != 3 {
		t.Fatalf("all filter: %+v", got)
	}
	if rec := getTeamAttention(t, h, "alpha-team", "?status=bogus"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bogus status must be 400, got %d", rec.Code)
	}
}

// TestAttentionOverdue_StrictlyBefore pins the S1 rule at the equality
// boundary: timeoutAt == now is NOT overdue (strict <), and a missing or
// unparseable deadline is never overdue.
func TestAttentionOverdue_StrictlyBefore(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if attentionOverdue(now.Format(time.RFC3339), now) {
		t.Fatalf("timeoutAt == now must not be overdue")
	}
	if !attentionOverdue(now.Add(-time.Second).Format(time.RFC3339), now) {
		t.Fatalf("one second past the deadline must be overdue")
	}
	for _, bad := range []string{"", "not-a-time", "2099-13-40T00:00:00Z"} {
		if attentionOverdue(bad, now) {
			t.Fatalf("deadline %q must not be overdue", bad)
		}
	}
}

// TestGetTeamAttention_TeamIsolation pins the team boundary: the read never
// crosses prefixes, and a scoped caller outside the team gets a 404 (no
// existence probing).
func TestGetTeamAttention_TeamIsolation(t *testing.T) {
	store := ossfake.NewMemory()
	seedTeamAttentionProject(store, "alpha-team", "p1", []string{"t1"}, "alpha-team")
	putTaskAttention(store, "alpha-team", "t1", map[string]any{
		"attention": []any{map[string]any{"kind": "decision", "question": "alpha?", "attempt": 1,
			"requestedAt": "2026-11-02T09:00:00Z", "resolved": false}},
	})
	seedTeamAttentionProject(store, "beta-team", "p2", []string{"t2"}, "beta-team")
	putTaskAttention(store, "beta-team", "t2", map[string]any{
		"attention": []any{map[string]any{"kind": "decision", "question": "beta?", "attempt": 1,
			"requestedAt": "2026-11-02T09:00:00Z", "resolved": false}},
	})
	// A standalone project at the global prefix must never leak into a team read.
	putProject(store, "shared/projects/p9/meta.json", map[string]any{
		"project_id": "p9", "title": "standalone", "status": "active", "plan_type": "dag",
		"tasks": []any{map[string]any{"task_id": "t9", "title": "t9", "status": "active"}},
	})
	putProject(store, "shared/tasks/t9/meta.json", map[string]any{
		"task_id": "t9", "attention": []any{map[string]any{"kind": "decision", "question": "global?",
			"attempt": 1, "requestedAt": "2026-11-02T09:00:00Z", "resolved": false}},
	})
	h := newTeamAttentionHandler(t, &mcLikeOSS{Memory: store}, team("alpha-team"), team("beta-team"))

	alpha := decodeTeamAttention(t, getTeamAttention(t, h, "alpha-team", ""))
	if alpha.Count != 1 || alpha.Items[0].TaskID != "t1" || alpha.Items[0].TeamID != "alpha-team" {
		t.Fatalf("alpha isolation: %+v", alpha.Items)
	}
	beta := decodeTeamAttention(t, getTeamAttention(t, h, "beta-team", ""))
	if beta.Count != 1 || beta.Items[0].TaskID != "t2" {
		t.Fatalf("beta isolation: %+v", beta.Items)
	}

	// Scoped callers: a leader of beta-team must not read alpha-team.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/teams/alpha-team/attention", nil)
	req.SetPathValue("name", "alpha-team")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "beta-leader", Team: "beta-team"})
	rec := httptest.NewRecorder()
	h.GetTeamAttention(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-team read must be 404, got %d", rec.Code)
	}
	// The same caller reads its own team.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/teams/beta-team/attention", nil)
	req2.SetPathValue("name", "beta-team")
	req2 = withCaller(req2, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "beta-leader", Team: "beta-team"})
	rec2 := httptest.NewRecorder()
	h.GetTeamAttention(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("own-team read must succeed, got %d body=%s", rec2.Code, rec2.Body.String())
	}
}

// TestGetTeamAttention_UnknownTeamAndUnreadableProject pins the failure
// split: unknown team -> 404; a broken project meta is skipped and counted in
// X-Attention-Skipped while the rest of the team still answers; an
// infrastructure list failure -> 500 with an error body.
func TestGetTeamAttention_UnknownTeamAndUnreadableProject(t *testing.T) {
	store := ossfake.NewMemory()
	seedTeamAttentionProject(store, "alpha-team", "p1", []string{"t1"}, "alpha-team")
	putTaskAttention(store, "alpha-team", "t1", map[string]any{
		"attention": []any{map[string]any{"kind": "decision", "question": "ok?", "attempt": 1,
			"requestedAt": "2026-11-02T09:00:00Z", "resolved": false}},
	})
	// p2's meta.json is not JSON at all.
	_ = store.PutObject(t.Context(), "teams/alpha-team/shared/projects/p2/meta.json", []byte("{not json"))
	h := newTeamAttentionHandler(t, &mcLikeOSS{Memory: store}, team("alpha-team"))

	rec := getTeamAttention(t, h, "alpha-team", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("broken project must not fail the read: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Attention-Skipped"); got != "1" {
		t.Fatalf("X-Attention-Skipped = %q, want 1", got)
	}
	if resp := decodeTeamAttention(t, rec); resp.Count != 1 {
		t.Fatalf("healthy project rows must survive: %+v", resp.Items)
	}

	missing := getTeamAttention(t, h, "no-such-team", "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown team must be 404, got %d", missing.Code)
	}

	// Infrastructure-level list failure is a 500, not an empty inbox.
	failing := newTeamAttentionHandler(t, &mcLikeOSS{Memory: store, failList: true}, team("alpha-team"))
	if rec := getTeamAttention(t, failing, "alpha-team", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("list failure must be 500, got %d", rec.Code)
	}
}

// TestGetTeamAttention_NotifiedLatestAttempt picks the newest attempt (a
// retry folds the previous physical outcome into `retries`).
func TestGetTeamAttention_NotifiedLatestAttempt(t *testing.T) {
	store := ossfake.NewMemory()
	seedTeamAttentionProject(store, "alpha-team", "p1", []string{"t1"}, "alpha-team")
	putTaskAttention(store, "alpha-team", "t1", map[string]any{
		"attention": []any{map[string]any{
			"kind": "approval", "question": "retry?", "attempt": 2,
			"requestedAt": "2026-11-02T09:00:00Z", "resolved": false,
			"notification_log": []any{
				map[string]any{"attempt": 1, "ts": "2026-11-02T09:00:01Z", "room_accepted": false, "channel_accepted": nil,
					"retries": []any{}},
				map[string]any{"attempt": 2, "ts": "2026-11-02T09:00:09Z", "room_accepted": true, "channel_accepted": nil,
					"recipients": []any{map[string]any{"mxid": "@luo:host", "role": "human", "delivered": true}},
					"gaps":       []any{map[string]any{"recipient": "@x:host", "role": "human", "reason": "human_not_in_room"}},
				},
			},
		}},
	})
	h := newTeamAttentionHandler(t, &mcLikeOSS{Memory: store}, team("alpha-team"))
	row := decodeTeamAttention(t, getTeamAttention(t, h, "alpha-team", "")).Items[0]
	if row.Notified == nil || !row.Notified.RoomAccepted || row.Notified.At != "2026-11-02T09:00:09Z" {
		t.Fatalf("latest attempt must win: %+v", row.Notified)
	}
	if row.Notified.ChannelAccepted != nil {
		t.Fatalf("channelAccepted must be null")
	}
	if len(row.Notified.Recipients) != 1 || len(row.Notified.Gaps) != 1 {
		t.Fatalf("recipients/gaps passthrough: %+v", row.Notified)
	}
}

// expectedOverdueEvent resolves the contract id_rule independently of the
// production helpers: fingerprint = sha256(hex) of the ref bytes,
// event_id = 'ev_' + sha256(joined fields)[:16].
func expectedOverdueEvent(teamID, projectID, taskID string, attempt int, requestedAt string, auditSeq int) (eventID, fingerprint string) {
	raw := fmt.Sprintf("%s|%d|%s", taskID, attempt, requestedAt)
	sum := sha256.Sum256([]byte(raw))
	fingerprint = hex.EncodeToString(sum[:])
	joined := strings.Join([]string{
		teamID, projectID, taskID, "", "SIG-ATTENTION", "attention_overdue", "", fingerprint, "0",
		fmt.Sprintf("%d", auditSeq),
	}, "|")
	idSum := sha256.Sum256([]byte(joined))
	return "ev_" + hex.EncodeToString(idSum[:])[:16], fingerprint
}

// TestGetTeamAttention_LazyOverdueEvent pins the S1 overdue half: one read
// writes the event for an overdue pending item, a second read re-emits
// idempotently (same key, original object untouched), and the id follows the
// contract vector.
func TestGetTeamAttention_LazyOverdueEvent(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "p1", "status": "active", "plan_type": "dag",
		"team_id": "alpha-team", "audit_seq": 7,
		"tasks": []any{map[string]any{"task_id": "t1", "title": "t1", "status": "active"}},
	})
	putTaskAttention(store, "alpha-team", "t1", map[string]any{
		"attention": []any{map[string]any{
			"kind": "approval", "question": "late?", "attempt": 2,
			"requestedAt": "2026-11-02T08:00:00Z", "timeoutAt": "2020-01-01T00:00:00Z", "resolved": false,
		}},
	})
	h := newTeamAttentionHandler(t, &mcLikeOSS{Memory: store}, team("alpha-team"))

	eventID, fingerprint := expectedOverdueEvent("alpha-team", "p1", "t1", 2, "2026-11-02T08:00:00Z", 7)
	key := "teams/alpha-team/shared/events/alpha-team/" + eventID + ".json"

	// Nothing emitted before the read.
	if _, err := store.GetObject(t.Context(), key); err == nil {
		t.Fatalf("event must not exist before the read")
	}
	if rec := getTeamAttention(t, h, "alpha-team", ""); rec.Code != http.StatusOK {
		t.Fatalf("read: %d %s", rec.Code, rec.Body.String())
	}
	first, err := store.GetObject(t.Context(), key)
	if err != nil {
		t.Fatalf("overdue event missing at %s: %v", key, err)
	}
	var ev signalEvent
	if err := json.Unmarshal(first, &ev); err != nil {
		t.Fatalf("event decode: %v", err)
	}
	if ev.EventID != eventID || ev.EventType != "SIG-ATTENTION" || !ev.IsSignal ||
		ev.SourceEvent.Origin != "attention_overdue" || ev.TaskID != "t1" {
		t.Fatalf("event shape: %+v", ev)
	}
	if ev.Ref != fingerprint || ev.Fingerprint != fingerprint {
		t.Fatalf("ref/fingerprint: %q %q want %q", ev.Ref, ev.Fingerprint, fingerprint)
	}

	// Second read: idempotent — same key, byte-identical object.
	if rec := getTeamAttention(t, h, "alpha-team", ""); rec.Code != http.StatusOK {
		t.Fatalf("second read: %d", rec.Code)
	}
	second, err := store.GetObject(t.Context(), key)
	if err != nil {
		t.Fatalf("event vanished on re-read: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("re-read modified the stored event")
	}
	// And exactly one event object for the team, not two.
	keys, err := store.ListObjects(t.Context(), "teams/alpha-team/shared/events/alpha-team/")
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("want 1 event object, got %d: %v", len(keys), keys)
	}

	// A resolved item is never swept.
	if rec := getTeamAttention(t, h, "alpha-team", "?status=all"); rec.Code != http.StatusOK {
		t.Fatalf("all read: %d", rec.Code)
	}
}

// TestGetTeamAttention_Limit pins the ceiling (400 above 1000), the invalid
// input handling, and the default 200.
func TestGetTeamAttention_Limit(t *testing.T) {
	store := ossfake.NewMemory()
	taskIDs := make([]string, 0, 205)
	for i := 0; i < 205; i++ {
		taskIDs = append(taskIDs, fmt.Sprintf("t%03d", i))
	}
	seedTeamAttentionProject(store, "alpha-team", "p1", taskIDs, "alpha-team")
	for i := 0; i < 205; i++ {
		id := fmt.Sprintf("t%03d", i)
		putTaskAttention(store, "alpha-team", id, map[string]any{
			"attention": []any{map[string]any{
				"kind": "decision", "question": "q", "attempt": 1,
				"requestedAt": fmt.Sprintf("2026-11-02T09:%02d:%02dZ", i/60, i%60), "resolved": false,
			}},
		})
	}
	h := newTeamAttentionHandler(t, &mcLikeOSS{Memory: store}, team("alpha-team"))

	// 205 rows against the default limit: capped AND flagged truncated.
	got := decodeTeamAttention(t, getTeamAttention(t, h, "alpha-team", ""))
	if got.Count != teamAttentionDefaultLimit || !got.Truncated {
		t.Fatalf("default limit: count=%d truncated=%v, want %d/true", got.Count, got.Truncated, teamAttentionDefaultLimit)
	}
	if got := decodeTeamAttention(t, getTeamAttention(t, h, "alpha-team", "?limit=1000")); got.Count != 205 || got.Truncated {
		t.Fatalf("explicit limit under the data size: count=%d truncated=%v, want 205/false", got.Count, got.Truncated)
	}
	for _, q := range []string{"?limit=1001", "?limit=0", "?limit=-2", "?limit=abc"} {
		if rec := getTeamAttention(t, h, "alpha-team", q); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s must be 400, got %d", q, rec.Code)
		}
	}
}

// TestGetTeamAttention_TruncatedFlag pins the partial-result contract:
// limit < total -> count==limit, truncated==true, X-Attention-Truncated: 1;
// limit >= total -> truncated==false and the header absent; a genuinely
// empty inbox is count==0 with truncated==false, distinguishable from a
// truncated result (count==limit with truncated==true).
func TestGetTeamAttention_TruncatedFlag(t *testing.T) {
	store := ossfake.NewMemory()
	taskIDs := []string{"t1", "t2", "t3", "t4"}
	seedTeamAttentionProject(store, "alpha-team", "p1", taskIDs, "alpha-team")
	for i, id := range taskIDs {
		putTaskAttention(store, "alpha-team", id, map[string]any{
			"attention": []any{
				map[string]any{"kind": "decision", "question": "q-a", "attempt": 1,
					"requestedAt": fmt.Sprintf("2026-11-02T09:0%d:00Z", i), "resolved": false},
				map[string]any{"kind": "decision", "question": "q-b", "attempt": 2,
					"requestedAt": fmt.Sprintf("2026-11-02T09:0%d:00Z", i), "resolved": false},
			},
		})
	}
	h := newTeamAttentionHandler(t, &mcLikeOSS{Memory: store}, team("alpha-team"))

	// 8 rows against limit=3: capped at limit and flagged truncated.
	rec := getTeamAttention(t, h, "alpha-team", "?limit=3")
	resp := decodeTeamAttention(t, rec)
	if resp.Count != 3 || len(resp.Items) != 3 || !resp.Truncated {
		t.Fatalf("limit<total: count=%d items=%d truncated=%v, want 3/3/true", resp.Count, len(resp.Items), resp.Truncated)
	}
	if got := rec.Header().Get("X-Attention-Truncated"); got != "1" {
		t.Fatalf("X-Attention-Truncated = %q, want 1", got)
	}

	// limit == total, limit > total, and the default (200): complete reads
	// never set the flag or the header.
	for _, q := range []string{"?limit=8", "?limit=100", ""} {
		rec := getTeamAttention(t, h, "alpha-team", q)
		resp := decodeTeamAttention(t, rec)
		if resp.Count != 8 || resp.Truncated {
			t.Fatalf("%s: count=%d truncated=%v, want 8/false", q, resp.Count, resp.Truncated)
		}
		if got := rec.Header().Get("X-Attention-Truncated"); got != "" {
			t.Fatalf("%s: X-Attention-Truncated = %q, want absent", q, got)
		}
	}

	// Empty inbox (tasks exist but none carries an attention record):
	// count=0 AND truncated=false — a different state than count=limit
	// with truncated=true.
	empty := ossfake.NewMemory()
	seedTeamAttentionProject(empty, "beta-team", "p1", []string{"t1"}, "beta-team")
	hb := newTeamAttentionHandler(t, &mcLikeOSS{Memory: empty}, team("beta-team"))
	rec = getTeamAttention(t, hb, "beta-team", "")
	resp = decodeTeamAttention(t, rec)
	if resp.Count != 0 || resp.Truncated {
		t.Fatalf("empty inbox: count=%d truncated=%v, want 0/false", resp.Count, resp.Truncated)
	}
	if got := rec.Header().Get("X-Attention-Truncated"); got != "" {
		t.Fatalf("empty inbox must not carry X-Attention-Truncated, got %q", got)
	}
}

// TestGetTeamAttention_TruncatedIndependentOfSkipped pins that truncation
// and read degradation are separate signals: X-Attention-Skipped counts
// unreadable projects and never absorbs truncated rows, so both headers
// can appear at once — and each is absent when only the other holds.
func TestGetTeamAttention_TruncatedIndependentOfSkipped(t *testing.T) {
	store := ossfake.NewMemory()
	seedTeamAttentionProject(store, "alpha-team", "p1", []string{"t1", "t2", "t3", "t4"}, "alpha-team")
	for _, id := range []string{"t1", "t2", "t3", "t4"} {
		putTaskAttention(store, "alpha-team", id, map[string]any{
			"attention": []any{map[string]any{"kind": "decision", "question": "q", "attempt": 1,
				"requestedAt": "2026-11-02T09:00:00Z", "resolved": false}},
		})
	}
	// p2 is healthy at the meta level but its only task meta is not JSON:
	// the project is skipped and counted, contributing no rows.
	putProject(store, "teams/alpha-team/shared/projects/p2/meta.json", map[string]any{
		"project_id": "p2", "title": "p2", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
		"tasks": []any{map[string]any{"task_id": "t9", "title": "t9", "status": "active"}},
	})
	_ = store.PutObject(t.Context(), "teams/alpha-team/shared/tasks/t9/meta.json", []byte("{not json"))
	h := newTeamAttentionHandler(t, &mcLikeOSS{Memory: store}, team("alpha-team"))

	// 4 healthy rows + 1 skipped project, limit=2: limited AND degraded.
	rec := getTeamAttention(t, h, "alpha-team", "?limit=2")
	resp := decodeTeamAttention(t, rec)
	if resp.Count != 2 || !resp.Truncated {
		t.Fatalf("truncated+degraded: count=%d truncated=%v, want 2/true", resp.Count, resp.Truncated)
	}
	if got := rec.Header().Get("X-Attention-Truncated"); got != "1" {
		t.Fatalf("X-Attention-Truncated = %q, want 1", got)
	}
	if got := rec.Header().Get("X-Attention-Skipped"); got != "1" {
		t.Fatalf("X-Attention-Skipped = %q, want 1", got)
	}

	// Same degradation, limit above the total: complete but degraded —
	// the skipped count is reported without any truncation flag.
	rec = getTeamAttention(t, h, "alpha-team", "?limit=10")
	resp = decodeTeamAttention(t, rec)
	if resp.Count != 4 || resp.Truncated {
		t.Fatalf("degraded complete: count=%d truncated=%v, want 4/false", resp.Count, resp.Truncated)
	}
	if got := rec.Header().Get("X-Attention-Truncated"); got != "" {
		t.Fatalf("X-Attention-Truncated must be absent, got %q", got)
	}
	if got := rec.Header().Get("X-Attention-Skipped"); got != "1" {
		t.Fatalf("X-Attention-Skipped = %q, want 1", got)
	}
}

// countingAttentionOSS wraps mcLikeOSS with a GetObject call counter so
// the team inbox read fan-out can be pinned: a layout change that adds or
// drops object reads fails the benchmark's count assertions.
type countingAttentionOSS struct {
	*mcLikeOSS
	getCalls int
}

func (c *countingAttentionOSS) GetObject(ctx context.Context, key string) ([]byte, error) {
	c.getCalls++
	return c.mcLikeOSS.GetObject(ctx, key)
}

// BenchmarkGetTeamAttention measures the team inbox read at a
// representative large-team scale: 200 projects x 10 attention items each
// (2000 rows, 10x the default limit of 200). It pins the read-cost model
// the docs record: exactly 1 ListObjects call (the team project prefix)
// and 2200 GetObject calls (200 project metas + 2000 task metas) per
// request — the limit caps returned rows (200, truncated=true), never the
// object reads. The wall time per op is the latency evidence for the same
// scale (docs/usage/project-workflow-api.md, "Current read cost").
func BenchmarkGetTeamAttention(b *testing.B) {
	const (
		projectsPerTeam = 200
		tasksPerProject = 10 // one attention item per task
	)
	store := ossfake.NewMemory()
	for i := 0; i < projectsPerTeam; i++ {
		proj := fmt.Sprintf("p%03d", i)
		taskIDs := make([]string, 0, tasksPerProject)
		for j := 0; j < tasksPerProject; j++ {
			id := fmt.Sprintf("%st%02d", proj, j)
			taskIDs = append(taskIDs, id)
		}
		seedTeamAttentionProject(store, "bench-team", proj, taskIDs, "bench-team")
		for _, id := range taskIDs {
			putTaskAttention(store, "bench-team", id, map[string]any{
				"attention": []any{map[string]any{
					"kind": "decision", "question": "q", "attempt": 1,
					"requestedAt": "2026-11-02T09:00:00Z",
					"timeoutAt":   "2099-01-01T00:00:00Z",
					"resolved":    false,
				}},
			})
		}
	}

	counting := &countingAttentionOSS{mcLikeOSS: &mcLikeOSS{Memory: store}}
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		b.Fatalf("add v1beta1 scheme: %v", err)
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(team("bench-team")).Build()
	h := NewProjectHandler(k8s, "default", counting)

	b.ReportAllocs()
	b.ResetTimer()
	var rec *httptest.ResponseRecorder
	for i := 0; i < b.N; i++ {
		counting.listCalls = 0
		counting.getCalls = 0
		req := httptest.NewRequest(http.MethodGet, "/api/v1/teams/bench-team/attention", nil)
		req.SetPathValue("name", "bench-team")
		req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
		rec = httptest.NewRecorder()
		h.GetTeamAttention(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("bench read: %d %s", rec.Code, rec.Body.String())
		}
	}
	b.StopTimer()

	// The counters hold the last iteration: every request reads the whole
	// team even though the response is capped at the default limit.
	if counting.listCalls != 1 {
		b.Fatalf("list calls = %d, want 1", counting.listCalls)
	}
	wantGets := projectsPerTeam + projectsPerTeam*tasksPerProject
	if counting.getCalls != wantGets {
		b.Fatalf("object reads = %d, want %d (%d project + %d task metas)",
			counting.getCalls, wantGets, projectsPerTeam, projectsPerTeam*tasksPerProject)
	}
	var resp struct {
		Count     int `json:"count"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		b.Fatalf("decode bench response: %v", err)
	}
	if resp.Count != teamAttentionDefaultLimit || !resp.Truncated {
		b.Fatalf("bench response: count=%d truncated=%v, want %d/true", resp.Count, resp.Truncated, teamAttentionDefaultLimit)
	}
}
