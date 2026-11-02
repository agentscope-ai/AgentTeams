package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// #1471: the task meta conditional commit is the task-level counterpart of
// the project meta commit (#1402). A worker write-back mirrors the WHOLE task
// record, so the Controller's cancellation (status=cancelled plus its fields
// and the resolved continuation) must be server-owned, and the commit must be
// bound to the version the caller read.

const taskMetaKey = "teams/alpha-team/shared/tasks/t1/meta.json"

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
}
