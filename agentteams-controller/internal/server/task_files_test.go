package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func TestTaskFilesResolveDeclaredOutputWithoutCallerPaths(t *testing.T) {
	store := ossfake.NewMemory()
	meta := `{"task_id":"t1","status":"submitted","deliverables":["shared/tasks/t1/workspace/report.md"]}`
	for key, data := range map[string]string{taskMetaKey: meta, "teams/alpha-team/shared/tasks/t1/workspace/report.md": "verified report\n"} {
		if err := store.PutObject(t.Context(), key, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	h := newProjectTestHandler(t, store, team("alpha-team"))
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "dev", Team: "alpha-team"}
	request := func(query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1/files?team=alpha-team"+query, nil)
		req.SetPathValue("id", "t1")
		rec := httptest.NewRecorder()
		h.GetTaskFiles(rec, withCaller(req, caller))
		return rec
	}
	listed := request("")
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), "shared/tasks") {
		t.Fatalf("list: %d %s", listed.Code, listed.Body.String())
	}
	var listing struct {
		Files []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"files"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &listing); err != nil || len(listing.Files) != 1 || listing.Files[0].Name != "report.md" || listing.Files[0].Version == "" {
		t.Fatalf("listing=%s err=%v", listed.Body.String(), err)
	}
	got := request("&name=report.md")
	if got.Code != http.StatusOK || got.Body.String() != "verified report\n" || got.Header().Get("X-Content-SHA256") == "" {
		t.Fatalf("get: %d %s", got.Code, got.Body.String())
	}
	preview := request("&name=report.md&view=text&limit=4")
	var text struct {
		Text       string `json:"text"`
		Truncated  bool   `json:"truncated"`
		NextOffset int64  `json:"nextOffset"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &text); err != nil || text.Text != "veri" || !text.Truncated || text.NextOffset != 4 {
		t.Fatalf("preview: %d %s err=%v", preview.Code, preview.Body.String(), err)
	}
	if err := store.PutObject(t.Context(), "teams/alpha-team/shared/tasks/t1/workspace/report.md", []byte("new version")); err != nil {
		t.Fatal(err)
	}
	stale := request("&name=report.md&version=" + listing.Files[0].Version)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale download: %d %s", stale.Code, stale.Body.String())
	}
	for _, query := range []string{"&name=meta.json", "&name=../meta.json", "&name=shared/tasks/other/report.md"} {
		if rec := request(query); rec.Code != http.StatusNotFound {
			t.Fatalf("undeclared file: %d %s", rec.Code, rec.Body.String())
		}
	}
}

func TestTaskFilesRejectForeignArtifactEvenWhenDeclared(t *testing.T) {
	store := ossfake.NewMemory()
	if err := store.PutObject(t.Context(), taskMetaKey, []byte(`{"task_id":"t1","status":"submitted","deliverables":["shared/tasks/other/workspace/private.md"]}`)); err != nil {
		t.Fatal(err)
	}
	h := newProjectTestHandler(t, store, team("alpha-team"))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1/files?name=private.md", nil)
	req.SetPathValue("id", "t1")
	rec := httptest.NewRecorder()
	h.GetTaskFiles(rec, withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin}))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("must reject invalid stored reference: %d %s", rec.Code, rec.Body.String())
	}
}

func TestTaskFileTextReadsKeepUnicodeAndVersionAcrossChunks(t *testing.T) {
	store := ossfake.NewMemory()
	if err := store.PutObject(t.Context(), taskMetaKey, []byte(`{"task_id":"t1","status":"submitted","deliverables":["shared/tasks/t1/workspace/report.md"]}`)); err != nil {
		t.Fatal(err)
	}
	key := "teams/alpha-team/shared/tasks/t1/workspace/report.md"
	if err := store.PutObject(t.Context(), key, []byte("中文文本")); err != nil {
		t.Fatal(err)
	}
	h := newProjectTestHandler(t, store, team("alpha-team"))
	read := func(query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1/files?name=report.md&view=text"+query, nil)
		req.SetPathValue("id", "t1")
		rec := httptest.NewRecorder()
		h.GetTaskFiles(rec, withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin}))
		return rec
	}
	first := read("&limit=4")
	var chunk struct {
		Text    string `json:"text"`
		Version string `json:"version"`
		Next    int64  `json:"nextOffset"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &chunk); err != nil || chunk.Text != "中" || chunk.Next != 3 {
		t.Fatalf("first=%s err=%v", first.Body.String(), err)
	}
	if got := read("&offset=3"); got.Code != http.StatusBadRequest {
		t.Fatalf("unversioned continuation: %d", got.Code)
	}
	if got := read("&offset=3&version=" + chunk.Version); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "文文本") {
		t.Fatalf("continuation: %d %s", got.Code, got.Body.String())
	}
	if err := store.PutObject(t.Context(), key, nil); err != nil {
		t.Fatal(err)
	}
	empty := read("")
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"text":""`) {
		t.Fatalf("empty text: %d %s", empty.Code, empty.Body.String())
	}
}
