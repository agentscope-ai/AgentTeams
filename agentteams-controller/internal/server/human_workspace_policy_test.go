package server

// HTTP-chain tests for the workspace access policy (#1502 fix): default
// access follows membership, restrictions are a global deny (deny-wins
// across the caller's teams), and the private/control prefixes are never
// authorizable. Every case goes through the real handler and the real
// authorization chain (authorizeHumanFile); nothing here calls
// humanWorkspaceEffectiveMode directly.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	audit "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// --- Test rig --------------------------------------------------------------

// newPolicyRig builds the human-files handler for human "alice" plus a
// ResourceHandler on the same fake client, so a test can change
// membership or workspaceAccess through the real UpdateHuman API and
// observe the next request re-evaluate the policy live.
func newPolicyRig(t *testing.T, spec v1beta1.HumanSpec, teams ...*v1beta1.Team) (*humanFilesHandler, *ResourceHandler, *ossfake.Memory) {
	t.Helper()
	scheme := newServerTestScheme(t)
	objects := []client.Object{&v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Spec:       spec,
	}}
	for _, tm := range teams {
		objects = append(objects, tm)
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	store := ossfake.NewMemory()
	return NewHumanFilesHandler(k8s, "default", store, audit.NewClient(store)),
		NewResourceHandler(k8s, "default", nil, "", nil),
		store
}

// wsHumanCaller builds a human addressed through a team identity — the
// only caller shape that can carry several teams at once.
func wsHumanCaller(username string, teams ...string) *authpkg.CallerIdentity {
	return &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: username, Teams: teams}
}

// wsRun asserts a batch of handler ops.
func wsRun(t *testing.T, h *humanFilesHandler, ops []wsOp) {
	t.Helper()
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			rec := wsDo(t, h, op)
			if rec.Code != op.want {
				t.Fatalf("%s %s: got %d, want %d (body: %s)", op.method, op.path, rec.Code, op.want, rec.Body.String())
			}
		})
	}
}

// wsTrashID parses the trashId from a 200 delete response.
func wsTrashID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var out struct {
		TrashID string `json:"trashId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.TrashID == "" {
		t.Fatalf("delete response %q: %v", rec.Body.String(), err)
	}
	return out.TrashID
}

// wsRestore invokes the real restore handler for one trash entry.
func wsRestore(t *testing.T, h *humanFilesHandler, id string, caller *authpkg.CallerIdentity) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.restoreTrash(rec, hfTrashReq(t, http.MethodPost, "alice", id, caller, ""))
	return rec
}

// --- 1. Default access follows membership (no workspaceAccess) --------------

// TestHumanFiles_PolicyMembershipBaseline: with no workspaceAccess at
// all, default-team membership alone gives list / read / write on
// files/**; a non-member team 404s (no existence probing).
func TestHumanFiles_PolicyMembershipBaseline(t *testing.T) {
	h, _, store := newPolicyRig(t,
		v1beta1.HumanSpec{DisplayName: "Alice", AccessibleTeams: []string{"team-a"}},
		wsTeam("team-a", "alice"),
	)
	wsSeed(t, store, "a.txt", "hello")
	wsRun(t, h, []wsOp{
		{"list", http.MethodGet, "", wsLeader("team-a", "ta-leader"), "", http.StatusOK},
		{"read", http.MethodGet, "a.txt", wsLeader("team-a", "ta-leader"), "", http.StatusOK},
		{"write", http.MethodPut, "b.txt", wsLeader("team-a", "ta-leader"), "x", http.StatusOK},
		{"worker read", http.MethodGet, "a.txt", wsWorker("team-a", "ta-worker"), "", http.StatusOK},
		{"worker write", http.MethodPut, "c.txt", wsWorker("team-a", "ta-worker"), "y", http.StatusOK},
		{"non-member read 404", http.MethodGet, "a.txt", wsLeader("team-b", "tb-leader"), "", http.StatusNotFound},
		{"non-member write 404", http.MethodPut, "d.txt", wsLeader("team-b", "tb-leader"), "x", http.StatusNotFound},
	})
}

// --- 2. Deny-wins regression: two default teams ------------------------------

// TestHumanFiles_PolicyRestrictionDenyWinsAcrossTeams is the regression
// for the permission expansion the reviewer flagged: team-a and team-b
// are both default teams of alice; team-a restricts the secret subtree
// to read and team-b is unrestricted. Under the old per-team
// composition the caller (a member of both teams) got write back through
// team-b; the global deny keeps the capped subtree at read (write 403)
// while the unrestricted subtree stays fully writable. Restore re-checks
// the per-path mode, so it follows the same decision.
//
// (The task book sketched the restriction as "files/secret*"; in this
// codebase "*" matches exactly one segment, so it cannot cover
// "files/secret/x". "files/secret/**" expresses the same intent — the
// whole secret subtree — and is what the case uses.)
func TestHumanFiles_PolicyRestrictionDenyWinsAcrossTeams(t *testing.T) {
	h, _, store := newPolicyRig(t,
		v1beta1.HumanSpec{
			DisplayName:     "Alice",
			AccessibleTeams: []string{"team-a", "team-b"},
			WorkspaceAccess: &v1beta1.HumanWorkspaceAccess{
				Restrictions: []v1beta1.HumanWorkspaceAccessRule{
					wsRule("team-a", "read", "files/secret/**"),
				},
			},
		},
		wsTeam("team-a", "alice"),
		wsTeam("team-b", "alice"),
	)
	wsSeed(t, store, "secret/x.txt", "shh")
	wsSeed(t, store, "secret/del.txt", "bye")
	wsSeed(t, store, "pub/x.txt", "ok")
	wsSeed(t, store, "pub/del.txt", "bye")

	dana := wsHumanCaller("dana", "team-a", "team-b")
	wsRun(t, h, []wsOp{
		{"list", http.MethodGet, "", dana, "", http.StatusOK},
		{"read secret capped", http.MethodGet, "secret/x.txt", dana, "", http.StatusOK},
		{"write secret denied", http.MethodPut, "secret/y.txt", dana, "x", http.StatusForbidden},
		{"delete secret denied", http.MethodDelete, "secret/del.txt", dana, "", http.StatusForbidden},
		{"read pub", http.MethodGet, "pub/x.txt", dana, "", http.StatusOK},
		{"write pub", http.MethodPut, "pub/y.txt", dana, "x", http.StatusOK},
	})

	// Trash entries at one capped and one uncapped path (created by the
	// human themself, who is exempt from the team policy).
	idSecret := wsTrashID(t, wsDo(t, h, wsOp{"self delete secret", http.MethodDelete, "secret/del.txt", hfSelf(), "", http.StatusOK}))
	idPub := wsTrashID(t, wsDo(t, h, wsOp{"self delete pub", http.MethodDelete, "pub/del.txt", hfSelf(), "", http.StatusOK}))

	if rec := wsRestore(t, h, idSecret, dana); rec.Code != http.StatusForbidden {
		t.Fatalf("restore of the capped path: got %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
	if rec := wsRestore(t, h, idPub, dana); rec.Code != http.StatusOK {
		t.Fatalf("restore of the uncapped path: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

// --- 3. Grant raised, restriction capped (no default baseline) ---------------

// TestHumanFiles_PolicyRestrictionWinsOverGrant: team-b is a non-member
// team granted readwrite on files/shared/** and team-a (also one of the
// caller's teams) restricts the same subtree to read — with NO default
// baseline involved. The restriction wins over the grant even though the
// grant is the only source of access (deny-wins is not limited to
// default teams).
func TestHumanFiles_PolicyRestrictionWinsOverGrant(t *testing.T) {
	h, _, store := newPolicyRig(t,
		v1beta1.HumanSpec{
			DisplayName: "Alice",
			WorkspaceAccess: &v1beta1.HumanWorkspaceAccess{
				Grants:       []v1beta1.HumanWorkspaceAccessRule{wsRule("team-b", "readwrite", "files/shared/**")},
				Restrictions: []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", "read", "files/shared/**")},
			},
		},
		wsTeam("team-a"),
		wsTeam("team-b"),
	)
	wsSeed(t, store, "shared/x.txt", "x")
	dana := wsHumanCaller("dana", "team-a", "team-b")
	wsRun(t, h, []wsOp{
		{"capped read", http.MethodGet, "shared/x.txt", dana, "", http.StatusOK},
		{"capped write 403", http.MethodPut, "shared/y.txt", dana, "x", http.StatusForbidden},
		{"capped delete 403", http.MethodDelete, "shared/x.txt", dana, "", http.StatusForbidden},
		{"ungranted path 404", http.MethodGet, "other/x.txt", dana, "", http.StatusNotFound},
	})
}

// --- 4. Revocation paths ------------------------------------------------------

// TestHumanFiles_PolicyMembershipRemoval: dropping the caller's team from
// the human's default set through the real UpdateHuman API removes the
// baseline. With no grant left, access 404s like a non-existent human;
// with a leftover read grant, read stays 200 and write 403s.
func TestHumanFiles_PolicyMembershipRemoval(t *testing.T) {
	h, rh, store := newPolicyRig(t,
		v1beta1.HumanSpec{DisplayName: "Alice", AccessibleTeams: []string{"team-a"}},
		wsTeam("team-a"),
	)
	wsSeed(t, store, "a.txt", "hello")
	lead := wsLeader("team-a", "ta-leader")
	wsRun(t, h, []wsOp{
		{"member read", http.MethodGet, "a.txt", lead, "", http.StatusOK},
		{"member write", http.MethodPut, "b.txt", lead, "x", http.StatusOK},
	})

	if rec := putHuman(t, rh, "alice", `{"accessibleTeams":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("remove membership: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	wsRun(t, h, []wsOp{
		{"removed read 404", http.MethodGet, "a.txt", lead, "", http.StatusNotFound},
		{"removed write 404", http.MethodPut, "c.txt", lead, "x", http.StatusNotFound},
	})

	if rec := putHuman(t, rh, "alice", wsGrantBody("team-a", "read", "files/**")); rec.Code != http.StatusOK {
		t.Fatalf("leftover read grant: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	wsRun(t, h, []wsOp{
		{"grant read", http.MethodGet, "a.txt", lead, "", http.StatusOK},
		{"grant write 403", http.MethodPut, "d.txt", lead, "x", http.StatusForbidden},
	})
}

// TestHumanFiles_PolicyGrantRevocation: a default team with a
// readwrite grant and a read restriction on the same subtree. Revoking
// the grant through the real UpdateHuman API (grants cleared, the
// restriction kept) must NOT restore write: the membership baseline
// stays capped by the same global restriction, so the write keeps 403ing.
func TestHumanFiles_PolicyGrantRevocation(t *testing.T) {
	h, rh, store := newPolicyRig(t,
		v1beta1.HumanSpec{
			DisplayName:     "Alice",
			AccessibleTeams: []string{"team-a"},
			WorkspaceAccess: &v1beta1.HumanWorkspaceAccess{
				Grants:       []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", "readwrite", "files/shared/**")},
				Restrictions: []v1beta1.HumanWorkspaceAccessRule{wsRule("team-a", "read", "files/shared/**")},
			},
		},
		wsTeam("team-a"),
	)
	wsSeed(t, store, "shared/x.txt", "x")
	lead := wsLeader("team-a", "ta-leader")
	wsRun(t, h, []wsOp{
		{"capped read", http.MethodGet, "shared/x.txt", lead, "", http.StatusOK},
		{"capped write 403", http.MethodPut, "shared/y.txt", lead, "x", http.StatusForbidden},
	})

	// Revoke the grant: the workspaceAccess object is replaced whole, so
	// the body clears grants and re-states the restriction.
	body := `{"workspaceAccess":{"grants":[],"restrictions":[{"team":"team-a","paths":["files/shared/**"],"mode":"read"}]}`
	if rec := putHuman(t, rh, "alice", body); rec.Code != http.StatusOK {
		t.Fatalf("revoke grant: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	wsRun(t, h, []wsOp{
		{"still read", http.MethodGet, "shared/x.txt", lead, "", http.StatusOK},
		{"still 403 after revocation", http.MethodPut, "shared/y.txt", lead, "x", http.StatusForbidden},
	})
}

// --- 5. Private and control prefixes are never authorizable ------------------

// TestHumanFiles_PolicyPrivatePrefixesNeverAuthorized: a hand-written
// spec (one validation would reject) grants readwrite over every
// private and control prefix, "fully" covering them; the decision entry
// still denies each of them — grants are invalid under these prefixes —
// while a normal path keeps working. The API-side validation rejects
// rule paths inside the control prefixes as well.
func TestHumanFiles_PolicyPrivatePrefixesNeverAuthorized(t *testing.T) {
	h, rh, store := newPolicyRig(t,
		v1beta1.HumanSpec{
			DisplayName: "Alice",
			// No default membership: the grants below are the only
			// possible source of access for the caller.
			WorkspaceAccess: &v1beta1.HumanWorkspaceAccess{
				Grants: []v1beta1.HumanWorkspaceAccessRule{
					wsRule("team-a", "readwrite", "files/memory/**"),
					wsRule("team-a", "readwrite", "files/digest/**"),
					wsRule("team-a", "readwrite", "files/.control/**"),
					wsRule("team-a", "readwrite", "files/.sync/**"),
					wsRule("team-a", "readwrite", "files/.trash/**"),
					wsRule("team-a", "readwrite", "memory/**"),
					wsRule("team-a", "readwrite", "digest/**"),
				},
			},
		},
		wsTeam("team-a"),
	)
	// Objects that exist in storage: without the decision-time gate the
	// grants above would serve them (200/201) instead of denying.
	wsSeed(t, store, "a.txt", "hello")
	wsSeed(t, store, "memory/secret.txt", "shh")
	wsSeed(t, store, "digest/x.md", "shh")
	wsSeed(t, store, ".control/quota.json", "{}")
	wsSeed(t, store, ".sync/state.json", "{}")
	wsSeed(t, store, ".trash/entry", "shh")

	lead := wsLeader("team-a", "ta-leader")
	wsRun(t, h, []wsOp{
		{"memory 404", http.MethodGet, "memory/secret.txt", lead, "", http.StatusNotFound},
		{"memory write 404", http.MethodPut, "memory/y.txt", lead, "x", http.StatusNotFound},
		{"digest 404", http.MethodGet, "digest/x.md", lead, "", http.StatusNotFound},
		{".control 404", http.MethodGet, ".control/quota.json", lead, "", http.StatusNotFound},
		{".sync 404", http.MethodGet, ".sync/state.json", lead, "", http.StatusNotFound},
		{".trash 404", http.MethodGet, ".trash/entry", lead, "", http.StatusNotFound},
		{"normal path still 200", http.MethodGet, "a.txt", lead, "", http.StatusOK},
	})

	// API-side: grants and restrictions naming the control prefixes are
	// rejected with 400 (memory/digest variants are covered by the
	// existing validation matrix).
	for _, body := range []string{
		wsGrantBody("team-a", "read", "files/.control/x"),
		wsGrantBody("team-a", "read", "files/.sync/x"),
		wsGrantBody("team-a", "read", "files/.trash/x"),
		wsRestrictionBody("team-a", "none", "files/.control/x"),
	} {
		if rec := putHuman(t, rh, "alice", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("rule %s: got %d, want 400 (body: %s)", body, rec.Code, rec.Body.String())
		}
	}
}
