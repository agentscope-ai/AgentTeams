package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	audit "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// --- Test rig and helpers (#1414 P2 workspace access) ----------------------

// newWorkspaceAccessRig builds a human-files handler for human "alice" with
// the given spec and Team CRs, backed by an in-memory store and a live
// audit client.
func newWorkspaceAccessRig(t *testing.T, spec v1beta1.HumanSpec, teams ...*v1beta1.Team) (*humanFilesHandler, *ossfake.Memory) {
	t.Helper()
	scheme := newServerTestScheme(t)
	alice := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Spec:       spec,
	}
	objects := []client.Object{alice}
	for _, tm := range teams {
		objects = append(objects, tm)
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	store := ossfake.NewMemory()
	return NewHumanFilesHandler(k8s, "default", store, audit.NewClient(store)), store
}

// wsTeam builds a Team CR that lists the given humans as humanMembers.
func wsTeam(name string, humanMembers ...string) *v1beta1.Team {
	spec := v1beta1.TeamSpec{}
	for _, m := range humanMembers {
		spec.HumanMembers = append(spec.HumanMembers, v1beta1.TeamMemberSpec{Name: m})
	}
	return &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spec,
	}
}

func wsLeader(team, username string) *authpkg.CallerIdentity {
	return &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: username, Team: team}
}

func wsWorker(team, username string) *authpkg.CallerIdentity {
	return &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: username, Team: team}
}

func wsRule(team, mode string, paths ...string) v1beta1.HumanWorkspaceAccessRule {
	return v1beta1.HumanWorkspaceAccessRule{Team: team, Paths: paths, Mode: mode}
}

// wsOp is one authorized request in the matrix below. path is
// files-relative ("" = the files root).
type wsOp struct {
	name   string
	method string
	path   string
	caller *authpkg.CallerIdentity
	body   string
	want   int
}

func wsDo(t *testing.T, h *humanFilesHandler, op wsOp) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	var req *http.Request
	if op.body != "" {
		req = hfReq(t, op.method, "alice", op.path, op.caller, strings.NewReader(op.body))
	} else {
		req = hfReq(t, op.method, "alice", op.path, op.caller, nil)
	}
	switch op.method {
	case http.MethodGet:
		h.getFile(rec, req)
	case http.MethodPut:
		h.putFile(rec, req)
	case http.MethodDelete:
		h.deleteFile(rec, req)
	default:
		t.Fatalf("wsDo: unsupported method %s", op.method)
	}
	return rec
}

// wsSeed writes one object under the human's files area.
func wsSeed(t *testing.T, store *ossfake.Memory, path, content string) {
	t.Helper()
	if err := store.PutObject(context.Background(), "humans/alice/files/"+path, []byte(content)); err != nil {
		t.Fatal(err)
	}
}

// --- The #1414 P2 decision matrix (task book §3.5) --------------------------

func TestHumanFiles_WorkspaceAccessMatrix(t *testing.T) {
	cases := []struct {
		name  string
		spec  v1beta1.HumanSpec
		teams []*v1beta1.Team
		seed  []string // "path=content" under humans/alice/files/
		ops   []wsOp
	}{
		{
			// 1. Default-team membership (accessibleTeams ∪ humanMembers)
			//    grants readwrite on files/**; a non-member team 404s.
			name: "default team membership readwrite",
			spec: v1beta1.HumanSpec{
				DisplayName:     "Alice",
				AccessibleTeams: []string{"team-a"},
			},
			teams: []*v1beta1.Team{wsTeam("team-a", "alice"), wsTeam("team-b")},
			seed:  []string{"a.txt=hello"},
			ops: []wsOp{
				{"leader read", http.MethodGet, "a.txt", wsLeader("team-a", "ta-leader"), "", http.StatusOK},
				{"leader write", http.MethodPut, "b.txt", wsLeader("team-a", "ta-leader"), "x", http.StatusOK},
				{"worker read", http.MethodGet, "a.txt", wsWorker("team-a", "ta-worker"), "", http.StatusOK},
				{"non-member team read 404", http.MethodGet, "a.txt", wsLeader("team-b", "tb-leader"), "", http.StatusNotFound},
				{"non-member team write 404", http.MethodPut, "c.txt", wsLeader("team-b", "tb-leader"), "x", http.StatusNotFound},
			},
		},
		{
			// 2. A read grant for a non-member team: reads (incl. the root
			//    list) 200, writes 403, other teams still 404.
			name: "grant read to non-member team",
			spec: v1beta1.HumanSpec{
				DisplayName: "Alice",
				WorkspaceAccess: &v1beta1.HumanWorkspaceAccess{
					Grants: []v1beta1.HumanWorkspaceAccessRule{
						wsRule("team-b", "read", "files/**"),
					},
				},
			},
			teams: []*v1beta1.Team{wsTeam("team-a"), wsTeam("team-b")},
			seed:  []string{"a.txt=hello"},
			ops: []wsOp{
				{"granted read", http.MethodGet, "a.txt", wsLeader("team-b", "tb-leader"), "", http.StatusOK},
				{"granted root list", http.MethodGet, "", wsLeader("team-b", "tb-leader"), "", http.StatusOK},
				{"granted write 403", http.MethodPut, "b.txt", wsLeader("team-b", "tb-leader"), "x", http.StatusForbidden},
				{"other team still 404", http.MethodGet, "a.txt", wsLeader("team-a", "ta-leader"), "", http.StatusNotFound},
			},
		},
		{
			// 3. A readwrite grant for a non-member team: full file CRUD.
			name: "grant readwrite to non-member team",
			spec: v1beta1.HumanSpec{
				DisplayName: "Alice",
				WorkspaceAccess: &v1beta1.HumanWorkspaceAccess{
					Grants: []v1beta1.HumanWorkspaceAccessRule{
						wsRule("team-b", "readwrite", "files/**"),
					},
				},
			},
			teams: []*v1beta1.Team{wsTeam("team-b")},
			seed:  []string{"a.txt=hello"},
			ops: []wsOp{
				{"read", http.MethodGet, "a.txt", wsLeader("team-b", "tb-leader"), "", http.StatusOK},
				{"write", http.MethodPut, "b.txt", wsLeader("team-b", "tb-leader"), "x", http.StatusOK},
				{"delete", http.MethodDelete, "b.txt", wsLeader("team-b", "tb-leader"), "", http.StatusOK},
				{"worker write", http.MethodPut, "c.txt", wsWorker("team-b", "tb-worker"), "y", http.StatusOK},
			},
		},
		{
			// 4. A path-scoped grant: only the matched subtree is
			//    reachable; the files root is NOT covered by "files/notes/*".
			name: "grant path scope only",
			spec: v1beta1.HumanSpec{
				DisplayName: "Alice",
				WorkspaceAccess: &v1beta1.HumanWorkspaceAccess{
					Grants: []v1beta1.HumanWorkspaceAccessRule{
						wsRule("team-b", "read", "files/notes/*"),
					},
				},
			},
			teams: []*v1beta1.Team{wsTeam("team-b")},
			seed:  []string{"notes/n1.txt=one", "other/o.txt=two"},
			ops: []wsOp{
				{"in scope", http.MethodGet, "notes/n1.txt", wsLeader("team-b", "tb-leader"), "", http.StatusOK},
				{"out of scope 404", http.MethodGet, "other/o.txt", wsLeader("team-b", "tb-leader"), "", http.StatusNotFound},
				{"in scope write 403", http.MethodPut, "notes/n1.txt", wsLeader("team-b", "tb-leader"), "x", http.StatusForbidden},
				{"root list 404", http.MethodGet, "", wsLeader("team-b", "tb-leader"), "", http.StatusNotFound},
			},
		},
		{
			// 5. A restriction "none" revokes the default team's readwrite.
			name: "default plus restriction none",
			spec: v1beta1.HumanSpec{
				DisplayName:     "Alice",
				AccessibleTeams: []string{"team-a"},
				WorkspaceAccess: &v1beta1.HumanWorkspaceAccess{
					Restrictions: []v1beta1.HumanWorkspaceAccessRule{
						wsRule("team-a", "none", "files/**"),
					},
				},
			},
			teams: []*v1beta1.Team{wsTeam("team-a", "alice")},
			seed:  []string{"a.txt=hello"},
			ops: []wsOp{
				{"read 404", http.MethodGet, "a.txt", wsLeader("team-a", "ta-leader"), "", http.StatusNotFound},
				{"write 404", http.MethodPut, "b.txt", wsLeader("team-a", "ta-leader"), "x", http.StatusNotFound},
			},
		},
		{
			// 6. A restriction "read" caps the default team at read.
			name: "default plus restriction read",
			spec: v1beta1.HumanSpec{
				DisplayName:     "Alice",
				AccessibleTeams: []string{"team-a"},
				WorkspaceAccess: &v1beta1.HumanWorkspaceAccess{
					Restrictions: []v1beta1.HumanWorkspaceAccessRule{
						wsRule("team-a", "read", "files/**"),
					},
				},
			},
			teams: []*v1beta1.Team{wsTeam("team-a", "alice")},
			seed:  []string{"a.txt=hello"},
			ops: []wsOp{
				{"read", http.MethodGet, "a.txt", wsLeader("team-a", "ta-leader"), "", http.StatusOK},
				{"write 403", http.MethodPut, "b.txt", wsLeader("team-a", "ta-leader"), "x", http.StatusForbidden},
				{"delete 403", http.MethodDelete, "a.txt", wsLeader("team-a", "ta-leader"), "", http.StatusForbidden},
			},
		},
		{
			// 7. Grant + restriction on the same path: the restriction wins
			//    (fail closed), but only on the restricted subtree.
			name: "restriction wins over grant same path",
			spec: v1beta1.HumanSpec{
				DisplayName:     "Alice",
				AccessibleTeams: []string{"team-a"},
				WorkspaceAccess: &v1beta1.HumanWorkspaceAccess{
					Grants: []v1beta1.HumanWorkspaceAccessRule{
						wsRule("team-a", "readwrite", "files/shared/**"),
					},
					Restrictions: []v1beta1.HumanWorkspaceAccessRule{
						wsRule("team-a", "read", "files/shared/**"),
					},
				},
			},
			teams: []*v1beta1.Team{wsTeam("team-a", "alice")},
			seed:  []string{"shared/x.txt=x"},
			ops: []wsOp{
				{"restricted read", http.MethodGet, "shared/x.txt", wsLeader("team-a", "ta-leader"), "", http.StatusOK},
				{"restricted write 403", http.MethodPut, "shared/y.txt", wsLeader("team-a", "ta-leader"), "x", http.StatusForbidden},
				{"unrestricted write", http.MethodPut, "plain/y.txt", wsLeader("team-a", "ta-leader"), "x", http.StatusOK},
			},
		},
		{
			// 8. Restrictions are a global deny: a restriction on a
			//    non-member team caps the access its own grant raises
			//    (deny-wins), and an ungranted path still 404s.
			name: "global deny: restriction caps a non-member grant",
			spec: v1beta1.HumanSpec{
				DisplayName: "Alice",
				WorkspaceAccess: &v1beta1.HumanWorkspaceAccess{
					Grants: []v1beta1.HumanWorkspaceAccessRule{
						wsRule("team-b", "readwrite", "files/shared/**"),
					},
					Restrictions: []v1beta1.HumanWorkspaceAccessRule{
						wsRule("team-b", "read", "files/shared/**"),
					},
				},
			},
			teams: []*v1beta1.Team{wsTeam("team-b")},
			seed:  []string{"shared/x.txt=x"},
			ops: []wsOp{
				{"capped read", http.MethodGet, "shared/x.txt", wsLeader("team-b", "tb-leader"), "", http.StatusOK},
				{"capped write 403", http.MethodPut, "shared/y.txt", wsLeader("team-b", "tb-leader"), "x", http.StatusForbidden},
				{"ungranted 404", http.MethodPut, "outside.txt", wsLeader("team-b", "tb-leader"), "x", http.StatusNotFound},
			},
		},
		{
			// 9. memory/** never enters the files API: even a default team
			//    with readwrite on files/** cannot reach the human's memory
			//    area through this route.
			name: "memory area not addressable",
			spec: v1beta1.HumanSpec{
				DisplayName:     "Alice",
				AccessibleTeams: []string{"team-a"},
			},
			teams: []*v1beta1.Team{wsTeam("team-a", "alice")},
			ops: []wsOp{
				{"memory file 404", http.MethodGet, "memory/secret.txt", wsLeader("team-a", "ta-leader"), "", http.StatusNotFound},
				{"root list 200", http.MethodGet, "", wsLeader("team-a", "ta-leader"), "", http.StatusOK},
			},
		},
		{
			// 10. Admin and manager bypass workspaceAccess entirely.
			name: "admin and manager bypass restrictions",
			spec: v1beta1.HumanSpec{
				DisplayName:     "Alice",
				AccessibleTeams: []string{"team-a"},
				WorkspaceAccess: &v1beta1.HumanWorkspaceAccess{
					Restrictions: []v1beta1.HumanWorkspaceAccessRule{
						wsRule("team-a", "none", "files/**"),
					},
				},
			},
			teams: []*v1beta1.Team{wsTeam("team-a", "alice")},
			seed:  []string{"a.txt=hello"},
			ops: []wsOp{
				{"admin read", http.MethodGet, "a.txt", hfAdmin(), "", http.StatusOK},
				{"admin write", http.MethodPut, "b.txt", hfAdmin(), "x", http.StatusOK},
				{"manager read", http.MethodGet, "a.txt", hfManager(), "", http.StatusOK},
				{"manager write", http.MethodPut, "c.txt", hfManager(), "x", http.StatusOK},
			},
		},
		{
			// 11. The human themself is unaffected by restrictions.
			name: "self access unaffected",
			spec: v1beta1.HumanSpec{
				DisplayName:     "Alice",
				AccessibleTeams: []string{"team-a"},
				WorkspaceAccess: &v1beta1.HumanWorkspaceAccess{
					Restrictions: []v1beta1.HumanWorkspaceAccessRule{
						wsRule("team-a", "none", "files/**"),
					},
				},
			},
			teams: []*v1beta1.Team{wsTeam("team-a", "alice")},
			seed:  []string{"a.txt=hello"},
			ops: []wsOp{
				{"self read", http.MethodGet, "a.txt", hfSelf(), "", http.StatusOK},
				{"self write", http.MethodPut, "b.txt", hfSelf(), "x", http.StatusOK},
			},
		},
		{
			// 12. A human addressed through a team identity uses its Teams
			//    set for the default-team check.
			name: "human via team identity",
			spec: v1beta1.HumanSpec{
				DisplayName:     "Alice",
				AccessibleTeams: []string{"team-a"},
			},
			teams: []*v1beta1.Team{wsTeam("team-a", "alice")},
			seed:  []string{"a.txt=hello"},
			ops: []wsOp{
				{"read", http.MethodGet, "a.txt",
					&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "dana", Teams: []string{"team-a"}},
					"", http.StatusOK},
				{"write", http.MethodPut, "b.txt",
					&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "dana", Teams: []string{"team-a"}},
					"x", http.StatusOK},
			},
		},
		{
			// 13. Team.spec.admin grants NOTHING: administering a team is
			//    not a source of file-area access (explicit negative).
			name: "team admin grants nothing",
			spec: v1beta1.HumanSpec{DisplayName: "Alice"},
			teams: func() []*v1beta1.Team {
				tc := wsTeam("team-c")
				tc.Spec.Admin = &v1beta1.TeamAdminSpec{Name: "alice"}
				return []*v1beta1.Team{tc}
			}(),
			seed: []string{"a.txt=hello"},
			ops: []wsOp{
				{"admin team leader read 404", http.MethodGet, "a.txt", wsLeader("team-c", "tc-leader"), "", http.StatusNotFound},
				{"admin team leader write 404", http.MethodPut, "b.txt", wsLeader("team-c", "tc-leader"), "x", http.StatusNotFound},
				{"admin team worker read 404", http.MethodGet, "a.txt", wsWorker("team-c", "tc-worker"), "", http.StatusNotFound},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, store := newWorkspaceAccessRig(t, tc.spec, tc.teams...)
			for _, s := range tc.seed {
				path, content, _ := strings.Cut(s, "=")
				wsSeed(t, store, path, content)
			}
			if tc.name == "memory area not addressable" {
				// The memory object exists in storage, out of band; the
				// files API must not expose it.
				if err := store.PutObject(context.Background(), "humans/alice/memory/secret.txt", []byte("shh")); err != nil {
					t.Fatal(err)
				}
			}
			for _, op := range tc.ops {
				t.Run(op.name, func(t *testing.T) {
					rec := wsDo(t, h, op)
					if rec.Code != op.want {
						t.Fatalf("%s %s: got %d, want %d (body: %s)", op.method, op.path, rec.Code, op.want, rec.Body.String())
					}
				})
			}
		})
	}
}

// --- UpdateHuman workspaceAccess validation (task book cases 9 + 11) -------

// wsGrantBody builds a PUT /humans body with one grants rule.
func wsGrantBody(team, mode string, paths ...string) string {
	elems := make([]string, 0, len(paths))
	for _, p := range paths {
		elems = append(elems, fmt.Sprintf("%q", p))
	}
	return fmt.Sprintf(`{"workspaceAccess":{"grants":[{"team":%q,"paths":[%s],"mode":%q}]}}`, team, strings.Join(elems, ","), mode)
}

func wsRestrictionBody(team, mode string, paths ...string) string {
	elems := make([]string, 0, len(paths))
	for _, p := range paths {
		elems = append(elems, fmt.Sprintf("%q", p))
	}
	return fmt.Sprintf(`{"workspaceAccess":{"restrictions":[{"team":%q,"paths":[%s],"mode":%q}]}}`, team, strings.Join(elems, ","), mode)
}

func newWorkspaceUpdateRig(t *testing.T) (*ResourceHandler, client.Client) {
	t.Helper()
	scheme := newServerTestScheme(t)
	team := &v1beta1.Team{ObjectMeta: metav1.ObjectMeta{Name: "market-team", Namespace: "default"}}
	human := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Spec: v1beta1.HumanSpec{
			DisplayName:     "Alice",
			PermissionLevel: 2,
			AccessibleTeams: []string{"market-team"},
		},
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team, human).Build()
	return NewResourceHandler(k8s, "default", nil, "", nil), k8s
}

func TestUpdateHuman_WorkspaceAccessValidation(t *testing.T) {
	handler, _ := newWorkspaceUpdateRig(t)

	manyPaths := make([]string, 17)
	for i := range manyPaths {
		manyPaths[i] = fmt.Sprintf("files/p%d", i+1)
	}
	rule33 := make([]string, 33)
	for i := range rule33 {
		rule33[i] = fmt.Sprintf(`{"team":"team-%d","paths":["files/**"],"mode":"read"}`, i)
	}

	cases := []struct {
		name string
		body string
		want string // substring expected in the 400 message
	}{
		{"grant memory area", wsGrantBody("team-a", "read", "memory/**"), "files/"},
		{"grant digest area", wsGrantBody("team-a", "read", "digest/**"), "files/"},
		{"grant non-files path", wsGrantBody("team-a", "read", "notes/*"), "files/"},
		{"grant bare files", wsGrantBody("team-a", "read", "files"), "files/"},
		{"grant absolute path", wsGrantBody("team-a", "read", "/files/x"), "absolute"},
		{"grant backslash", wsGrantBody("team-a", "read", `files/a\b`), "backslash"},
		{"grant dotdot", wsGrantBody("team-a", "read", "files/../a"), ".."},
		{"grant reserved .trash", wsGrantBody("team-a", "read", "files/.trash/x"), "reserved"},
		{"grant trailing slash", wsGrantBody("team-a", "read", "files/a/"), "end with /"},
		{"grant control char", `{"workspaceAccess":{"grants":[{"team":"team-a","paths":["files/a\x00b"],"mode":"read"}]}}`, "invalid"},
		{"grant mode write", wsGrantBody("team-a", "write", "files/**"), "read, readwrite"},
		{"grant mode none", wsGrantBody("team-a", "none", "files/**"), "read, readwrite"},
		{"restriction mode readwrite", wsRestrictionBody("team-a", "readwrite", "files/**"), "none, read"},
		{"restriction mode write", wsRestrictionBody("team-a", "write", "files/**"), "none, read"},
		{"duplicate team in grants", `{"workspaceAccess":{"grants":[` +
			`{"team":"team-a","paths":["files/**"],"mode":"read"},` +
			`{"team":"team-a","paths":["files/x"],"mode":"read"}]}}`, "duplicate team"},
		{"empty team", wsGrantBody("", "read", "files/**"), "team must not be empty"},
		{"empty paths", `{"workspaceAccess":{"grants":[{"team":"team-a","paths":[],"mode":"read"}]}}`, "at least 1"},
		{"too many paths", wsGrantBody("team-a", "read", manyPaths...), "at most 16"},
		{"33 rules", `{"workspaceAccess":{"grants":[` + strings.Join(rule33, ",") + `]}}`, "at most 32"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := putHuman(t, handler, "alice", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("message %q does not contain %q", rec.Body.String(), tc.want)
			}
		})
	}
}

// TestUpdateHuman_WorkspaceAccessSemantics covers the apply/clear/keep
// merge-patch behavior of workspaceAccess on the Human CR.
func TestUpdateHuman_WorkspaceAccessSemantics(t *testing.T) {
	handler, k8s := newWorkspaceUpdateRig(t)
	getSpec := func(t *testing.T) *v1beta1.HumanWorkspaceAccess {
		t.Helper()
		var human v1beta1.Human
		if err := k8s.Get(context.Background(), client.ObjectKey{Name: "alice", Namespace: "default"}, &human); err != nil {
			t.Fatal(err)
		}
		return human.Spec.WorkspaceAccess
	}

	// 1. A valid grant applies and persists.
	rec := putHuman(t, handler, "alice", wsGrantBody("market-team", "readwrite", "files/**"))
	if rec.Code != http.StatusOK {
		t.Fatalf("apply grant: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	wa := getSpec(t)
	if wa == nil || len(wa.Grants) != 1 || wa.Grants[0].Team != "market-team" {
		t.Fatalf("grant not persisted: %+v", wa)
	}

	// 2. The same team in both lists is allowed; the restriction wins at
	//    decision time (covered by the matrix), here just schema-level.
	rec = putHuman(t, handler, "alice", `{"workspaceAccess":{"grants":[`+
		`{"team":"market-team","paths":["files/**"],"mode":"readwrite"}],`+
		`"restrictions":[{"team":"market-team","paths":["files/secret/**"],"mode":"none"}]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("both lists: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	wa = getSpec(t)
	if wa == nil || len(wa.Grants) != 1 || len(wa.Restrictions) != 1 {
		t.Fatalf("both lists not persisted: %+v", wa)
	}

	// 3. Absent (and explicit null) leaves the spec unchanged — Go JSON
	//    decodes null on a pointer to nil, indistinguishable from absent.
	rec = putHuman(t, handler, "alice", `{"note":"touch"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("absent: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	wa = getSpec(t)
	if wa == nil || len(wa.Restrictions) != 1 {
		t.Fatalf("absent clobbered workspaceAccess: %+v", wa)
	}
	rec = putHuman(t, handler, "alice", `{"workspaceAccess":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("null: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	wa = getSpec(t)
	if wa == nil || len(wa.Restrictions) != 1 {
		t.Fatalf("null clobbered workspaceAccess: %+v", wa)
	}

	// 4. An empty object replaces with empty lists (clears the rules).
	rec = putHuman(t, handler, "alice", `{"workspaceAccess":{}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	wa = getSpec(t)
	if wa == nil {
		t.Fatalf("clear left workspaceAccess nil")
	}
	if len(wa.Grants) != 0 || len(wa.Restrictions) != 0 {
		t.Fatalf("clear left rules: %+v", wa)
	}
}

// --- Unit tests: glob matcher and the in-package validator ------------------

func TestHumanWorkspaceGlobMatch(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"files/**", "files", true}, // root is the bare "files"
		{"files/*", "files", false}, // direct children only, root excluded
		{"files/**", "files/a.txt", true},
		{"files/**", "files/a/b/c.txt", true},
		{"files/*", "files/a.txt", true},
		{"files/*", "files/a/b.txt", false},
		{"files/notes/*", "files/notes/n1.md", true},
		{"files/notes/*", "files/notes/deep/n1.md", false},
		{"files/notes/*", "files/other/n1.md", false},
		{"files/a*txt", "files/ab.txt", true},
		{"files/a*txt", "files/xyz.txt", false},
		{"files/a*-b.txt", "files/a1-2-b.txt", true},
		{"files/**/end.txt", "files/end.txt", true}, // ** = zero segments
		{"files/**/end.txt", "files/x/end.txt", true},
		{"files/**/end.txt", "files/x/y/end.txt", true},
		{"files/**/end.txt", "files/x/end.md", false},
		{"files/doc*/ch*.md", "files/docs2/chapter1.md", true},
		{"files/doc*/ch*.md", "files/docs2/chapter1.pdf", false},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+" vs "+tc.path, func(t *testing.T) {
			if got := humanWorkspaceGlobMatch(tc.pattern, tc.path); got != tc.want {
				t.Fatalf("humanWorkspaceGlobMatch(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
			}
		})
	}
}

func TestValidateHumanWorkspaceAccess(t *testing.T) {
	valid := &v1beta1.HumanWorkspaceAccess{
		Grants:       []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", "read", "files/**")},
		Restrictions: []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", "none", "files/secret/**")},
	}
	if err := validateHumanWorkspaceAccess(nil); err != nil {
		t.Fatalf("nil: %v", err)
	}
	if err := validateHumanWorkspaceAccess(valid); err != nil {
		t.Fatalf("valid: %v", err)
	}
	// Boundary sizes pass: 16 paths, 32 rules.
	wide := &v1beta1.HumanWorkspaceAccess{
		Grants: []v1beta1.HumanWorkspaceAccessRule{
			{Team: "team-a", Mode: "read", Paths: make([]string, 16)},
		},
	}
	for i := range wide.Grants[0].Paths {
		wide.Grants[0].Paths[i] = fmt.Sprintf("files/p%d", i)
	}
	if err := validateHumanWorkspaceAccess(wide); err != nil {
		t.Fatalf("16 paths: %v", err)
	}
	deep := &v1beta1.HumanWorkspaceAccess{}
	for i := 0; i < 32; i++ {
		deep.Grants = append(deep.Grants, wsRule(fmt.Sprintf("team-%d", i), "read", "files/**"))
	}
	if err := validateHumanWorkspaceAccess(deep); err != nil {
		t.Fatalf("32 rules: %v", err)
	}

	invalid := []struct {
		name string
		wa   *v1beta1.HumanWorkspaceAccess
		want string
	}{
		{"memory grant", &v1beta1.HumanWorkspaceAccess{Grants: []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", "read", "memory/**")}}, "files/"},
		{"digest grant", &v1beta1.HumanWorkspaceAccess{Grants: []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", "read", "digest/**")}}, "files/"},
		{"non-files grant", &v1beta1.HumanWorkspaceAccess{Grants: []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", "read", "notes/*")}}, "files/"},
		{"absolute path", &v1beta1.HumanWorkspaceAccess{Grants: []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", "read", "/files/x")}}, "absolute"},
		{"backslash path", &v1beta1.HumanWorkspaceAccess{Grants: []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", "read", `files/a\b`)}}, "backslash"},
		{"dotdot path", &v1beta1.HumanWorkspaceAccess{Grants: []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", "read", "files/../a")}}, ".."},
		{"empty segment", &v1beta1.HumanWorkspaceAccess{Grants: []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", "read", "files//a")}}, "empty segment"},
		{"reserved .trash", &v1beta1.HumanWorkspaceAccess{Grants: []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", "read", "files/.trash/x")}}, "reserved"},
		{"control char", &v1beta1.HumanWorkspaceAccess{Grants: []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", "read", "files/a\x00b")}}, "control"},
		{"grant mode none", &v1beta1.HumanWorkspaceAccess{Grants: []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", "none", "files/**")}}, "read, readwrite"},
		{"grant mode write", &v1beta1.HumanWorkspaceAccess{Grants: []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", "write", "files/**")}}, "read, readwrite"},
		{"restriction mode readwrite", &v1beta1.HumanWorkspaceAccess{Restrictions: []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", "readwrite", "files/**")}}, "none, read"},
		{"duplicate team", &v1beta1.HumanWorkspaceAccess{Grants: []v1beta1.HumanWorkspaceAccessRule{
			wsRule("team-a", "read", "files/**"), wsRule("team-a", "read", "files/x")}}, "duplicate team"},
		{"empty team", &v1beta1.HumanWorkspaceAccess{Grants: []v1beta1.HumanWorkspaceAccessRule{wsRule("", "read", "files/**")}}, "team must not be empty"},
		{"empty paths", &v1beta1.HumanWorkspaceAccess{Grants: []v1beta1.HumanWorkspaceAccessRule{{Team: "team-a", Mode: "read"}}}, "at least 1"},
		{"17 paths", &v1beta1.HumanWorkspaceAccess{Grants: []v1beta1.HumanWorkspaceAccessRule{
			{Team: "team-a", Mode: "read", Paths: make([]string, 17)}}}, "at most 16"},
		{"33 rules", func() *v1beta1.HumanWorkspaceAccess {
			wa := &v1beta1.HumanWorkspaceAccess{}
			for i := 0; i < 33; i++ {
				wa.Grants = append(wa.Grants, wsRule(fmt.Sprintf("team-%d", i), "read", "files/**"))
			}
			return wa
		}(), "at most 32"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHumanWorkspaceAccess(tc.wa)
			if err == nil {
				t.Fatalf("expected error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// --- Write attribution audit (#1414 P2) --------------------------------------

// TestHumanFiles_TeamWriteAuditAttribution verifies that a successful
// write by a team caller records the caller identity plus the team that
// produced the access (onBehalfOf + team in the audit Detail).
func TestHumanFiles_TeamWriteAuditAttribution(t *testing.T) {
	h, store := newWorkspaceAccessRig(
		t,
		v1beta1.HumanSpec{DisplayName: "Alice", AccessibleTeams: []string{"team-a"}},
		wsTeam("team-a", "alice"),
	)
	rec := wsDo(t, h, wsOp{"team write", http.MethodPut, "report/q3.txt", wsLeader("team-a", "ta-leader"), "content", http.StatusOK})
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}

	lines := readAuditLines(t, store)
	var writeLine string
	for _, line := range lines {
		if m := decodeAuditLine(t, line); m["action"] == "human_file_write" {
			writeLine = line
			break
		}
	}
	if writeLine == "" {
		t.Fatalf("no human_file_write audit line: %v", lines)
	}
	m := decodeAuditLine(t, writeLine)
	if m["who"] != "ta-leader" || m["role"] != "team-leader" {
		t.Fatalf("caller identity = %v %v, want ta-leader / team-leader", m["who"], m["role"])
	}
	if m["target"] != "alice" {
		t.Fatalf("target = %v, want alice", m["target"])
	}
	if got := detailField(t, writeLine, "onBehalfOf"); got != "alice" {
		t.Fatalf("onBehalfOf = %v, want alice", got)
	}
	if got := detailField(t, writeLine, "team"); got != "team-a" {
		t.Fatalf("team = %v, want team-a", got)
	}
	if got := detailField(t, writeLine, "path"); got != "files/report/q3.txt" {
		t.Fatalf("path = %v, want files/report/q3.txt", got)
	}
}

// Recursive listing must not expose denied files through either entries or
// the pagination cursor, and walking every page must retain readable files.
func TestWorkspaceAccessRecursivePaginationHidesRestrictedNames(t *testing.T) {
	h, store := newWorkspaceAccessRig(t, v1beta1.HumanSpec{
		AccessibleTeams: []string{"team-a"},
		WorkspaceAccess: &v1beta1.HumanWorkspaceAccess{Restrictions: []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", "none", "files/private/**")}},
	}, wsTeam("team-a"))
	for _, name := range []string{"private/a.txt", "private/b.txt", "public/a.txt", "public/b.txt"} {
		wsSeed(t, store, name, "owned data")
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 6; page++ {
		req := hfReq(t, http.MethodGet, "alice", "", wsWorker("team-a", "worker-a"), nil)
		query := req.URL.Query()
		query.Set("recursive", "1")
		query.Set("limit", "1")
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		req.URL.RawQuery = query.Encode()
		rec := httptest.NewRecorder()
		h.listFiles(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "private") {
			t.Fatalf("restricted name exposed: %s", rec.Body.String())
		}
		var body humanFileRecursiveResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, entry := range body.Entries {
			if seen[entry.Path] {
				t.Fatalf("duplicate %s", entry.Path)
			}
			seen[entry.Path] = true
		}
		if body.NextKey == nil {
			break
		}
		if *body.NextKey == cursor {
			t.Fatal("cursor did not advance")
		}
		cursor = *body.NextKey
	}
	if !seen["files/public/a.txt"] || !seen["files/public/b.txt"] || len(seen) != 2 {
		t.Fatalf("readable files lost: %v", seen)
	}
}

func TestWorkspaceAccessDirectUploadRequiresWriteAtIssueAndCommit(t *testing.T) {
	for _, mode := range []string{"read", "none"} {
		t.Run(mode, func(t *testing.T) {
			h, _ := newWorkspaceAccessRig(t, v1beta1.HumanSpec{AccessibleTeams: []string{"team-a"}, WorkspaceAccess: &v1beta1.HumanWorkspaceAccess{Restrictions: []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", mode, "files/notes/**")}}}, wsTeam("team-a"))
			want := http.StatusForbidden
			if mode == "none" {
				want = http.StatusNotFound
			}
			for _, commit := range []bool{false, true} {
				req := hfReq(t, http.MethodPost, "alice", "notes/n.txt", wsWorker("team-a", "worker-a"), nil)
				rec := httptest.NewRecorder()
				if commit {
					h.commitPresignedFile(rec, req, "notes/n.txt")
				} else {
					h.presignUploadFile(rec, req, "notes/n.txt")
				}
				if rec.Code != want {
					t.Fatalf("commit=%v status=%d want=%d: %s", commit, rec.Code, want, rec.Body.String())
				}
			}
		})
	}
}
