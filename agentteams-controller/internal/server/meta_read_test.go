package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

// GET /projects/{id}/meta and /tasks/{id}/meta are the read half of the
// conditional commit pair: they must return the stored bytes verbatim and
// the ETag that Put*Meta accepts as If-Match.

func TestGetProjectMeta_ReturnsStoredBytesAndETag(t *testing.T) {
	store := ossfake.NewMemory()
	key := metaCommitKey("alpha-team", "pm1")
	original := []byte(`{"project_id":"pm1","title":"PM1","status":"active","plan_type":"dag","team_id":"alpha-team"}`)
	if err := store.PutObject(context.Background(), key, original); err != nil {
		t.Fatal(err)
	}
	h := newProjectTestHandler(t, store, team("alpha-team"))
	read, err := store.StatMeta(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/pm1/meta", nil)
	req.SetPathValue("id", "pm1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectMeta(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("ETag"); got != read.ETag {
		t.Fatalf("ETag=%q, want %q", got, read.ETag)
	}
	if rec.Body.String() != string(original) {
		t.Fatalf("body not verbatim: %q", rec.Body.String())
	}
}

func TestGetProjectMeta_MissingIs404(t *testing.T) {
	store := ossfake.NewMemory()
	h := newProjectTestHandler(t, store, team("alpha-team"))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/nope/meta", nil)
	req.SetPathValue("id", "nope")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectMeta(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetTaskMeta_ReturnsStoredBytesAndETag(t *testing.T) {
	store := ossfake.NewMemory()
	key := "teams/alpha-team/shared/tasks/t1/meta.json"
	original := []byte(`{"task_id":"t1","status":"running","title":"T1"}`)
	if err := store.PutObject(context.Background(), key, original); err != nil {
		t.Fatal(err)
	}
	h := newProjectTestHandler(t, store, team("alpha-team"))
	read, err := store.StatMeta(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1/meta", nil)
	req.SetPathValue("id", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskMeta(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("ETag"); got != read.ETag {
		t.Fatalf("ETag=%q, want %q", got, read.ETag)
	}
	if rec.Body.String() != string(original) {
		t.Fatalf("body not verbatim: %q", rec.Body.String())
	}
}

func TestGetTaskMeta_MissingIs404(t *testing.T) {
	store := ossfake.NewMemory()
	h := newProjectTestHandler(t, store, team("alpha-team"))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/nope/meta", nil)
	req.SetPathValue("id", "nope")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskMeta(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
