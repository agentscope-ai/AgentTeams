package server

import (
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHumanChangesRouteKeepsExistingFileReadable(t *testing.T) {
	h, store, k8s := newHumanFilesRig(t)
	if rec := hfPutFile(t, h, "alice", "changes", hfSelf(), "existing file bytes"); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	srv := NewHTTPServer(":0", ServerDeps{Client: k8s, Namespace: "default", OSS: store, AuthMw: authpkg.NewMiddleware(nil, nil, nil, nil, "")})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/humans/alice/files/changes", nil)
	req = withCaller(req, hfSelf())
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "existing file bytes" {
		t.Fatalf("existing file GET: %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/humans/alice/files/changes?since=v1:", nil)
	req = withCaller(req, hfSelf())
	rec = httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") == "text/plain" {
		t.Fatalf("delta GET: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHumanChangesIncludesPresignedCommit(t *testing.T) {
	h, store, _ := newHumanFilesRig(t)
	pr := hfPresign(t, h, "alice", "uploaded.txt", "presign=1")
	if err := store.PutObject(t.Context(), hfStagingKey(t, &pr), []byte("upload bytes")); err != nil {
		t.Fatal(err)
	}
	rec, delta := hfGetChanges(t, h, "alice", "since=v1:", hfSelf())
	if rec.Code != http.StatusOK || len(delta.Items) != 0 {
		t.Fatalf("staging must not be visible: %d %+v", rec.Code, delta)
	}
	commit := hfPostFile(t, h, "alice", "uploaded.txt/commit", hfSelf(), "")
	if commit.Code != http.StatusOK {
		t.Fatalf("commit: %d %s", commit.Code, commit.Body.String())
	}
	rec, delta = hfGetChanges(t, h, "alice", "since=v1:", hfSelf())
	if rec.Code != http.StatusOK || len(delta.Items) != 1 || delta.Items[0].Path != "files/uploaded.txt" || delta.Items[0].Size != 12 || delta.Items[0].ETag == "" {
		t.Fatalf("committed upload missing from delta: %d %+v", rec.Code, delta)
	}
	retry := hfPostFile(t, h, "alice", "uploaded.txt/commit", hfSelf(), "")
	if retry.Code != http.StatusOK {
		t.Fatalf("retry: %d %s", retry.Code, retry.Body.String())
	}
	rec, delta = hfGetChanges(t, h, "alice", "since="+delta.Cursor, hfSelf())
	if rec.Code != http.StatusOK || len(delta.Items) != 0 {
		t.Fatalf("retry must not create another mutation: %d %+v", rec.Code, delta)
	}
}
