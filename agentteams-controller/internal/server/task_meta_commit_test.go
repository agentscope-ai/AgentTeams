package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// #1471: the task meta conditional commit is the task-level counterpart of
// the project meta commit (#1402). A worker write-back mirrors the WHOLE task
// record, so the Controller's cancellation (status=cancelled plus its fields
// and the resolved continuation) must be server-owned, and the commit must be
// bound to the version the caller read.

const taskMetaKey = "teams/alpha-team/shared/tasks/t1/meta.json"

func TestSameJSONValueObjectOrder(t *testing.T) {
	for _, tc := range []struct {
		name, a, b string
		want       bool
	}{
		{"object order", `{"verdict":"pass","reviewer":"leader"}`, `{"reviewer":"leader","verdict":"pass"}`, true},
		{"nested order", `{"scorecard":{"b":2,"a":1}}`, ` { "scorecard": {"a":1,"b":2} } `, true},
		{"changed verdict", `{"verdict":"pass"}`, `{"verdict":"fail"}`, false},
		{"array order", `[1,2]`, `[2,1]`, false},
		{"large integers", `9007199254740992`, `9007199254740993`, false},
		{"value type", `1`, `"1"`, false},
		{"trailing JSON", `1 2`, `1`, false},
		{"malformed", `{`, `{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameJSONValue(json.RawMessage(tc.a), json.RawMessage(tc.b)); got != tc.want {
				t.Fatalf("sameJSONValue(%s, %s)=%v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestPutTaskMeta_WorkerPreservesReorderedReview(t *testing.T) {
	store := ossfake.NewMemory()
	base := `{"task_id":"t1","project_id":"p1","status":"submitted","review":{"verdict":"pass","reviewer":"leader"}}`
	if err := store.PutObject(context.Background(), taskMetaKey, []byte(base)); err != nil {
		t.Fatal(err)
	}
	h := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
	worker := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}
	for _, tc := range []struct {
		verdict string
		want    int
	}{
		{"pass", http.StatusOK},
		{"fail", http.StatusForbidden},
	} {
		body := `{"task_id":"t1","project_id":"p1","status":"submitted","review":{"reviewer":"leader","verdict":"` + tc.verdict + `"},"attention":[{"kind":"approval","attempt":1,"resolved":false}]}`
		rec := putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), body, worker)
		if rec.Code != tc.want {
			t.Fatalf("verdict=%s -> %d %s, want %d", tc.verdict, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

func taskMeta(extra map[string]any) map[string]any {
	meta := map[string]any{
		"task_id":    "t1",
		"project_id": "p1",
		"status":     "in_progress",
		"room_id":    "!room:example.org",
	}
	for key, value := range extra {
		meta[key] = value
	}
	return meta
}

func taskMetaBody(meta map[string]any) string {
	raw, err := json.Marshal(meta)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func putTaskMetaRequest(t *testing.T, h *ProjectHandler, taskID, ifMatch, body string, caller *authpkg.CallerIdentity) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/tasks/"+taskID+"/meta", strings.NewReader(body))
	req.SetPathValue("id", taskID)
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	req = withCaller(req, caller)
	rec := httptest.NewRecorder()
	h.PutTaskMeta(rec, req)
	return rec
}

// putTaskMetaCreateRequest is the create-if-absent variant (If-None-Match: *);
// team optionally adds the ?team= qualifier.
func putTaskMetaCreateRequest(t *testing.T, h *ProjectHandler, taskID, team, body string, caller *authpkg.CallerIdentity) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/v1/tasks/" + taskID + "/meta"
	if team != "" {
		path += "?team=" + team
	}
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	req.SetPathValue("id", taskID)
	req.Header.Set("If-None-Match", "*")
	req = withCaller(req, caller)
	rec := httptest.NewRecorder()
	h.PutTaskMeta(rec, req)
	return rec
}

func TestPutTaskMeta_RequiresIfMatch(t *testing.T) {
	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, taskMeta(nil))
	h := newProjectTestHandler(t, store, team("alpha-team"))

	rec := putTaskMetaRequest(t, h, "t1", "", taskMetaBody(taskMeta(nil)), nil)

	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("status=%d, want 428; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPutTaskMeta_UnknownTask404(t *testing.T) {
	store := ossfake.NewMemory()
	h := newProjectTestHandler(t, store, team("alpha-team"))

	rec := putTaskMetaRequest(t, h, "missing", "etag", taskMetaBody(taskMeta(nil)), nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPutTaskMeta_AmbiguousAcrossTeams(t *testing.T) {
	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, taskMeta(nil))
	putTask(store, "teams/beta-team/shared/tasks/t1/meta.json", taskMeta(nil))
	h := newProjectTestHandler(t, store, team("alpha-team"), team("beta-team"))

	rec := putTaskMetaRequest(t, h, "t1", "etag", taskMetaBody(taskMeta(nil)), nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "ambiguous_task") {
		t.Fatalf("status=%d body=%s, want 409 ambiguous_task", rec.Code, rec.Body.String())
	}

	beta, err := store.StatMeta(context.Background(), "teams/beta-team/shared/tasks/t1/meta.json")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/tasks/t1/meta?team=beta-team", strings.NewReader(taskMetaBody(taskMeta(map[string]any{"progress": "beta"}))))
	req.SetPathValue("id", "t1")
	req.Header.Set("If-Match", beta.ETag)
	rec = httptest.NewRecorder()
	h.PutTaskMeta(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("qualified request status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	alphaStored, err := store.GetObject(context.Background(), taskMetaKey)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(alphaStored), `"beta"`) {
		t.Fatalf("qualified commit wrote alpha-team's task: %s", alphaStored)
	}
}

func TestPutTaskMeta_WorkerTeamScope(t *testing.T) {
	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, taskMeta(nil))
	h := newProjectTestHandler(t, store, team("alpha-team"))
	read, err := store.StatMeta(context.Background(), taskMetaKey)
	if err != nil {
		t.Fatal(err)
	}

	own := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}
	rec := putTaskMetaRequest(t, h, "t1", read.ETag, taskMetaBody(taskMeta(map[string]any{"progress": "half"})), own)
	if rec.Code != http.StatusOK {
		t.Fatalf("own-team status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	other := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "beta-dev", Team: "beta-team"}
	rec = putTaskMetaRequest(t, h, "t1", read.ETag, taskMetaBody(taskMeta(nil)), other)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-team status=%d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPutTaskMeta_StaleETagIsAConflict(t *testing.T) {
	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, taskMeta(nil))
	h := newProjectTestHandler(t, store, team("alpha-team"))
	stale, err := store.StatMeta(context.Background(), taskMetaKey)
	if err != nil {
		t.Fatal(err)
	}
	// A concurrent writer lands between the caller's read and its commit.
	putTask(store, taskMetaKey, taskMeta(map[string]any{"progress": "concurrent"}))

	rec := putTaskMetaRequest(t, h, "t1", stale.ETag, taskMetaBody(taskMeta(map[string]any{"progress": "mine"})), nil)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Code != "conflict" {
		t.Fatalf("409 body=%s, want code=conflict", rec.Body.String())
	}
}

func TestPutTaskMeta_RejectsNonCanonicalStatus(t *testing.T) {
	for _, status := range []string{" cancelled ", "CANCELLED", "in progress", ""} {
		store := ossfake.NewMemory()
		putTask(store, taskMetaKey, taskMeta(map[string]any{"status": "cancelled"}))
		h := newProjectTestHandler(t, store, team("alpha-team"))
		read, err := store.StatMeta(context.Background(), taskMetaKey)
		if err != nil {
			t.Fatal(err)
		}

		rec := putTaskMetaRequest(t, h, "t1", read.ETag, taskMetaBody(taskMeta(map[string]any{"status": status})), nil)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %q -> %d, want 400; body=%s", status, rec.Code, rec.Body.String())
		}
	}
}

func TestPutTaskMeta_CancelledTaskIsTerminal(t *testing.T) {
	cancelled := taskMeta(map[string]any{
		"status":        "cancelled",
		"cancel_reason": "operator stop",
		"cancelled_at":  "2026-11-01T00:00:00Z",
		"continuation": map[string]any{
			"status":      "resolved",
			"resolution":  "cancelled",
			"resolved_at": "2026-11-01T00:00:00Z",
		},
	})
	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, cancelled)
	h := newProjectTestHandler(t, store, team("alpha-team"))
	read, err := store.StatMeta(context.Background(), taskMetaKey)
	if err != nil {
		t.Fatal(err)
	}

	// Reopening the status is refused for every caller.
	reopened := taskMeta(map[string]any{
		"status":        "submitted",
		"cancel_reason": "operator stop",
		"cancelled_at":  "2026-11-01T00:00:00Z",
	})
	for _, caller := range []*authpkg.CallerIdentity{
		{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"},
		{Role: authpkg.RoleAdmin, Username: "admin"},
	} {
		rec := putTaskMetaRequest(t, h, "t1", read.ETag, taskMetaBody(reopened), caller)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("reopen as %s -> %d, want 403; body=%s", caller.Role, rec.Code, rec.Body.String())
		}
	}

	// Dropping the cancellation fields is refused.
	withoutReason := taskMeta(map[string]any{"status": "cancelled", "cancelled_at": "2026-11-01T00:00:00Z"})
	rec := putTaskMetaRequest(t, h, "t1", read.ETag, taskMetaBody(withoutReason), nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "cancel_reason") {
		t.Fatalf("drop cancel_reason -> %d %s, want 403 naming the field", rec.Code, rec.Body.String())
	}

	// Reopening the resolved continuation is refused.
	reopenedContinuation := taskMeta(map[string]any{
		"status":        "cancelled",
		"cancel_reason": "operator stop",
		"cancelled_at":  "2026-11-01T00:00:00Z",
		"continuation":  map[string]any{"status": "pending"},
	})
	rec = putTaskMetaRequest(t, h, "t1", read.ETag, taskMetaBody(reopenedContinuation), nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "continuation") {
		t.Fatalf("reopen continuation -> %d %s, want 403 naming the continuation", rec.Code, rec.Body.String())
	}

	// A progress-only commit that keeps the cancellation intact still lands.
	progress := taskMeta(map[string]any{
		"status":        "cancelled",
		"cancel_reason": "operator stop",
		"cancelled_at":  "2026-11-01T00:00:00Z",
		"continuation": map[string]any{
			"status":      "resolved",
			"resolution":  "cancelled",
			"resolved_at": "2026-11-01T00:00:00Z",
		},
		"progress_note": "stopped cleanly",
	})
	rec = putTaskMetaRequest(t, h, "t1", read.ETag, taskMetaBody(progress), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("progress commit -> %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPutTaskMeta_OmittingStatusIsRejected(t *testing.T) {
	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, taskMeta(nil))
	h := newProjectTestHandler(t, store, team("alpha-team"))
	read, err := store.StatMeta(context.Background(), taskMetaKey)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"task_id": "t1", "project_id": "p1", "progress": "x"}

	rec := putTaskMetaRequest(t, h, "t1", read.ETag, taskMetaBody(body), nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPutTaskMeta_HTTPChainRegistersRouteAndWorkerWrites(t *testing.T) {
	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, taskMeta(nil))
	k8s := fake.NewClientBuilder().
		WithScheme(newProjectTestScheme(t)).
		WithRuntimeObjects(team("alpha-team")).
		Build()
	worker := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}
	mw := authpkg.NewMiddleware(&roleAuthenticator{identity: worker}, nil, authpkg.NewAuthorizer(), k8s, "default")
	var ossStore oss.StorageClient = &mcLikeOSS{Memory: store}
	srv := NewHTTPServer(":0", ServerDeps{Client: k8s, Namespace: "default", OSS: ossStore, AuthMw: mw})

	read, err := store.StatMeta(context.Background(), taskMetaKey)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/tasks/t1/meta", strings.NewReader(taskMetaBody(taskMeta(map[string]any{"progress": "chain"}))))
	req.Header.Set("Authorization", "Bearer sa-token")
	req.Header.Set("If-Match", read.ETag)
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	stored, err := store.GetObject(context.Background(), taskMetaKey)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(stored, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["progress"] != "chain" {
		t.Fatalf("stored meta=%v, want progress=chain", meta)
	}

	// The commit endpoint must not open the ordinary task CRUD surface.
	req = httptest.NewRequest(http.MethodPut, "/api/v1/tasks/t1", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sa-token")
	rec = httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /api/v1/tasks/t1 -> %d, want 404/405 (no generic task write)", rec.Code)
	}

	// The create path (If-None-Match: *) runs through the same chain: the
	// first creation of a task meta is conditional too.
	createBody := taskMetaBody(map[string]any{
		"task_id":    "t2",
		"project_id": "p1",
		"status":     "in_progress",
	})
	req = httptest.NewRequest(http.MethodPut, "/api/v1/tasks/t2/meta?team=alpha-team", strings.NewReader(createBody))
	req.Header.Set("Authorization", "Bearer sa-token")
	req.Header.Set("If-None-Match", "*")
	rec = httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	stored, err = store.GetObject(context.Background(), "teams/alpha-team/shared/tasks/t2/meta.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != createBody {
		t.Fatalf("stored create body=%s, want verbatim %s", stored, createBody)
	}
}

// =====================================================================
// #1467 P1b — W2 decision authority on the task-meta commit boundary
// =====================================================================
//
// The ETag/CAS binds a commit to the version the caller read, but not to a
// decision authority: a same-team worker holding a valid bearer could
// rewrite the trusted W2 decision/approval metadata (decision_class,
// decision_status + history, review, the approval attention records) and a
// plain leader's accept_task_result would then pass without any human
// approval. These tests pin the commit boundary's per-caller-class rules:
// a worker reports but never decides; an approval close must name a real
// human (the authenticated human themself, or a team-roster human for
// leader/operator commits); self-approval is forbidden; an unfreeze needs a
// newly closed approval; a standalone task closes only via manager/admin.

const standaloneTaskMetaKey = "shared/tasks/t1/meta.json"

func taskMetaETag(t *testing.T, store *ossfake.Memory, key string) string {
	t.Helper()
	read, err := store.StatMeta(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return read.ETag
}

// teamWithRoster is a Team CR carrying a human roster: two members (luo,
// zhang) and the team admin — the identities a leader/operator close may
// name as the human decider.
func teamWithRoster(name string) *v1beta1.Team {
	tm := team(name)
	tm.Spec.HumanMembers = []v1beta1.TeamMemberSpec{
		{Name: "luo", MatrixUserID: "@luo:example.test"},
		{Name: "zhang", MatrixUserID: "@zhang:example.test"},
	}
	tm.Spec.Admin = &v1beta1.TeamAdminSpec{Name: "ops-admin", MatrixUserID: "@ops-admin:example.test"}
	return tm
}

// humanCR is a Human CR pinning the Matrix identity of a human member.
func humanCR(name, matrixUserID string) *v1beta1.Human {
	return &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1beta1.HumanSpec{DisplayName: name, PermissionLevel: 2},
		Status:     v1beta1.HumanStatus{MatrixUserID: matrixUserID},
	}
}

// approvalTaskMeta is the P1 fixture: a submitted result, frozen by the W2
// stall chain, with an OPEN approval attention record raised by the
// assignee. The r1 gate reads exactly this record.
func approvalTaskMeta() map[string]any {
	return taskMeta(map[string]any{
		"status":          "submitted",
		"decision_status": "frozen",
		"assigned_to":     "@alpha-dev:example.test",
		"attention": []any{
			map[string]any{
				"kind":        "approval",
				"question":    "approve the submitted result?",
				"attempt":     1,
				"requestedAt": "2026-11-01T10:00:00Z",
				"resolved":    false,
			},
		},
	})
}

// approvalTaskMetaOpen is approvalTaskMeta with the decision sub-state
// still open (the close under test is not entangled with an unfreeze).
func approvalTaskMetaOpen() map[string]any {
	meta := approvalTaskMeta()
	meta["decision_status"] = "open"
	return meta
}

// closedApprovalBody returns a copy of meta with its approval attention
// record closed by the given decider actor and channel.
func closedApprovalBody(t *testing.T, meta map[string]any, actor, via string) map[string]any {
	t.Helper()
	out := map[string]any{}
	for key, value := range meta {
		out[key] = value
	}
	closed := []any{}
	for _, item := range meta["attention"].([]any) {
		rec := item.(map[string]any)
		if rec["kind"] == "approval" {
			rec = map[string]any{
				"kind":          "approval",
				"question":      "approve the submitted result?",
				"attempt":       1,
				"requestedAt":   "2026-11-01T10:00:00Z",
				"resolved":      true,
				"resolvedRole":  "human",
				"resolvedVia":   via,
				"resolvedActor": actor,
				"resolvedAt":    "2026-11-01T11:00:00Z",
			}
		}
		closed = append(closed, rec)
	}
	out["attention"] = closed
	return out
}

// 1. P1 regression: a same-team worker forges the approval markers on the
// open approval record — refused, and the stored meta stays byte-for-byte.
func TestPutTaskMeta_WorkerCannotForgeApprovalResolution(t *testing.T) {
	base := approvalTaskMeta()
	forged := closedApprovalBody(t, base, "human:@luo:example.test", "dashboard")

	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, base)
	h := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
	original, err := store.GetObject(context.Background(), taskMetaKey)
	if err != nil {
		t.Fatal(err)
	}

	worker := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}
	rec := putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(forged), worker)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "worker may not resolve an approval record") {
		t.Fatalf("body=%s, want the worker approval-resolution refusal", rec.Body.String())
	}
	after, err := store.GetObject(context.Background(), taskMetaKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("stored meta changed after the refused commit:\nbefore=%s\nafter =%s", original, after)
	}
}

// 2. A worker may raise (add) an OPEN approval record, never a resolved one.
func TestPutTaskMeta_WorkerApprovalRecordAddRules(t *testing.T) {
	base := approvalTaskMeta()

	addClosed := map[string]any{}
	for key, value := range base {
		addClosed[key] = value
	}
	addClosed["attention"] = append(base["attention"].([]any), map[string]any{
		"kind":          "approval",
		"question":      "a second approval",
		"attempt":       2,
		"requestedAt":   "2026-11-01T10:05:00Z",
		"resolved":      true,
		"resolvedRole":  "human",
		"resolvedVia":   "dashboard",
		"resolvedActor": "human:luo",
	})

	addOpen := map[string]any{}
	for key, value := range base {
		addOpen[key] = value
	}
	addOpen["attention"] = append(base["attention"].([]any), map[string]any{
		"kind":        "approval",
		"question":    "a second approval",
		"attempt":     2,
		"requestedAt": "2026-11-01T10:05:00Z",
		"resolved":    false,
	})

	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, base)
	h := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
	worker := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}

	rec := putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(addClosed), worker)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "worker may not resolve an approval record") {
		t.Fatalf("adding a RESOLVED approval record -> %d %s, want 403", rec.Code, rec.Body.String())
	}

	// The store is untouched by the refused commit: the open-record add
	// (an ordinary raise) lands with the same ETag.
	rec = putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(addOpen), worker)
	if rec.Code != http.StatusOK {
		t.Fatalf("adding an OPEN approval record -> %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// 3. The attention list is append-only: a worker may not delete a record.
func TestPutTaskMeta_WorkerCannotDeleteAttentionRecord(t *testing.T) {
	base := approvalTaskMeta()
	deleted := map[string]any{}
	for key, value := range base {
		deleted[key] = value
	}
	deleted["attention"] = []any{}

	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, base)
	h := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
	worker := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}

	rec := putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(deleted), worker)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "worker may not change the W2 decision fields") {
		t.Fatalf("deleting the approval record -> %d %s, want 403", rec.Code, rec.Body.String())
	}
}

// 4. decision_class and the review verdict are decision-authority owned:
// a worker keeps them verbatim.
func TestPutTaskMeta_WorkerDecisionClassAndReview(t *testing.T) {
	base := taskMeta(map[string]any{
		"decision_class": "r2",
		"review":         map[string]any{"verdict": "pass", "reviewer": "alpha-lead"},
	})
	changedClass := map[string]any{}
	for key, value := range base {
		changedClass[key] = value
	}
	changedClass["decision_class"] = "r5"

	changedReview := map[string]any{}
	for key, value := range base {
		changedReview[key] = value
	}
	changedReview["review"] = map[string]any{"verdict": "fail", "reviewer": "alpha-lead"}

	unchanged := map[string]any{}
	for key, value := range base {
		unchanged[key] = value
	}
	unchanged["progress"] = "done"

	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, base)
	h := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
	worker := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}

	for _, tc := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"changed decision_class", changedClass, http.StatusForbidden},
		{"changed review", changedReview, http.StatusForbidden},
		{"decision fields kept verbatim", unchanged, http.StatusOK},
	} {
		rec := putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(tc.body), worker)
		if rec.Code != tc.want {
			t.Fatalf("%s -> %d %s, want %d", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

// 5. Worker decision_status: frozen is sticky; open may escalate.
func TestPutTaskMeta_WorkerDecisionStatus(t *testing.T) {
	worker := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}

	// frozen -> open: refused (a frozen task leaves only by an authorized
	// approval close, which a worker cannot perform at all).
	frozen := taskMeta(map[string]any{"decision_status": "frozen"})
	toOpen := map[string]any{}
	for key, value := range frozen {
		toOpen[key] = value
	}
	toOpen["decision_status"] = "open"
	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, frozen)
	h := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
	rec := putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(toOpen), worker)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "worker may not change the W2 decision fields") {
		t.Fatalf("frozen -> open as worker -> %d %s, want 403", rec.Code, rec.Body.String())
	}

	// open -> frozen: an escalation a worker may record.
	open := taskMeta(map[string]any{"decision_status": "open"})
	toFrozen := map[string]any{}
	for key, value := range open {
		toFrozen[key] = value
	}
	toFrozen["decision_status"] = "frozen"
	store = ossfake.NewMemory()
	putTask(store, taskMetaKey, open)
	h = newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
	rec = putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(toFrozen), worker)
	if rec.Code != http.StatusOK {
		t.Fatalf("open -> frozen as worker -> %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// 6. Worker status lane: a worker submits, never completes or reopens.
func TestPutTaskMeta_WorkerStatusTransition(t *testing.T) {
	worker := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}

	// submitted -> completed: refused.
	submitted := approvalTaskMetaOpen()
	completed := map[string]any{}
	for key, value := range submitted {
		completed[key] = value
	}
	completed["status"] = "completed"
	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, submitted)
	h := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
	rec := putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(completed), worker)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "worker may not complete or reopen a task") {
		t.Fatalf("submitted -> completed as worker -> %d %s, want 403", rec.Code, rec.Body.String())
	}

	// in_progress -> submitted: the worker's own lane.
	inProgress := taskMeta(nil)
	submittedBody := map[string]any{}
	for key, value := range inProgress {
		submittedBody[key] = value
	}
	submittedBody["status"] = "submitted"
	store = ossfake.NewMemory()
	putTask(store, taskMetaKey, inProgress)
	h = newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
	rec = putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(submittedBody), worker)
	if rec.Code != http.StatusOK {
		t.Fatalf("in_progress -> submitted as worker -> %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// 7. decision_status_history is append-only for a worker: existing entries
// are kept verbatim, new entries may be appended.
func TestPutTaskMeta_WorkerDecisionHistory(t *testing.T) {
	worker := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}
	history := []any{
		map[string]any{"ts": "2026-11-01T09:00:00Z", "from": "open", "to": "blocked", "reason": "stall threshold", "source": "attention:approval"},
	}
	base := taskMeta(map[string]any{
		"decision_status":         "blocked",
		"decision_status_history": history,
	})

	rewritten := map[string]any{}
	for key, value := range base {
		rewritten[key] = value
	}
	rewritten["decision_status_history"] = []any{
		map[string]any{"ts": "2026-11-01T09:00:00Z", "from": "open", "to": "blocked", "reason": "REWRITTEN", "source": "attention:approval"},
	}

	appended := map[string]any{}
	for key, value := range base {
		appended[key] = value
	}
	appended["decision_status_history"] = append(history,
		map[string]any{"ts": "2026-11-01T09:30:00Z", "from": "blocked", "to": "frozen", "reason": "stall threshold", "source": "attention:approval"})

	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, base)
	h := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))

	rec := putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(rewritten), worker)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "worker may not rewrite decision_status_history") {
		t.Fatalf("rewriting a history entry -> %d %s, want 403", rec.Code, rec.Body.String())
	}

	// The refused commit left the store untouched: the append lands.
	rec = putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(appended), worker)
	if rec.Code != http.StatusOK {
		t.Fatalf("appending a history entry -> %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// 8. Positive control: the authenticated human closes the approval in their
// own name (and unfreezes); the same body from another human is refused.
func TestPutTaskMeta_HumanApprovalSelfControl(t *testing.T) {
	base := approvalTaskMeta()
	body := closedApprovalBody(t, base, "human:luo", "im_reply")
	body["decision_status"] = "open"

	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, base)
	k8s := fake.NewClientBuilder().
		WithScheme(newProjectTestScheme(t)).
		WithRuntimeObjects(teamWithRoster("alpha-team"), humanCR("luo", "@luo:example.test")).
		Build()
	var ossStore oss.StorageClient = &mcLikeOSS{Memory: store}
	h := NewProjectHandler(k8s, "default", ossStore)

	luo := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "luo", Teams: []string{"alpha-team"}}
	rec := putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(body), luo)
	if rec.Code != http.StatusOK {
		t.Fatalf("the human closes in their own name -> %d %s, want 200", rec.Code, rec.Body.String())
	}

	// The SAME body from a different human (the actor names luo, not
	// zhang): refused — a human commits only their own decision.
	putTask(store, taskMetaKey, base) // fresh open approval, fresh ETag
	zhang := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "zhang", Teams: []string{"alpha-team"}}
	rec = putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(body), zhang)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "human approval must be committed by the human it names") {
		t.Fatalf("another human commits luo's decision -> %d %s, want 403", rec.Code, rec.Body.String())
	}
}

// 9. A leader's close names a real human of the team roster — never a
// stranger, never the assignee (self-approval).
func TestPutTaskMeta_LeaderApprovalRoster(t *testing.T) {
	base := approvalTaskMetaOpen()
	leader := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"}

	// (a) a roster human (zhang) is a legitimate decider.
	bodyA := closedApprovalBody(t, base, "human:zhang", "im_reply")
	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, base)
	h := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
	rec := putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(bodyA), leader)
	if rec.Code != http.StatusOK {
		t.Fatalf("leader close naming a roster human -> %d %s, want 200", rec.Code, rec.Body.String())
	}

	// (b) a stranger is not on the roster.
	putTask(store, taskMetaKey, base)
	bodyB := closedApprovalBody(t, base, "human:stranger", "im_reply")
	rec = putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(bodyB), leader)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "approval provenance") {
		t.Fatalf("leader close naming a stranger -> %d %s, want 403", rec.Code, rec.Body.String())
	}

	// (c) the assignee is a roster human, but self-approval is forbidden.
	selfBase := approvalTaskMetaOpen()
	selfBase["assigned_to"] = "@zhang:example.test"
	putTask(store, taskMetaKey, selfBase)
	bodyC := closedApprovalBody(t, selfBase, "human:zhang", "im_reply")
	rec = putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(bodyC), leader)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "self-approval") {
		t.Fatalf("leader close naming the assignee -> %d %s, want 403 (self-approval)", rec.Code, rec.Body.String())
	}
}

// 10. Unfreeze binding: leaving decision_status=frozen requires a newly
// closed (provenance-valid) approval record in the same commit.
func TestPutTaskMeta_UnfreezeRequiresNewApproval(t *testing.T) {
	base := approvalTaskMeta() // frozen, open approval
	leader := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"}
	human := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "luo", Teams: []string{"alpha-team"}}

	noClose := map[string]any{}
	for key, value := range base {
		noClose[key] = value
	}
	noClose["decision_status"] = "open"

	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, base)
	h := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))

	rec := putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(noClose), leader)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "unfreezing a frozen task requires a newly resolved approval record") {
		t.Fatalf("leader unfreeze without a close -> %d %s, want 403", rec.Code, rec.Body.String())
	}

	putTask(store, taskMetaKey, base) // fresh store for the human leg
	rec = putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(noClose), human)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "unfreezing a frozen task requires a newly resolved approval record") {
		t.Fatalf("human unfreeze without a close -> %d %s, want 403", rec.Code, rec.Body.String())
	}

	// With a valid close in the same commit, the unfreeze lands.
	withClose := closedApprovalBody(t, base, "human:luo", "dashboard")
	withClose["decision_status"] = "open"
	putTask(store, taskMetaKey, base)
	rec = putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(withClose), leader)
	if rec.Code != http.StatusOK {
		t.Fatalf("leader unfreeze WITH a valid close -> %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// 11. Standalone (no team, no roster): a leader cannot close at all
// (fail closed — via the HTTP surface the team boundary already hides the
// task as 404), a manager can.
func TestPutTaskMeta_StandaloneApprovalClose(t *testing.T) {
	base := approvalTaskMetaOpen()
	body := closedApprovalBody(t, base, "human:ops-human", "im_command")

	store := ossfake.NewMemory()
	putTask(store, standaloneTaskMetaKey, base)
	h := newProjectTestHandler(t, store) // no Team CRs: the task is standalone
	leader := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"}
	manager := &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"}

	// Via the HTTP surface a leader never sees a standalone task (the
	// anti-probing 404 fires before any field check).
	rec := putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, standaloneTaskMetaKey), taskMetaBody(body), leader)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("leader on a standalone task -> %d %s, want 404 (hidden by the team boundary)", rec.Code, rec.Body.String())
	}

	// ...while a manager may close it (no roster to check; the markers and
	// the self-approval rule still apply).
	rec = putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, standaloneTaskMetaKey), taskMetaBody(body), manager)
	if rec.Code != http.StatusOK {
		t.Fatalf("manager close on a standalone task -> %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// 12. The Go status-transition projection must stay in lock-step with the
// cross-language contract (the single source of truth the Python runtime also
// reads). A drift here means the server and the runtime disagree about which
// task lifecycle moves are legal.
func TestStatusTransitionTableParity(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "plugins", "agentteams-teamharness", "contracts", "task-transitions.json"))
	if err != nil {
		t.Fatalf("read the transition contract: %v", err)
	}
	var contract struct {
		States      []string            `json:"states"`
		Terminal    []string            `json:"terminal"`
		Transitions map[string][]string `json:"transitions"`
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatalf("parse the transition contract: %v", err)
	}
	// The state sets must match exactly (contract states are canonical Go
	// statuses, and vice versa).
	if len(contract.States) != len(canonicalTaskStatuses) {
		t.Fatalf("contract lists %d states, Go knows %d", len(contract.States), len(canonicalTaskStatuses))
	}
	for _, s := range contract.States {
		if !canonicalTaskStatus(s) {
			t.Fatalf("contract state %q is not a canonical Go task status", s)
		}
	}
	for s := range canonicalTaskStatuses {
		if !containsString(contract.States, s) {
			t.Fatalf("Go canonical task status %q is missing from the contract states", s)
		}
	}
	// The terminal sets must match exactly.
	if len(contract.Terminal) != len(terminalTaskStatuses) {
		t.Fatalf("contract lists %d terminal states, Go has %d", len(contract.Terminal), len(terminalTaskStatuses))
	}
	for _, s := range contract.Terminal {
		if !taskStatusIsTerminal(s) {
			t.Fatalf("contract terminal state %q is not terminal in Go", s)
		}
	}
	for s := range terminalTaskStatuses {
		if !containsString(contract.Terminal, s) {
			t.Fatalf("Go terminal state %q is missing from the contract terminal list", s)
		}
	}
	// Every contract edge must be present in Go (no drift either way). For a
	// state the contract does not list as a source, Go must give it no
	// outgoing edges (it is terminal).
	for from, targets := range contract.Transitions {
		if !stringSetEqual(taskStatusTransitions[from], targets) {
			t.Fatalf("transitions from %q drifted: Go=%v contract=%v", from, taskStatusTransitions[from], targets)
		}
	}
	for from, targets := range taskStatusTransitions {
		if !containsString(contract.States, from) {
			t.Fatalf("Go transition source %q is not a contract state", from)
		}
		want, ok := contract.Transitions[from]
		if !ok {
			if len(targets) != 0 {
				t.Fatalf("Go gives %q outgoing %v but the contract lists no transitions from it", from, targets)
			}
			continue
		}
		if !stringSetEqual(targets, want) {
			t.Fatalf("transitions from %q drifted: Go=%v contract=%v", from, targets, want)
		}
	}
}

// =====================================================================
// #1467 r2 — the duplicate attention-record identity gap
// =====================================================================
//
// checkAttention indexed the proposed records by stable identity with a map
// overwrite, so a body carrying two records with the same (kind, attempt)
// identity only checked the LAST of them and silently stored the sibling:
// a worker could smuggle in a forged resolved twin of an open approval and
// the r1 gate would then see the approval as approved. The stored side had
// the same ambiguity. Duplicates are now refused on both sides, for every
// caller class (fail closed). The r2 positive controls C2 (a human closes
// in their own name -> 200) and C3 (a worker's single forged close -> 403)
// are the existing tests 8 and 1 above, which stay green.

// 13. Duplicate identity in the PROPOSED body: D1 (worker, forged first —
// the ordering the old map overwrite was fooled by), D2 (worker, open
// first), D3 (leader, a provenance-legal close as the second record).
func TestPutTaskMeta_DuplicateIdentityInBody(t *testing.T) {
	base := approvalTaskMeta()
	openRec := base["attention"].([]any)[0]
	forged := map[string]any{
		"kind":          "approval",
		"question":      "approve the submitted result?",
		"attempt":       1,
		"requestedAt":   "2026-11-01T10:00:00Z",
		"resolved":      true,
		"resolvedRole":  "human",
		"resolvedVia":   "im_reply",
		"resolvedActor": "human:luo",
		"resolvedAt":    "2026-11-01T11:00:00Z",
	}
	legalClose := map[string]any{
		"kind":          "approval",
		"question":      "approve the submitted result?",
		"attempt":       1,
		"requestedAt":   "2026-11-01T10:00:00Z",
		"resolved":      true,
		"resolvedRole":  "human",
		"resolvedVia":   "im_reply",
		"resolvedActor": "human:zhang",
		"resolvedAt":    "2026-11-01T11:00:00Z",
	}

	worker := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}
	leader := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"}

	withAttention := func(records ...any) map[string]any {
		body := map[string]any{}
		for key, value := range base {
			body[key] = value
		}
		body["attention"] = records
		return body
	}

	for _, tc := range []struct {
		name   string
		body   map[string]any
		caller *authpkg.CallerIdentity
	}{
		{"worker, forged first then open", withAttention(forged, openRec), worker},
		{"worker, open first then forged", withAttention(openRec, forged), worker},
		{"leader, open first then a legal close", withAttention(openRec, legalClose), leader},
	} {
		store := ossfake.NewMemory()
		putTask(store, taskMetaKey, base)
		h := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
		rec := putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(tc.body), tc.caller)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "duplicate identity") {
			t.Fatalf("%s -> %d %s, want 403 naming the duplicate identity", tc.name, rec.Code, rec.Body.String())
		}
	}
}

// 14. Duplicate identity in the STORED meta: the stored attention already
// carries two records with the same (kind, attempt) identity, so even a
// pure worker write-back (attention verbatim) is refused — the corrupt
// stored record must be fixed out of band, not written through.
func TestPutTaskMeta_DuplicateIdentityInStored(t *testing.T) {
	base := approvalTaskMeta()
	stored := map[string]any{}
	for key, value := range base {
		stored[key] = value
	}
	stored["attention"] = []any{
		base["attention"].([]any)[0],
		map[string]any{
			"kind":        "approval",
			"question":    "a stale twin reusing the same attempt",
			"attempt":     1,
			"requestedAt": "2026-11-01T09:55:00Z",
			"resolved":    false,
		},
	}

	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, stored)
	h := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
	worker := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}

	rec := putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(stored), worker)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "stored attention records contain an ambiguous duplicate identity") {
		t.Fatalf("stored duplicate, worker write-back -> %d %s, want 403 naming the stored duplicate identity", rec.Code, rec.Body.String())
	}
}

// 15. Positive control (r2 C1): an ordinary worker write-back keeps the
// attention records verbatim (the open approval stays open) — it lands.
func TestPutTaskMeta_WorkerWriteBackKeepsAttention(t *testing.T) {
	base := approvalTaskMeta()
	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, base)
	h := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
	worker := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}

	rec := putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(base), worker)
	if rec.Code != http.StatusOK {
		t.Fatalf("worker write-back of the identical meta -> %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// =====================================================================
// #1467 r3 — the decision_class alias (decisionClass) spelling gap
// =====================================================================
//
// checkDecisionClass protected only the canonical "decision_class"
// spelling, but the W2 acceptance guard (Python _decision_class_of,
// plugins/agentteams-teamharness/mcp/lifecycle/review.py L90-93) resolves
// the effective decision class from EITHER spelling, canonical first:
//
//     return _first_text(task.get("decision_class"), task.get("decisionClass"))
//             or DEFAULT_DECISION_CLASS
//
// so a same-team worker could rewrite just the alias (e.g. a stored
// {decisionClass: "r1"} -> "r4"), or add/remove it, and the stored bytes'
// effective approval requirement would change with the commit landing —
// and the Python guard then reads the stored bytes and sees the downgraded
// class with no approval record. The fix protects BOTH spellings as
// field-level invariants for workers (key presence and value are
// server-owned) and validates the r1-r6 vocabulary for every spelling a
// leader or human sends.
//
// Cross-language parity note: these tests align with the effective
// semantics of the Python _decision_class_of above (decision_class wins
// over the decisionClass alias; an absent pair reads as the r4 default).
// They assert that whatever bytes the Go guard lets a worker through keep
// the Python-side effective value unchanged. The Python guard is the
// reference and stays unchanged.

// 16. Worker field-level invariants over both decision-class spellings.
// Case (a) is the maintainer's probe: the stored task carries ONLY the
// alias, the worker rewrites it, and the review verdict is kept — before
// the fix the commit landed 200 and downgraded the effective class r1->r4.
func TestPutTaskMeta_WorkerDecisionClassBothSpellings(t *testing.T) {
	worker := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}

	// base with the given keys overwritten/added in the body.
	withFields := func(base, extra map[string]any) map[string]any {
		body := map[string]any{}
		for key, value := range base {
			body[key] = value
		}
		for key, value := range extra {
			body[key] = value
		}
		return body
	}

	// Alias-only stored task with a review verdict, so the write-back under
	// test is a review-passing sync and the alias rewrite is the only thing
	// the guard must catch.
	aliasOnly := taskMeta(map[string]any{
		"decisionClass": "r1",
		"review":        map[string]any{"verdict": "pass", "reviewer": "alpha-lead"},
	})
	canonicalOnly := taskMeta(map[string]any{"decision_class": "r4"})
	// Legacy dirty data: both spellings stored with differing values.
	dirtyDual := taskMeta(map[string]any{
		"decision_class": "r1",
		"decisionClass":  "r2",
	})
	bare := taskMeta(nil)

	for _, tc := range []struct {
		name string
		base map[string]any
		body map[string]any
		want int
	}{
		{
			// (a) The probe: alias-only downgrade r1 -> r4, review kept.
			"rewrite the alias r1 -> r4",
			aliasOnly,
			withFields(aliasOnly, map[string]any{"decisionClass": "r4"}),
			http.StatusForbidden,
		},
		{
			// (b) Write-back of an alias-only task, fields verbatim: legal.
			"keep the alias verbatim",
			aliasOnly,
			withFields(aliasOnly, map[string]any{"progress": "done"}),
			http.StatusOK,
		},
		{
			// (c) The pre-existing canonical rewrite (also pinned by the r1
			// tests; kept here so this group is self-contained).
			"rewrite the canonical r1 -> r4",
			taskMeta(map[string]any{"decision_class": "r1"}),
			withFields(taskMeta(map[string]any{"decision_class": "r1"}), map[string]any{"decision_class": "r4"}),
			http.StatusForbidden,
		},
		{
			// (d) Add the alias on top of the canonical, differing value.
			"add an alias with a different value",
			canonicalOnly,
			withFields(canonicalOnly, map[string]any{"decisionClass": "r1"}),
			http.StatusForbidden,
		},
		{
			// (e) Add the alias with the same value: the key itself is
			// server-owned, so a presence change is still refused.
			"add an alias with the same value",
			canonicalOnly,
			withFields(canonicalOnly, map[string]any{"decisionClass": "r4"}),
			http.StatusForbidden,
		},
		{
			// (f) Drop the only stored spelling (the alias): refused.
			"drop the stored alias",
			taskMeta(map[string]any{"decisionClass": "r4"}),
			bare,
			http.StatusForbidden,
		},
		{
			// (g) Introduce the field from nothing: refused.
			"introduce the alias from nothing",
			bare,
			taskMeta(map[string]any{"decisionClass": "r1"}),
			http.StatusForbidden,
		},
		{
			// (h) Legacy dirty dual-spelling data, kept verbatim: legal.
			"keep the dirty dual spelling verbatim",
			dirtyDual,
			withFields(dirtyDual, map[string]any{"progress": "done"}),
			http.StatusOK,
		},
		{
			// (i) Dirty dual spelling, canonical side changed: refused.
			"change the canonical of a dirty dual spelling",
			dirtyDual,
			withFields(dirtyDual, map[string]any{"decision_class": "r3"}),
			http.StatusForbidden,
		},
		{
			// (j) Dirty dual spelling, alias side changed: refused.
			"change the alias of a dirty dual spelling",
			dirtyDual,
			withFields(dirtyDual, map[string]any{"decisionClass": "r6"}),
			http.StatusForbidden,
		},
	} {
		store := ossfake.NewMemory()
		putTask(store, taskMetaKey, tc.base)
		h := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
		original, err := store.GetObject(context.Background(), taskMetaKey)
		if err != nil {
			t.Fatal(err)
		}

		rec := putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(tc.body), worker)
		if rec.Code != tc.want {
			t.Fatalf("%s -> %d %s, want %d", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
		after, err := store.GetObject(context.Background(), taskMetaKey)
		if err != nil {
			t.Fatal(err)
		}
		if tc.want == http.StatusOK {
			// The endpoint stores the committed body verbatim.
			if !bytes.Equal(after, []byte(taskMetaBody(tc.body))) {
				t.Fatalf("%s: stored meta != committed body\nbefore=%s\nstored=%s\nbody  =%s",
					tc.name, original, after, taskMetaBody(tc.body))
			}
		} else if !bytes.Equal(after, original) {
			t.Fatalf("%s: stored meta changed after the refused commit:\nbefore=%s\nafter =%s",
				tc.name, original, after)
		}
	}
}

// 17. Leader / human vocabulary over both spellings: an out-of-vocabulary
// value is refused under whichever spelling it arrives in (the message
// names the actual key), an in-vocabulary value lands. Manager/admin are
// not content-validated (mirroring the runtime) and stay out of scope.
func TestPutTaskMeta_LeaderDecisionClassAliasVocabulary(t *testing.T) {
	leader := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"}
	human := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "luo", Teams: []string{"alpha-team"}}

	for _, tc := range []struct {
		name        string
		body        map[string]any
		caller      *authpkg.CallerIdentity
		want        int
		wantMessage string
	}{
		{
			// The alias with an out-of-vocabulary value: refused, naming
			// the alias key.
			"alias out of vocabulary",
			taskMeta(map[string]any{"decisionClass": "r9"}),
			leader,
			http.StatusForbidden,
			"decisionClass must be one of r1, r2, r3, r4, r5 or r6",
		},
		{
			// The canonical spelling keeps its pre-existing vocabulary rule
			// (and its pre-existing message).
			"canonical out of vocabulary",
			taskMeta(map[string]any{"decision_class": "r9"}),
			leader,
			http.StatusForbidden,
			"decision_class must be one of r1, r2, r3, r4, r5 or r6",
		},
		{
			// The alias with a legal value: lands.
			"alias in vocabulary",
			taskMeta(map[string]any{"decisionClass": "r3"}),
			leader,
			http.StatusOK,
			"",
		},
		{
			// The same vocabulary rule applies to the human role.
			"human alias out of vocabulary",
			taskMeta(map[string]any{"decisionClass": "r0"}),
			human,
			http.StatusForbidden,
			"decisionClass must be one of r1, r2, r3, r4, r5 or r6",
		},
	} {
		store := ossfake.NewMemory()
		putTask(store, taskMetaKey, taskMeta(nil))
		h := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
		original, err := store.GetObject(context.Background(), taskMetaKey)
		if err != nil {
			t.Fatal(err)
		}

		rec := putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(tc.body), tc.caller)
		if rec.Code != tc.want {
			t.Fatalf("%s -> %d %s, want %d", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
		if tc.wantMessage != "" && !strings.Contains(rec.Body.String(), tc.wantMessage) {
			t.Fatalf("%s: body=%s, want it to contain %q", tc.name, rec.Body.String(), tc.wantMessage)
		}
		after, err := store.GetObject(context.Background(), taskMetaKey)
		if err != nil {
			t.Fatal(err)
		}
		if tc.want == http.StatusOK {
			if !bytes.Equal(after, []byte(taskMetaBody(tc.body))) {
				t.Fatalf("%s: stored meta != committed body\nbefore=%s\nstored=%s\nbody  =%s",
					tc.name, original, after, taskMetaBody(tc.body))
			}
		} else if !bytes.Equal(after, original) {
			t.Fatalf("%s: stored meta changed after the refused commit:\nbefore=%s\nafter =%s",
				tc.name, original, after)
		}
	}
}

// =====================================================================
// #1498 — create-if-absent (If-None-Match: *) on the task-meta commit
// =====================================================================

// #1498: the first creation is the conditional-commit story with
// If-None-Match: * — a concurrent create + cancellation landing between the
// caller's absence read and the write must surface as a 409 (the caller
// re-reads and converges on the now-versioned record), never as a clobber.
func TestPutTaskMetaCreateIfAbsent_SucceedsWhenAbsent(t *testing.T) {
	store := ossfake.NewMemory()
	h := newProjectTestHandler(t, store, team("alpha-team"))

	rec := putTaskMetaCreateRequest(t, h, "t1", "alpha-team", taskMetaBody(taskMeta(nil)), nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ETag string `json:"etag"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.ETag == "" {
		t.Fatalf("response=%s, want a committed etag", rec.Body.String())
	}
	committed, err := store.StatMeta(context.Background(), taskMetaKey)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ETag != committed.ETag {
		t.Fatalf("committed etag=%s, want %s (the version a later If-Match commit must match)", committed.ETag, resp.ETag)
	}
	stored, err := store.GetObject(context.Background(), taskMetaKey)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != taskMetaBody(taskMeta(nil)) {
		t.Fatalf("stored=%s, want the create body verbatim", stored)
	}
}

func TestPutTaskMetaCreateIfAbsent_ConflictsWhenPresent(t *testing.T) {
	store := ossfake.NewMemory()
	// A concurrent writer created the task (and cancelled it) before this
	// caller's create lands — exactly the #1498 interleaving.
	concurrent := taskMeta(map[string]any{"status": "cancelled", "cancel_reason": "operator stop"})
	putTask(store, taskMetaKey, concurrent)
	before, err := store.GetObject(context.Background(), taskMetaKey)
	if err != nil {
		t.Fatal(err)
	}
	h := newProjectTestHandler(t, store, team("alpha-team"))

	rec := putTaskMetaCreateRequest(t, h, "t1", "alpha-team", taskMetaBody(taskMeta(map[string]any{"progress": "mine"})), nil)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Code != "conflict" {
		t.Fatalf("409 body=%s, want code=conflict", rec.Body.String())
	}
	after, err := store.GetObject(context.Background(), taskMetaKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the concurrent record was clobbered by the create: %s", after)
	}
}

func TestPutTaskMetaCreateIfAbsent_RequiresCanonicalStatus(t *testing.T) {
	bodies := []string{
		`{"task_id": "t1"}`,                          // missing status
		`{"task_id": "t1", "Status": "in_progress"}`, // case alias
		`{"task_id": "t1", "status": "running"}`,     // noncanonical
	}
	for _, body := range bodies {
		store := ossfake.NewMemory()
		h := newProjectTestHandler(t, store, team("alpha-team"))
		req := httptest.NewRequest(http.MethodPut, "/api/v1/tasks/t1/meta?team=alpha-team", strings.NewReader(body))
		req.SetPathValue("id", "t1")
		req.Header.Set("If-None-Match", "*")
		rec := httptest.NewRecorder()
		h.PutTaskMeta(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s -> %d, want 400; body=%s", body, rec.Code, rec.Body.String())
		}
		if _, err := store.StatMeta(context.Background(), taskMetaKey); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("body %s must not write: %v", body, err)
		}
	}
}

func TestPutTaskMetaCreateIfAbsent_RejectsBothHeaders(t *testing.T) {
	store := ossfake.NewMemory()
	h := newProjectTestHandler(t, store, team("alpha-team"))
	req := httptest.NewRequest(http.MethodPut, "/api/v1/tasks/t1/meta?team=alpha-team", strings.NewReader(taskMetaBody(taskMeta(nil))))
	req.SetPathValue("id", "t1")
	req.Header.Set("If-Match", "etag-1")
	req.Header.Set("If-None-Match", "*")
	rec := httptest.NewRecorder()
	h.PutTaskMeta(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (exactly one conditional header); body=%s", rec.Code, rec.Body.String())
	}
	if _, err := store.StatMeta(context.Background(), taskMetaKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected request must not write: %v", err)
	}
}

func TestPutTaskMetaCreateIfAbsent_RejectsNonStarETag(t *testing.T) {
	store := ossfake.NewMemory()
	h := newProjectTestHandler(t, store, team("alpha-team"))
	req := httptest.NewRequest(http.MethodPut, "/api/v1/tasks/t1/meta?team=alpha-team", strings.NewReader(taskMetaBody(taskMeta(nil))))
	req.SetPathValue("id", "t1")
	req.Header.Set("If-None-Match", "etag-1")
	rec := httptest.NewRecorder()
	h.PutTaskMeta(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (literal * only); body=%s", rec.Code, rec.Body.String())
	}
	if _, err := store.StatMeta(context.Background(), taskMetaKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected request must not write: %v", err)
	}
}

func TestPutTaskMetaCreateIfAbsent_UnknownTeamIs404(t *testing.T) {
	store := ossfake.NewMemory()
	h := newProjectTestHandler(t, store, team("alpha-team"))

	rec := putTaskMetaCreateRequest(t, h, "t1", "ghost-team", taskMetaBody(taskMeta(nil)), nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (no such team prefix); body=%s", rec.Code, rec.Body.String())
	}
}

func TestPutTaskMetaCreateIfAbsent_WorkerTeamScope(t *testing.T) {
	alpha := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}
	beta := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "beta-dev", Team: "beta-team"}

	store := ossfake.NewMemory()
	h := newProjectTestHandler(t, store, team("alpha-team"))
	rec := putTaskMetaCreateRequest(t, h, "t1", "alpha-team", taskMetaBody(taskMeta(nil)), alpha)
	if rec.Code != http.StatusOK {
		t.Fatalf("own-team create -> %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := store.StatMeta(context.Background(), taskMetaKey); err != nil {
		t.Fatalf("own-team create did not store under the team prefix: %v", err)
	}

	store = ossfake.NewMemory()
	h = newProjectTestHandler(t, store, team("alpha-team"))
	rec = putTaskMetaCreateRequest(t, h, "t1", "alpha-team", taskMetaBody(taskMeta(nil)), beta)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-team create -> %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := store.StatMeta(context.Background(), taskMetaKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cross-team create must not write: %v", err)
	}

	// Without the ?team= qualifier the create targets the global scope,
	// which is not the worker's own team — refused, same rule as the update
	// path's worker boundary.
	store = ossfake.NewMemory()
	h = newProjectTestHandler(t, store, team("alpha-team"))
	rec = putTaskMetaCreateRequest(t, h, "t1", "", taskMetaBody(taskMeta(nil)), alpha)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unqualified create -> %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func containsString(set []string, s string) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
}

func stringSetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	members := make(map[string]struct{}, len(a))
	for _, v := range a {
		members[v] = struct{}{}
	}
	for _, v := range b {
		if _, ok := members[v]; !ok {
			return false
		}
	}
	return true
}
