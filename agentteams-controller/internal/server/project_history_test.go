package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// lsLikeOSS mimics `mc ls <prefix>` for BOTH files and directories: returns
// direct child names ("1723.json", "p1/") rather than full keys. The
// mcLikeOSS helper used by the project-list tests only returns directories,
// which is not enough for listing a history/ directory of snapshot files.
type lsLikeOSS struct {
	*ossfake.Memory
}

func (m *lsLikeOSS) ListObjects(_ context.Context, prefix string) ([]string, error) {
	keys, err := m.Memory.ListObjects(context.Background(), prefix)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, k := range keys {
		rest := strings.TrimPrefix(k, prefix)
		parts := strings.SplitN(rest, "/", 2)
		if parts[0] == "" {
			continue
		}
		child := parts[0]
		if len(parts) == 2 && parts[1] != "" {
			child += "/"
		}
		if !seen[child] {
			seen[child] = true
			out = append(out, child)
		}
	}
	sort.Strings(out)
	return out, nil
}

// newHistoryTestHandler builds a ProjectHandler whose OSS lists both files
// and directories (needed by the history endpoints).
func newHistoryTestHandler(t *testing.T, store *ossfake.Memory, teams ...*v1beta1.Team) *ProjectHandler {
	t.Helper()
	objs := make([]runtime.Object, 0, len(teams))
	for _, tm := range teams {
		objs = append(objs, tm)
	}
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	var o oss.StorageClient = &lsLikeOSS{Memory: store}
	return NewProjectHandler(k8s, "default", o)
}

// putHistorySnapshot writes a snapshot file into a project's history dir.
func putHistorySnapshot(store *ossfake.Memory, key string, ts string, content string) {
	_ = store.PutObject(context.Background(), key+"history/"+ts+".json", []byte(content))
}

func TestGetProjectHistory_ListsSnapshotsNewestFirst(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	// Two snapshots; "newest" has the larger unixNano timestamp.
	putHistorySnapshot(store, "teams/alpha-team/shared/projects/p1/", "1723785123456789010", `{"status":"planning"}`)
	putHistorySnapshot(store, "teams/alpha-team/shared/projects/p1/", "1723785123456789020", `{"status":"active"}`)
	h := newHistoryTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/history", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectHistory(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp projectHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Snapshots) != 2 {
		t.Fatalf("snapshots=%d, want 2", len(resp.Snapshots))
	}
	if resp.Snapshots[0].Timestamp != "1723785123456789020" || resp.Snapshots[1].Timestamp != "1723785123456789010" {
		t.Fatalf("order wrong: %+v", resp.Snapshots)
	}
}

func TestGetProjectHistory_EmptyHistoryReturnsEmptyArray(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p2/meta.json", map[string]any{
		"project_id": "p2", "title": "Standalone", "status": "active", "plan_type": "loop",
	})
	h := newHistoryTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p2/history", nil)
	req.SetPathValue("id", "p2")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectHistory(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// snapshots must serialize as [] — not null.
	if !json.Valid(rec.Body.Bytes()) || !strings.Contains(rec.Body.String(), `"snapshots":[]`) {
		t.Fatalf("want empty snapshots array, got %s", rec.Body.String())
	}
}

func TestGetProjectHistorySnapshot_ReturnsVerbatim(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "status": "active", "team_id": "alpha-team",
	})
	raw := `{"project_id":"p1","status":"planning","updated_by":"luo","pause_reason":"waiting for review"}`
	putHistorySnapshot(store, "teams/alpha-team/shared/projects/p1/", "1723785123456789010", raw)
	h := newHistoryTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/history/1723785123456789010", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("timestamp", "1723785123456789010")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectHistorySnapshot(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != raw {
		t.Fatalf("snapshot not verbatim:\n got %s\nwant %s", rec.Body.String(), raw)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type=%q, want application/json", ct)
	}
}

func TestGetProjectHistorySnapshot_InvalidTimestamp(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{"project_id": "p1"})
	h := newHistoryTestHandler(t, store)

	for _, ts := range []string{"../etc", "abc", "123456789012345678", "12345678901234567890"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/history/"+ts, nil)
		req.SetPathValue("id", "p1")
		req.SetPathValue("timestamp", ts)
		req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
		rec := httptest.NewRecorder()
		h.GetProjectHistorySnapshot(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("timestamp=%q status=%d, want 400", ts, rec.Code)
		}
	}
}

func TestGetProjectHistorySnapshot_NotFound(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{"project_id": "p1"})
	h := newHistoryTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/history/1723785123456789010", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("timestamp", "1723785123456789010")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectHistorySnapshot(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for absent snapshot", rec.Code)
	}
}

func TestGetProjectHistory_TeamLeaderCrossTeamDenied(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/beta-team/shared/projects/p2/meta.json", map[string]any{
		"project_id": "p2", "status": "active", "team_id": "beta-team",
	})
	putHistorySnapshot(store, "teams/beta-team/shared/projects/p2/", "1723785123456789010", `{}`)
	h := newHistoryTestHandler(t, store, team("beta-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p2/history", nil)
	req.SetPathValue("id", "p2")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"})
	rec := httptest.NewRecorder()
	h.GetProjectHistory(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for cross-team access", rec.Code)
	}
}
