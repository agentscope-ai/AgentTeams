package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
)

func TestHumanFileChangesFiltersRestrictedDescendants(t *testing.T) {
	spec := v1beta1.HumanSpec{
		AccessibleTeams: []string{"team-a"},
		WorkspaceAccess: &v1beta1.HumanWorkspaceAccess{
			Restrictions: []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", "none", "files/private/**")},
		},
	}
	h, _ := newWorkspaceAccessRig(t, spec, wsTeam("team-a", "alice"))
	for _, path := range []string{"private/a.md", "public/a.md", "private/b.md"} {
		if rec := hfPutFile(t, h, "alice", path, hfSelf(), "data"); rec.Code != http.StatusOK {
			t.Fatalf("seed %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	actor := wsWorker("team-a", "worker")
	get := httptest.NewRecorder()
	h.getFile(get, hfReq(t, http.MethodGet, "alice", "private/a.md", actor, nil))
	if get.Code != http.StatusNotFound {
		t.Fatalf("restricted file GET: %d %s", get.Code, get.Body.String())
	}
	rec, page := hfGetChanges(t, h, "alice", "since=v1:&limit=1", actor)
	if rec.Code != http.StatusOK || len(page.Items) != 1 || page.Items[0].Path != "files/public/a.md" || len(page.Deleted) != 0 {
		t.Fatalf("visible page: %d %s", rec.Code, rec.Body.String())
	}
	rec, hidden := hfGetChanges(t, h, "alice", "since="+page.Cursor+"&limit=1", actor)
	if rec.Code != http.StatusOK || len(hidden.Items) != 0 || len(hidden.Deleted) != 0 || hidden.Cursor == page.Cursor {
		t.Fatalf("hidden-only page must advance without disclosure: %d %s", rec.Code, rec.Body.String())
	}
	del := httptest.NewRecorder()
	h.deleteFile(del, hfReq(t, http.MethodDelete, "alice", "private/a.md", hfSelf(), nil))
	if del.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", del.Code, del.Body.String())
	}
	rec, deleted := hfGetChanges(t, h, "alice", "since="+hidden.Cursor, actor)
	if rec.Code != http.StatusOK || len(deleted.Items) != 0 || len(deleted.Deleted) != 0 || deleted.Cursor == hidden.Cursor {
		t.Fatalf("hidden deletion must advance without disclosure: %d %s", rec.Code, rec.Body.String())
	}
	rec, owner := hfGetChanges(t, h, "alice", "since="+hidden.Cursor, hfSelf())
	if rec.Code != http.StatusOK || len(owner.Deleted) != 1 || owner.Deleted[0] != "files/private/a.md" {
		t.Fatalf("owner must retain deletion visibility: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHumanFileChangesPurgeDoesNotDeleteLiveFiles(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		name := "absent_live_file"
		if recreate {
			name = "recreated_live_file"
		}
		t.Run(name, func(t *testing.T) {
			h, store, _ := newHumanFilesRig(t)
			if rec := hfPutFile(t, h, "alice", "a.md", hfSelf(), "old"); rec.Code != http.StatusOK {
				t.Fatal(rec.Body.String())
			}
			del := httptest.NewRecorder()
			h.deleteFile(del, hfReq(t, http.MethodDelete, "alice", "a.md", hfSelf(), nil))
			if del.Code != http.StatusOK {
				t.Fatal(del.Body.String())
			}
			var body struct {
				TrashID string `json:"trashId"`
			}
			if err := json.Unmarshal(del.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if recreate {
				if rec := hfPutFile(t, h, "alice", "a.md", hfSelf(), "recreated"); rec.Code != http.StatusOK {
					t.Fatal(rec.Body.String())
				}
			}
			rec, before := hfGetChanges(t, h, "alice", "since=v1:", hfSelf())
			if rec.Code != http.StatusOK {
				t.Fatal(rec.Body.String())
			}
			purge := httptest.NewRecorder()
			h.purgeTrash(purge, hfTrashReq(t, http.MethodDelete, "alice", body.TrashID, hfAdmin(), ""))
			if purge.Code != http.StatusOK {
				t.Fatal(purge.Body.String())
			}
			rec, after := hfGetChanges(t, h, "alice", "since="+before.Cursor, hfSelf())
			if rec.Code != http.StatusOK || len(after.Items) != 0 || len(after.Deleted) != 0 || after.Cursor == before.Cursor {
				t.Errorf("purge must only advance the cursor: %d %s", rec.Code, rec.Body.String())
			}
			if recreate {
				data, err := store.GetObject(t.Context(), humanFilesPrefix("alice")+"a.md")
				if err != nil || string(data) != "recreated" {
					t.Fatalf("recreated file changed: %q %v", data, err)
				}
			}
		})
	}
}
