package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestTaskRoomArchivePermissions(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1beta1.AddToScheme(scheme)
	team := &v1beta1.Team{ObjectMeta: metav1.ObjectMeta{Name: "team-cr", Namespace: "default"}, Spec: v1beta1.TeamSpec{TeamName: "storage-team", Admin: &v1beta1.TeamAdminSpec{Name: "alice"}, WorkerMembers: []v1beta1.TeamWorkerRef{{Name: "leader", Role: "team_leader"}}}}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team).Build()
	cases := []struct {
		name, role, user string
		teams            []string
		archive, remove  bool
	}{
		{"L1", authpkg.RoleAdmin, "root", nil, true, true},
		{"Manager", authpkg.RoleManager, "manager", nil, true, true},
		{"Team admin", authpkg.RoleHuman, "alice", []string{"team-cr"}, true, true},
		{"Leader", authpkg.RoleTeamLeader, "leader", []string{"team-cr"}, true, false},
		{"member", authpkg.RoleHuman, "bob", []string{"team-cr"}, false, false},
		{"other Team admin", authpkg.RoleHuman, "alice", []string{"other-team"}, false, false},
		{"other Leader", authpkg.RoleTeamLeader, "other-leader", []string{"team-cr"}, false, false},
		{"Worker", authpkg.RoleWorker, "worker", []string{"team-cr"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer service-admin" {
					t.Errorf("wrong upstream identity")
				}
				if r.Method == http.MethodPost {
					writes++
					w.Write([]byte(`{}`))
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"is_task_room": true, "archived": true, "room_meta": map[string]any{"roomKind": "task_room", "teamName": "storage-team", "teamAdmin": map[string]any{"userId": "@forged:test"}}})
			}))
			defer upstream.Close()
			h := NewTaskRoomArchiveHandler(ServerDeps{Client: k8s, Namespace: "default", MatrixConfig: matrix.Config{ServerURL: upstream.URL}})
			h.adminToken = func(context.Context) (string, error) { return "service-admin", nil }
			caller := &authpkg.CallerIdentity{Role: tc.role, Username: tc.user, WorkerName: tc.user, Teams: tc.teams, HumanPermissionLevel: 2}
			for _, action := range []string{"archive", "unarchive", "delete"} {
				req := httptest.NewRequest(http.MethodPost, "/api/v1/task-rooms/!room:test/"+action, nil)
				req.SetPathValue("roomId", "!room:test")
				req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), caller))
				rec := httptest.NewRecorder()
				h.Mutate(action)(rec, req)
				allowed := tc.archive
				if action == "delete" {
					allowed = tc.remove
				}
				want := http.StatusForbidden
				if allowed {
					want = http.StatusOK
				}
				if rec.Code != want {
					t.Fatalf("%s: got %d, want %d: %s", action, rec.Code, want, rec.Body.String())
				}
			}
			expected := 0
			if tc.archive {
				expected += 2
			}
			if tc.remove {
				expected++
			}
			if writes != expected {
				t.Fatalf("writes=%d want=%d", writes, expected)
			}
		})
	}
}

func TestTaskRoomArchiveEligibility(t *testing.T) {
	for _, kind := range []string{"dm", "group_room", "team_room", "task_room"} {
		t.Run(kind, func(t *testing.T) {
			writes := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					writes++
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"is_task_room": kind == "task_room", "archived": false, "room_meta": map[string]any{"roomKind": kind}})
			}))
			defer upstream.Close()
			h := NewTaskRoomArchiveHandler(ServerDeps{MatrixConfig: matrix.Config{ServerURL: upstream.URL}})
			h.adminToken = func(context.Context) (string, error) { return "admin", nil }
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.SetPathValue("roomId", "!room:test")
			req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), &authpkg.CallerIdentity{Role: authpkg.RoleAdmin}))
			rec := httptest.NewRecorder()
			h.Mutate("delete")(rec, req)
			if rec.Code != 403 || writes != 0 {
				t.Fatalf("kind=%s code=%d writes=%d", kind, rec.Code, writes)
			}
		})
	}
}

func TestTaskRoomArchiveListUsesOwnIdentityAndTeamScope(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1beta1.AddToScheme(scheme)
	team := &v1beta1.Team{ObjectMeta: metav1.ObjectMeta{Name: "team-cr", Namespace: "default"}, Spec: v1beta1.TeamSpec{TeamName: "storage-team", Admin: &v1beta1.TeamAdminSpec{Name: "alice"}}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer own-matrix-token" {
			t.Errorf("list used a different account identity")
		}
		q := r.URL.Query()
		if q.Get("team_admin") != "@alice:test" || q.Get("human_member") != "@bob:test" || q.Get("role") != "" {
			t.Errorf("bad filters: %s", r.URL.RawQuery)
		}
		json.NewEncoder(w).Encode(map[string]any{"rooms": []map[string]any{
			{"room_id": "!own:test", "room_meta": map[string]any{"roomKind": "task_room", "teamName": "storage-team"}, "can_archive": true, "can_delete": true},
			{"room_id": "!other:test", "room_meta": map[string]any{"roomKind": "task_room", "teamName": "other-team"}},
			{"room_id": "!group:test", "room_meta": map[string]any{"roomKind": "group_room", "teamName": "storage-team"}},
		}})
	}))
	defer upstream.Close()
	h := NewTaskRoomArchiveHandler(ServerDeps{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(team).Build(), Namespace: "default", MatrixConfig: matrix.Config{ServerURL: upstream.URL}})
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", MatrixUserID: "@bob:test", HumanPermissionLevel: 2, Teams: []string{"team-cr"}}
	req := httptest.NewRequest(http.MethodGet, "/?team_admin=%40alice%3Atest&human_member=%40bob%3Atest&role=admin", nil)
	req.Header.Set("Authorization", "bearer own-matrix-token")
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), caller))
	rec := httptest.NewRecorder()
	h.List(rec, req)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var response struct {
		Rooms []map[string]any `json:"rooms"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &response)
	if len(response.Rooms) != 1 || response.Rooms[0]["room_id"] != "!own:test" || response.Rooms[0]["can_archive"] != false || response.Rooms[0]["can_delete"] != false {
		t.Fatalf("unexpected visibility/capabilities: %s", rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Error("account-scoped result must not be shared-cached")
	}
}

func TestTaskRoomArchiveRejectsAnonymousBeforeAccessingMatrix(t *testing.T) {
	h := NewTaskRoomArchiveHandler(ServerDeps{})
	h.adminToken = func(context.Context) (string, error) {
		t.Fatal("anonymous caller accessed admin credential")
		return "", nil
	}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.SetPathValue("roomId", "!task:test")
	for _, handler := range []http.HandlerFunc{h.List, h.Status, h.Mutate("archive"), h.Mutate("delete")} {
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != 401 {
			t.Fatalf("got %d want 401", rec.Code)
		}
	}
}

func TestTaskRoomArchiveLeaderUsesCanonicalCRIdentity(t *testing.T) {
	team := v1beta1.Team{ObjectMeta: metav1.ObjectMeta{Name: "team-cr"}, Spec: v1beta1.TeamSpec{TeamName: "storage-team", WorkerMembers: []v1beta1.TeamWorkerRef{{Name: "leader-cr", Role: "team_leader"}}}}
	meta := map[string]any{"roomKind": "task_room", "teamName": "storage-team"}
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "leader-cr", WorkerName: "leader-runtime", Team: team.Name}
	read, archive, remove := taskRoomRights(caller, meta, []v1beta1.Team{team})
	if !read || !archive || remove {
		t.Fatalf("Leader runtime alias lost its CR permissions: read=%v archive=%v delete=%v", read, archive, remove)
	}
	caller.Username, caller.WorkerName = "other-cr", "leader-cr"
	read, archive, remove = taskRoomRights(caller, meta, []v1beta1.Team{team})
	if read || archive || remove {
		t.Fatal("runtime name must not impersonate the owning Leader CR")
	}
}

func TestTaskRoomArchiveMatchesExistingTeamNameAliases(t *testing.T) {
	team := v1beta1.Team{ObjectMeta: metav1.ObjectMeta{Name: "team-cr"}, Spec: v1beta1.TeamSpec{TeamName: "storage-team", Admin: &v1beta1.TeamAdminSpec{Name: "alice"}, WorkerMembers: []v1beta1.TeamWorkerRef{{Name: "leader-cr", Role: "team_leader"}}}}
	for _, roomTeam := range []string{team.Name, team.Spec.TeamName} {
		for _, scope := range []string{team.Name, team.Spec.TeamName} {
			for _, role := range []string{authpkg.RoleHuman, authpkg.RoleTeamLeader} {
				caller := &authpkg.CallerIdentity{Role: role, Username: "alice", HumanPermissionLevel: 2, Teams: []string{scope}}
				if role == authpkg.RoleTeamLeader {
					caller.Username, caller.WorkerName = "leader-cr", "leader-runtime"
					caller.Team, caller.Teams = scope, nil
				}
				read, archive, remove := taskRoomRights(caller, map[string]any{"roomKind": "task_room", "teamName": roomTeam}, []v1beta1.Team{team})
				if !read || !archive || remove != (role == authpkg.RoleHuman) {
					t.Fatalf("role=%s roomTeam=%s scope=%s: read=%v archive=%v delete=%v", role, roomTeam, scope, read, archive, remove)
				}
			}
		}
	}
}
