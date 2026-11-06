package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
)

func TestHumanFiles_RestrictedDirectoryEntries(t *testing.T) {
	h, store := newWorkspaceAccessRig(t, v1beta1.HumanSpec{
		DisplayName: "Alice", AccessibleTeams: []string{"team-a"},
		WorkspaceAccess: &v1beta1.HumanWorkspaceAccess{Restrictions: []v1beta1.HumanWorkspaceAccessRule{
			wsRule("team-a", "none", "files/payroll.csv", "files/secret/**", "files/mixed/hidden.txt"),
		}},
	}, wsTeam("team-a", "alice"))
	for _, path := range []string{"payroll.csv", "secret/data.txt", "mixed/hidden.txt", "mixed/public.txt", "public.txt", "memory/private.txt", "digest/private.txt", ".control/quota.json"} {
		wsSeed(t, store, path, "confidential")
	}
	for _, tc := range []struct {
		name, path string
		caller     *authpkg.CallerIdentity
		want       []string
	}{
		{"leader-root", "", wsLeader("team-a", "leader"), []string{"mixed", "public.txt"}},
		{"worker-root", "", wsWorker("team-a", "worker"), []string{"mixed", "public.txt"}},
		{"leader-subdirectory", "mixed/", wsLeader("team-a", "leader"), []string{"public.txt"}},
		{"owner", "", hfSelf(), []string{".control", "digest", "memory", "mixed", "payroll.csv", "public.txt", "secret"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := wsDo(t, h, wsOp{"list", http.MethodGet, tc.path, tc.caller, "", http.StatusOK})
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var result humanFileListResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Entries) != len(tc.want) {
				t.Fatalf("entries=%+v want names=%v", result.Entries, tc.want)
			}
			for i, entry := range result.Entries {
				if entry.Name != tc.want[i] {
					t.Fatalf("entries=%+v want names=%v", result.Entries, tc.want)
				}
			}
		})
	}
}

func TestHumanFiles_RestrictedTrashEntries(t *testing.T) {
	h, store := newWorkspaceAccessRig(t, v1beta1.HumanSpec{
		DisplayName: "Alice", AccessibleTeams: []string{"team-a"},
		WorkspaceAccess: &v1beta1.HumanWorkspaceAccess{Restrictions: []v1beta1.HumanWorkspaceAccessRule{
			wsRule("team-a", "none", "files/payroll.csv"),
		}},
	}, wsTeam("team-a", "alice"))
	for _, path := range []string{"payroll.csv", "public.txt"} {
		wsSeed(t, store, path, "content")
		wsTrashID(t, wsDo(t, h, wsOp{"delete", http.MethodDelete, path, hfSelf(), "", http.StatusOK}))
	}
	// A corrupt/missing metadata object must not disclose even a trash ID
	// to a caller whose access depends on the original path.
	if err := store.PutObject(context.Background(), humanTrashPrefix("alice")+"20261106T000000Z-deadbeef/content", []byte("orphan")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		caller *authpkg.CallerIdentity
		want   int
	}{
		{"leader", wsLeader("team-a", "leader"), 1},
		{"worker", wsWorker("team-a", "worker"), 1},
		{"owner", hfSelf(), 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.listTrash(rec, hfTrashReq(t, http.MethodGet, "alice", "", tc.caller, ""))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var result struct {
				Entries []trashEntry `json:"entries"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Entries) != tc.want {
				t.Fatalf("entries=%+v want count=%d", result.Entries, tc.want)
			}
			if tc.want == 1 && result.Entries[0].Path != "files/public.txt" {
				t.Fatalf("restricted trash entry exposed: %+v", result.Entries)
			}
		})
	}
}
