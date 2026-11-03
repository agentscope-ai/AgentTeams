package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// TH-4: the dashboard attention resolve endpoint tests. The bench reuses the
// package's in-memory OSS fake + fake K8s client; the Matrix client is a
// recording test double (the room ✅ line is asserted, not sent).

// --- Matrix test double ---

type resolveMatrixSent struct {
	RoomID string
	Body   string
}

// resolveFakeMatrix records the ✅ lines the resolve handler posts; fail
// makes the send error (the meta write must survive a room failure).
type resolveFakeMatrix struct {
	matrix.Client
	mu   sync.Mutex
	fail bool
	sent []resolveMatrixSent
}

func (f *resolveFakeMatrix) SendMessageAsAdmin(_ context.Context, roomID, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("matrix send failed (test)")
	}
	f.sent = append(f.sent, resolveMatrixSent{RoomID: roomID, Body: body})
	return nil
}

func (f *resolveFakeMatrix) sentMessages() []resolveMatrixSent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]resolveMatrixSent, len(f.sent))
	copy(out, f.sent)
	return out
}

// --- bench ---

func teamWithHumans(name string, humans ...string) *v1beta1.Team {
	spec := v1beta1.TeamSpec{TeamName: name}
	for _, human := range humans {
		spec.HumanMembers = append(spec.HumanMembers, v1beta1.TeamMemberSpec{Name: human})
	}
	return &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spec,
	}
}

// newResolveTestHandlerWithStore builds the resolve bench on the given OSS
// store (mcLikeOSS semantics for project resolution are expected).
func newResolveTestHandlerWithStore(t *testing.T, store oss.StorageClient, fx *resolveFakeMatrix, teams ...*v1beta1.Team) *ProjectHandler {
	t.Helper()
	objs := make([]runtime.Object, 0, len(teams))
	for _, tm := range teams {
		objs = append(objs, tm)
	}
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	return NewProjectHandler(k8s, "default", store, fx)
}

func newResolveTestHandler(t *testing.T, store *ossfake.Memory, fx *resolveFakeMatrix, teams ...*v1beta1.Team) *ProjectHandler {
	return newResolveTestHandlerWithStore(t, &mcLikeOSS{Memory: store}, fx, teams...)
}

const (
	resolveTaskKey      = "teams/alpha-team/shared/tasks/t1/meta.json"
	resolveProjectKey   = "teams/alpha-team/shared/projects/p1/meta.json"
	resolveEventsPrefix = "teams/alpha-team/shared/events/alpha-team/"
)

func pendingDecisionRecord() map[string]any {
	return map[string]any{
		"kind":        "decision",
		"question":    "Ship option A or B?",
		"attempt":     1,
		"requestedAt": "2026-11-03T07:00:00Z",
		"resolved":    false,
		"eventId":     "att-pending-1",
		"options": []any{
			map[string]any{"id": "ship-a", "label": "Ship A"},
			map[string]any{"id": "ship-b", "label": "Ship B"},
		},
		"suggested": "ship-a",
	}
}

func resolveTaskMeta(extra map[string]any) map[string]any {
	meta := map[string]any{
		"task_id":    "t1",
		"project_id": "p1",
		"status":     "in_progress",
		"room_id":    "!task-room:example.org",
	}
	for k, v := range extra {
		meta[k] = v
	}
	return meta
}

// seedResolveBench lays project p1 (task t1 in the DAG) + the task meta with
// the given attention list (nil = no attention records).
func seedResolveBench(t *testing.T, store *ossfake.Memory, attention []any) {
	t.Helper()
	putProject(store, resolveProjectKey, map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
		"tasks": []map[string]any{{"task_id": "t1", "title": "T1", "status": "in_progress", "depends_on": []string{}}},
	})
	putTaskMeta(store, "alpha-team", "t1", resolveTaskMeta(map[string]any{"attention": attention}))
}

var (
	aliceCaller    = &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"}}
	outsiderCaller = &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "mallory", Teams: []string{"alpha-team"}}
)

func resolveAttentionTestRequest(t *testing.T, h *ProjectHandler, projectID, taskID, body string, caller *authpkg.CallerIdentity) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/attention/"+taskID+"/resolve", strings.NewReader(body))
	req.SetPathValue("id", projectID)
	req.SetPathValue("taskId", taskID)
	req = withCaller(req, caller)
	rec := httptest.NewRecorder()
	h.ResolveAttention(rec, req)
	return rec
}

type resolveResp struct {
	TaskID      string `json:"taskId"`
	Resolved    bool   `json:"resolved"`
	Idempotent  bool   `json:"idempotent"`
	ResolvedVia string `json:"resolvedVia"`
	ResolvedBy  string `json:"resolvedBy"`
	ResolvedAt  string `json:"resolvedAt"`
	EventID     string `json:"eventId"`
}

func decodeResolveResp(t *testing.T, rec *httptest.ResponseRecorder) resolveResp {
	t.Helper()
	var resp resolveResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return resp
}

func readResolveCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body.Code
}

func readResolveTaskRecord(t *testing.T, store *ossfake.Memory) map[string]any {
	t.Helper()
	data, err := store.GetObject(context.Background(), resolveTaskKey)
	if err != nil {
		t.Fatalf("read task meta: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("decode task meta: %v", err)
	}
	entries, _ := meta["attention"].([]any)
	if len(entries) != 1 {
		t.Fatalf("attention entries=%d, want 1: %v", len(entries), meta["attention"])
	}
	entry, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("attention[0] is %T", entries[0])
	}
	return entry
}

// resolveEventID is the deterministic event id of a committed resolve (the
// fixed vector of the test: id_rule hash of the stable ref).
func resolveEventID(t *testing.T, taskID string, attempt int, resolution, resolvedAt string, auditSeq int) string {
	t.Helper()
	ref := attentionResolveRef(taskID, attempt, resolution, resolvedAt)
	fp := sha256.Sum256([]byte(ref))
	return signalEventID("alpha-team", "p1", taskID, "", "SIG-ATTENTION", "attention_resolved", nil, hex.EncodeToString(fp[:]), 0, auditSeq)
}

// --- (1) happy path: option id ---

func TestResolveAttention_HappyPathOption(t *testing.T) {
	store := ossfake.NewMemory()
	seedResolveBench(t, store, []any{pendingDecisionRecord()})
	fx := &resolveFakeMatrix{}
	h := newResolveTestHandler(t, store, fx, teamWithHumans("alpha-team", "alice"))

	rec := resolveAttentionTestRequest(t, h, "p1", "t1", `{"resolution":"ship-a","comment":"go with A","evidence":{"freeform":false,"note":"cost analysis wins"}}`, aliceCaller)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeResolveResp(t, rec)
	if resp.TaskID != "t1" || !resp.Resolved || resp.Idempotent {
		t.Fatalf("resp=%+v, want taskId=t1 resolved=true idempotent=false", resp)
	}
	if resp.ResolvedVia != "dashboard" || resp.ResolvedBy != "alice" || resp.ResolvedAt == "" || !strings.HasPrefix(resp.EventID, "ev_") {
		t.Fatalf("provenance resp=%+v, want dashboard/alice/<ts>/ev_*", resp)
	}

	entry := readResolveTaskRecord(t, store)
	if entry["resolved"] != true || entry["resolution"] != "ship-a" || entry["comment"] != "go with A" ||
		entry["resolvedVia"] != "dashboard" || entry["resolvedBy"] != "alice" || entry["resolvedAt"] != resp.ResolvedAt ||
		entry["attempt"] != float64(1) || entry["freeform"] != false {
		t.Fatalf("record=%v", entry)
	}
	ev, ok := entry["resolveEvidence"].(map[string]any)
	if !ok || ev["note"] != "cost analysis wins" || ev["freeform"] != false {
		t.Fatalf("resolveEvidence=%v", entry["resolveEvidence"])
	}

	sent := fx.sentMessages()
	if len(sent) != 1 || sent[0].RoomID != "!task-room:example.org" || sent[0].Body != "✅ 已答复：Ship A（@alice，via dashboard）" {
		t.Fatalf("room messages=%+v, want exactly one ✅ line to the task room", sent)
	}

	wantID := resolveEventID(t, "t1", 1, "ship-a", resp.ResolvedAt, 0)
	if resp.EventID != wantID {
		t.Fatalf("eventId=%s, want deterministic %s", resp.EventID, wantID)
	}
	evBody := readSignalEvent(t, store, resolveEventsPrefix+wantID+".json")
	assertSignalEventShape(t, evBody, "SIG-ATTENTION", attentionResolveRef("t1", 1, "ship-a", resp.ResolvedAt), "t1")
	if evBody["event_id"] != wantID {
		t.Fatalf("stored event_id=%v, want %s", evBody["event_id"], wantID)
	}
}

// --- (2) freeform ---

func TestResolveAttention_Freeform(t *testing.T) {
	// Declared freeform: arbitrary text is a valid resolution.
	store := ossfake.NewMemory()
	seedResolveBench(t, store, []any{pendingDecisionRecord()})
	fx := &resolveFakeMatrix{}
	h := newResolveTestHandler(t, store, fx, teamWithHumans("alpha-team", "alice"))
	rec := resolveAttentionTestRequest(t, h, "p1", "t1", `{"resolution":"defer to Q1","evidence":{"freeform":true}}`, aliceCaller)
	if rec.Code != http.StatusOK {
		t.Fatalf("declared freeform status=%d body=%s", rec.Code, rec.Body.String())
	}
	entry := readResolveTaskRecord(t, store)
	if entry["resolution"] != "defer to Q1" || entry["freeform"] != true {
		t.Fatalf("record=%v, want freeform resolution", entry)
	}
	sent := fx.sentMessages()
	if len(sent) != 1 || sent[0].Body != "✅ 已答复：defer to Q1（@alice，via dashboard）" {
		t.Fatalf("room messages=%+v, want the free text in the ✅ line", sent)
	}

	// Undeclared freeform on a decision kind: not an option id -> 400.
	store2 := ossfake.NewMemory()
	seedResolveBench(t, store2, []any{pendingDecisionRecord()})
	h2 := newResolveTestHandler(t, store2, nil, teamWithHumans("alpha-team", "alice"))
	rec2 := resolveAttentionTestRequest(t, h2, "p1", "t1", `{"resolution":"defer to Q1"}`, aliceCaller)
	if rec2.Code != http.StatusBadRequest || readResolveCode(t, rec2) != "resolution_not_an_option" {
		t.Fatalf("undeclared freeform status=%d code=%s, want 400 resolution_not_an_option", rec2.Code, readResolveCode(t, rec2))
	}
}

// --- (3) human roster gate ---

func TestResolveAttention_RosterGate(t *testing.T) {
	// A human who is in scope but NOT named in team.spec.humanMembers.
	store := ossfake.NewMemory()
	seedResolveBench(t, store, []any{pendingDecisionRecord()})
	h := newResolveTestHandler(t, store, nil, teamWithHumans("alpha-team", "alice"))
	rec := resolveAttentionTestRequest(t, h, "p1", "t1", `{"resolution":"ship-a"}`, outsiderCaller)
	if rec.Code != http.StatusForbidden || readResolveCode(t, rec) != "not_a_human_member" {
		t.Fatalf("outsider status=%d code=%s, want 403 not_a_human_member", rec.Code, readResolveCode(t, rec))
	}
	if strings.Contains(rec.Body.String(), "alice") {
		t.Fatalf("403 body leaks the roster: %s", rec.Body.String())
	}
	// The attention must remain pending (no partial write).
	entry := readResolveTaskRecord(t, store)
	if entry["resolved"] == true {
		t.Fatalf("denied resolve mutated the record: %v", entry)
	}

	// A roster member resolves.
	store2 := ossfake.NewMemory()
	seedResolveBench(t, store2, []any{pendingDecisionRecord()})
	fx := &resolveFakeMatrix{}
	h2 := newResolveTestHandler(t, store2, fx, teamWithHumans("alpha-team", "alice"))
	rec2 := resolveAttentionTestRequest(t, h2, "p1", "t1", `{"resolution":"ship-a"}`, aliceCaller)
	if rec2.Code != http.StatusOK {
		t.Fatalf("roster member status=%d body=%s, want 200", rec2.Code, rec2.Body.String())
	}
}

// --- (4) task not in project ---

func TestResolveAttention_TaskNotInProject(t *testing.T) {
	store := ossfake.NewMemory()
	seedResolveBench(t, store, []any{pendingDecisionRecord()})
	// t2 joins the graph, but its task meta belongs to another project.
	putProject(store, resolveProjectKey, map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "T1", "status": "in_progress", "depends_on": []string{}},
			{"task_id": "t2", "title": "T2", "status": "in_progress", "depends_on": []string{}},
		},
	})
	putTaskMeta(store, "alpha-team", "t2", map[string]any{
		"task_id": "t2", "project_id": "p9", "status": "in_progress",
		"attention": []any{pendingDecisionRecord()},
	})
	h := newResolveTestHandler(t, store, nil, teamWithHumans("alpha-team", "alice"))

	rec := resolveAttentionTestRequest(t, h, "p1", "t9", `{"resolution":"ship-a"}`, aliceCaller)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("absent task status=%d, want 404", rec.Code)
	}
	rec = resolveAttentionTestRequest(t, h, "p1", "t2", `{"resolution":"ship-a"}`, aliceCaller)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign task status=%d, want 404", rec.Code)
	}
}

// --- (5) idempotent re-submit ---

func TestResolveAttention_IdempotentResubmit(t *testing.T) {
	store := ossfake.NewMemory()
	seedResolveBench(t, store, []any{pendingDecisionRecord()})
	fx := &resolveFakeMatrix{}
	h := newResolveTestHandler(t, store, fx, teamWithHumans("alpha-team", "alice"))
	body := `{"resolution":"ship-a","comment":"go with A"}`

	rec1 := resolveAttentionTestRequest(t, h, "p1", "t1", body, aliceCaller)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", rec1.Code, rec1.Body.String())
	}
	resp1 := decodeResolveResp(t, rec1)

	rec2 := resolveAttentionTestRequest(t, h, "p1", "t1", body, aliceCaller)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	resp2 := decodeResolveResp(t, rec2)
	if !resp2.Idempotent {
		t.Fatalf("second resp=%+v, want idempotent=true", resp2)
	}
	if resp2.ResolvedAt != resp1.ResolvedAt || resp2.ResolvedBy != resp1.ResolvedBy || resp2.EventID != resp1.EventID {
		t.Fatalf("idempotent resp must return the original provenance: %+v vs %+v", resp2, resp1)
	}
	if sent := fx.sentMessages(); len(sent) != 1 {
		t.Fatalf("room messages=%d, want exactly 1 (first resolve only): %+v", len(sent), sent)
	}
	keys, err := store.ListObjects(context.Background(), resolveEventsPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("event objects=%v, want exactly 1 (no duplicate)", keys)
	}
}

// --- (6) already resolved / not pending ---

func TestResolveAttention_AlreadyResolved(t *testing.T) {
	resolvedRecord := func(resolution, by string) map[string]any {
		rec := pendingDecisionRecord()
		rec["resolved"] = true
		rec["resolution"] = resolution
		rec["resolvedVia"] = "im_reply"
		rec["resolvedBy"] = by
		rec["resolvedAt"] = "2026-11-02T00:00:00Z"
		return rec
	}

	// A different resolution: 409 attention_already_resolved.
	store := ossfake.NewMemory()
	seedResolveBench(t, store, []any{resolvedRecord("ship-b", "bob")})
	h := newResolveTestHandler(t, store, nil, teamWithHumans("alpha-team", "alice", "bob"))
	rec := resolveAttentionTestRequest(t, h, "p1", "t1", `{"resolution":"ship-a"}`, aliceCaller)
	if rec.Code != http.StatusConflict || readResolveCode(t, rec) != "attention_already_resolved" {
		t.Fatalf("different resolution status=%d code=%s, want 409 attention_already_resolved", rec.Code, readResolveCode(t, rec))
	}

	// The same resolution but a different resolver: still 409 (the provenance
	// pair must match exactly).
	store2 := ossfake.NewMemory()
	seedResolveBench(t, store2, []any{resolvedRecord("ship-a", "bob")})
	h2 := newResolveTestHandler(t, store2, nil, teamWithHumans("alpha-team", "alice", "bob"))
	rec2 := resolveAttentionTestRequest(t, h2, "p1", "t1", `{"resolution":"ship-a"}`, aliceCaller)
	if rec2.Code != http.StatusConflict || readResolveCode(t, rec2) != "attention_already_resolved" {
		t.Fatalf("same resolution different resolver status=%d code=%s, want 409 attention_already_resolved", rec2.Code, readResolveCode(t, rec2))
	}

	// No attention records at all: 409 attention_not_pending.
	store3 := ossfake.NewMemory()
	seedResolveBench(t, store3, nil)
	h3 := newResolveTestHandler(t, store3, nil, teamWithHumans("alpha-team", "alice"))
	rec3 := resolveAttentionTestRequest(t, h3, "p1", "t1", `{"resolution":"ship-a"}`, aliceCaller)
	if rec3.Code != http.StatusConflict || readResolveCode(t, rec3) != "attention_not_pending" {
		t.Fatalf("not pending status=%d code=%s, want 409 attention_not_pending", rec3.Code, readResolveCode(t, rec3))
	}
}

// --- (7) CAS conflict: retry on the latest meta ---

// conflictOSS simulates a concurrent writer landing on the task meta at the
// moment of our conditional write: the first strict write fails with
// precondition-failed after the store has moved to a new version (a racing
// worker meta commit); alwaysFail exhausts the retry budget instead.
type conflictOSS struct {
	*mcLikeOSS
	mu         sync.Mutex
	calls      int
	alwaysFail bool
	external   map[string]any // the "concurrent writer's" fields
}

func (c *conflictOSS) PutObjectIfMatchStrict(ctx context.Context, key string, body []byte, ifMatch string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.alwaysFail {
		return oss.ErrPreconditionFailed
	}
	if c.calls == 1 {
		if data, err := c.Memory.GetObject(ctx, key); err == nil {
			var m map[string]any
			if err := json.Unmarshal(data, &m); err == nil {
				for k, v := range c.external {
					m[k] = v
				}
				if b, err := json.Marshal(m); err == nil {
					_ = c.Memory.PutObject(ctx, key, b)
				}
			}
		}
		return oss.ErrPreconditionFailed
	}
	return c.Memory.PutObjectIfMatchStrict(ctx, key, body, ifMatch)
}

func TestResolveAttention_CASConflictRetriesOnLatest(t *testing.T) {
	store := ossfake.NewMemory()
	seedResolveBench(t, store, []any{pendingDecisionRecord()})
	fx := &resolveFakeMatrix{}
	ossStore := &conflictOSS{mcLikeOSS: &mcLikeOSS{Memory: store}, external: map[string]any{"progress_note": "external update"}}
	h := newResolveTestHandlerWithStore(t, ossStore, fx, teamWithHumans("alpha-team", "alice"))

	rec := resolveAttentionTestRequest(t, h, "p1", "t1", `{"resolution":"ship-a"}`, aliceCaller)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 after the conflict retry", rec.Code, rec.Body.String())
	}
	if ossStore.calls != 2 {
		t.Fatalf("strict writes=%d, want 2 (1 conflict + 1 success)", ossStore.calls)
	}
	// The committed record sits on the LATEST meta: the concurrent field
	// survived, the resolve fields are present.
	var meta map[string]any
	data, err := store.GetObject(context.Background(), resolveTaskKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["progress_note"] != "external update" {
		t.Fatalf("concurrent field clobbered by the retry: %v", meta["progress_note"])
	}
	entry := readResolveTaskRecord(t, store)
	if entry["resolved"] != true || entry["resolution"] != "ship-a" {
		t.Fatalf("record lost in the retry: %v", entry)
	}
}

func TestResolveAttention_CASExhaustion409(t *testing.T) {
	store := ossfake.NewMemory()
	seedResolveBench(t, store, []any{pendingDecisionRecord()})
	ossStore := &conflictOSS{mcLikeOSS: &mcLikeOSS{Memory: store}, alwaysFail: true}
	h := newResolveTestHandlerWithStore(t, ossStore, nil, teamWithHumans("alpha-team", "alice"))

	rec := resolveAttentionTestRequest(t, h, "p1", "t1", `{"resolution":"ship-a"}`, aliceCaller)
	if rec.Code != http.StatusConflict || readResolveCode(t, rec) != "task_changed_during_resolve" {
		t.Fatalf("status=%d code=%s, want 409 task_changed_during_resolve", rec.Code, readResolveCode(t, rec))
	}
	if ossStore.calls != attentionResolveMaxAttempts {
		t.Fatalf("strict writes=%d, want %d (full attempt budget)", ossStore.calls, attentionResolveMaxAttempts)
	}
	entry := readResolveTaskRecord(t, store)
	if entry["resolved"] == true {
		t.Fatalf("exhausted retries mutated the record: %v", entry)
	}
}

// --- (8) size limits ---

func TestResolveAttention_SizeLimits(t *testing.T) {
	// 501 CJK runes: the limit counts characters (rune), not bytes.
	store := ossfake.NewMemory()
	seedResolveBench(t, store, []any{pendingDecisionRecord()})
	h := newResolveTestHandler(t, store, nil, teamWithHumans("alpha-team", "alice"))
	body := `{"resolution":"ship-a","comment":"` + strings.Repeat("字", attentionResolveMaxCommentRunes+1) + `"}`
	rec := resolveAttentionTestRequest(t, h, "p1", "t1", body, aliceCaller)
	if rec.Code != http.StatusBadRequest || readResolveCode(t, rec) != "comment_too_long" {
		t.Fatalf("long comment status=%d code=%s, want 400 comment_too_long", rec.Code, readResolveCode(t, rec))
	}

	// 5KB evidence: the limit counts the raw JSON bytes.
	store2 := ossfake.NewMemory()
	seedResolveBench(t, store2, []any{pendingDecisionRecord()})
	h2 := newResolveTestHandler(t, store2, nil, teamWithHumans("alpha-team", "alice"))
	body2 := `{"resolution":"ship-a","evidence":{"pad":"` + strings.Repeat("x", 5*1024) + `"}}`
	rec2 := resolveAttentionTestRequest(t, h2, "p1", "t1", body2, aliceCaller)
	if rec2.Code != http.StatusBadRequest || readResolveCode(t, rec2) != "evidence_too_large" {
		t.Fatalf("large evidence status=%d code=%s, want 400 evidence_too_large", rec2.Code, readResolveCode(t, rec2))
	}
}

// --- (9) room failure is best-effort ---

type captureLogSink struct {
	logr.LogSink
	mu    sync.Mutex
	lines []string
}

func (c *captureLogSink) Init(logr.RuntimeInfo) {}

func (c *captureLogSink) Enabled(int) bool { return true }

func (c *captureLogSink) Info(_ int, msg string, _ ...any) { c.add(msg) }

func (c *captureLogSink) Error(_ error, msg string, _ ...any) { c.add(msg) }

func (c *captureLogSink) WithValues(...any) logr.LogSink { return c }

func (c *captureLogSink) WithName(string) logr.LogSink { return c }

func (c *captureLogSink) add(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, msg)
}

func (c *captureLogSink) contains(s string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.lines {
		if l == s {
			return true
		}
	}
	return false
}

func TestResolveAttention_RoomFailureDoesNotFail(t *testing.T) {
	store := ossfake.NewMemory()
	seedResolveBench(t, store, []any{pendingDecisionRecord()})
	fx := &resolveFakeMatrix{fail: true}
	sink := &captureLogSink{}
	old := log.Log
	log.SetLogger(logr.New(sink))
	defer log.SetLogger(old)

	h := newResolveTestHandler(t, store, fx, teamWithHumans("alpha-team", "alice"))
	rec := resolveAttentionTestRequest(t, h, "p1", "t1", `{"resolution":"ship-a"}`, aliceCaller)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 (room delivery is best-effort)", rec.Code, rec.Body.String())
	}
	entry := readResolveTaskRecord(t, store)
	if entry["resolved"] != true || entry["resolution"] != "ship-a" {
		t.Fatalf("meta write lost on room failure: %v", entry)
	}
	if !sink.contains("attention resolve notification failed") {
		t.Fatalf("expected the notification warn in the log, got: %v", sink.lines)
	}
}

// --- (10) contract row ---

func TestResolveAttention_ContractRow(t *testing.T) {
	c := loadSignalContract(t)
	em := signalEmitters["ResolveAttention"]
	if em.SourceEvent != "attention_resolved" || em.EventType != "SIG-ATTENTION" || em.Origin != "attention_resolved" || em.Level != "info" {
		t.Fatalf("emitter=%+v, want attention_resolved/SIG-ATTENTION/attention_resolved/info", em)
	}
	rowFound := false
	for _, row := range c.Events {
		inRow := false
		for _, se := range row.SourceEvents {
			if se == "attention_resolved" {
				inRow = true
			}
		}
		if !inRow {
			continue
		}
		rowFound = true
		for _, e := range row.Emitters {
			if e == "go:ResolveAttention" {
				return
			}
		}
		t.Fatalf("attention_resolved row emitters=%v, want go:ResolveAttention", row.Emitters)
	}
	if !rowFound {
		t.Fatal("contract has no attention_resolved row")
	}
}

// --- (11) real HTTP chain: route wiring + worker denial ---

func TestResolveAttention_HTTPChainRouteAndWorkerDeny(t *testing.T) {
	store := ossfake.NewMemory()
	seedResolveBench(t, store, []any{pendingDecisionRecord()})
	fx := &resolveFakeMatrix{}
	var ossStore oss.StorageClient = &mcLikeOSS{Memory: store}
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(teamWithHumans("alpha-team", "alice")).Build()

	// The roster human through the real RequireAuthz chain.
	mw := authpkg.NewMiddleware(&roleAuthenticator{identity: aliceCaller}, nil, authpkg.NewAuthorizer(), k8s, "default")
	srv := NewHTTPServer(":0", ServerDeps{Client: k8s, Namespace: "default", OSS: ossStore, AuthMw: mw, MatrixClient: fx})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/attention/t1/resolve", strings.NewReader(`{"resolution":"ship-a"}`))
	req.Header.Set("Authorization", "Bearer sa-token")
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chain status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if sent := fx.sentMessages(); len(sent) != 1 {
		t.Fatalf("room messages=%d, want 1", len(sent))
	}

	// The worker: the authorizer denies ActionUpdate + "project" BEFORE the
	// handler runs (the cancel/pause precedent).
	worker := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}
	mwW := authpkg.NewMiddleware(&roleAuthenticator{identity: worker}, nil, authpkg.NewAuthorizer(), k8s, "default")
	srvW := NewHTTPServer(":0", ServerDeps{Client: k8s, Namespace: "default", OSS: ossStore, AuthMw: mwW})
	reqW := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/attention/t1/resolve", strings.NewReader(`{"resolution":"ship-a"}`))
	reqW.Header.Set("Authorization", "Bearer sa-token")
	recW := httptest.NewRecorder()
	srvW.Mux.ServeHTTP(recW, reqW)
	if recW.Code != http.StatusForbidden {
		t.Fatalf("worker status=%d, want 403 (authorizer denial)", recW.Code)
	}
}
