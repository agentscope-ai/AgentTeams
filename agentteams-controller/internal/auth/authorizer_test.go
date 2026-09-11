package auth

import "testing"

func TestAuthorizer_AdminAllowsEverything(t *testing.T) {
	az := NewAuthorizer()
	caller := &CallerIdentity{Role: RoleAdmin, Username: "admin"}

	actions := []Action{ActionCreate, ActionUpdate, ActionDelete, ActionGet, ActionList, ActionWake, ActionSleep}
	for _, a := range actions {
		if err := az.Authorize(caller, AuthzRequest{Action: a, ResourceKind: "worker"}); err != nil {
			t.Errorf("admin should be allowed %s worker, got: %v", a, err)
		}
	}
}

func TestAuthorizer_ManagerAllowsEverything(t *testing.T) {
	az := NewAuthorizer()
	caller := &CallerIdentity{Role: RoleManager, Username: "manager"}

	if err := az.Authorize(caller, AuthzRequest{Action: ActionDelete, ResourceKind: "team", ResourceName: "alpha"}); err != nil {
		t.Errorf("manager should be allowed, got: %v", err)
	}
}

// TestAuthorizer_HumanScoped guards the L2 security boundary: an L2 human
// (RoleHuman) may read projects/teams/workers in scope, may update projects in
// scope (pause/resume/replan/lifecycle, code-level requireSameTeam), and may
// update workers in scope (self-service skill / MCP config — the middleware
// cannot resolve worker -> team, so the UpdateWorker handler enforces the real
// boundary). They must NOT create/delete workers, wake/sleep them, refresh
// credentials, or mutate teams.
func TestAuthorizer_HumanScoped(t *testing.T) {
	az := NewAuthorizer()
	caller := &CallerIdentity{Role: RoleHuman, Username: "maizong", Teams: []string{"market-team"}}

	allowed := []AuthzRequest{
		{Action: ActionList, ResourceKind: "project"},
		{Action: ActionGet, ResourceKind: "project"},
		{Action: ActionUpdate, ResourceKind: "project", ResourceTeam: "market-team"},
		{Action: ActionList, ResourceKind: "team"},
		{Action: ActionGet, ResourceKind: "team"},
		{Action: ActionList, ResourceKind: "worker"},
		{Action: ActionGet, ResourceKind: "worker"},
		{Action: ActionUpdate, ResourceKind: "worker", ResourceTeam: "market-team"},
		{Action: ActionUpdate, ResourceKind: "worker"},
		{Action: ActionGet, ResourceKind: "status"},
	}
	for _, req := range allowed {
		if err := az.Authorize(caller, req); err != nil {
			t.Errorf("L2 human should be allowed %s %s, got: %v", req.Action, req.ResourceKind, err)
		}
	}

	denied := []AuthzRequest{
		{Action: ActionCreate, ResourceKind: "worker"},
		{Action: ActionUpdate, ResourceKind: "worker", ResourceTeam: "another-team"},
		{Action: ActionDelete, ResourceKind: "worker"},
		{Action: ActionWake, ResourceKind: "worker"},
		{Action: ActionSleep, ResourceKind: "worker"},
		{Action: ActionRefreshMatrixToken, ResourceKind: "credentials"},
		{Action: ActionSTS, ResourceKind: "credentials"},
		{Action: ActionUpdate, ResourceKind: "project", ResourceTeam: "another-team"},
		{Action: ActionCreate, ResourceKind: "team"},
		{Action: ActionDelete, ResourceKind: "team"},
	}
	for _, req := range denied {
		if err := az.Authorize(caller, req); err == nil {
			t.Errorf("L2 human must be denied %s %s, got nil error", req.Action, req.ResourceKind)
		}
	}
}

// TestAuthorizer_HumanUpdateAdminOnly guards the human permission-update
// boundary: only admin/manager may PUT /api/v1/humans/{name}.
func TestAuthorizer_HumanUpdateAdminOnly(t *testing.T) {
	az := NewAuthorizer()
	allowed := []CallerIdentity{
		{Role: RoleAdmin, Username: "admin"},
		{Role: RoleManager, Username: "manager"},
	}
	for i := range allowed {
		if err := az.Authorize(&allowed[i], AuthzRequest{Action: ActionUpdate, ResourceKind: "human", ResourceName: "maizong"}); err != nil {
			t.Errorf("%s should be allowed to update humans, got: %v", allowed[i].Role, err)
		}
	}
	denied := []CallerIdentity{
		{Role: RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"},
		{Role: RoleHuman, Username: "maizong", Teams: []string{"market-team"}},
		{Role: RoleWorker, Username: "alpha-dev", Team: "alpha-team"},
	}
	for i := range denied {
		if err := az.Authorize(&denied[i], AuthzRequest{Action: ActionUpdate, ResourceKind: "human", ResourceName: "maizong"}); err == nil {
			t.Errorf("%s must be denied updating humans", denied[i].Role)
		}
	}
}

func TestAuthorizer_TeamLeaderOwnTeam(t *testing.T) {
	az := NewAuthorizer()
	caller := &CallerIdentity{Role: RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"}

	allowedCases := []AuthzRequest{
		{Action: ActionGet, ResourceKind: "worker", ResourceName: "alpha-dev", ResourceTeam: "alpha-team"},
		{Action: ActionReady, ResourceKind: "worker", ResourceName: "alpha-lead", ResourceTeam: "alpha-team"},
		{Action: ActionCreate, ResourceKind: "worker", ResourceTeam: "alpha-team"},
		{Action: ActionWake, ResourceKind: "worker", ResourceName: "alpha-dev", ResourceTeam: "alpha-team"},
		{Action: ActionSleep, ResourceKind: "worker", ResourceName: "alpha-dev", ResourceTeam: "alpha-team"},
		{Action: ActionEnsureReady, ResourceKind: "worker", ResourceName: "alpha-dev", ResourceTeam: "alpha-team"},
		{Action: ActionReady, ResourceKind: "worker", ResourceName: "alpha-dev", ResourceTeam: "alpha-team"},
		{Action: ActionList, ResourceKind: "worker"},
		{Action: ActionGet, ResourceKind: "status"},
	}
	for _, req := range allowedCases {
		if err := az.Authorize(caller, req); err != nil {
			t.Errorf("team-leader should be allowed %s %s, got: %v", req.Action, req.ResourceKind, err)
		}
	}
}

func TestAuthorizer_TeamLeaderCrossTeamDenied(t *testing.T) {
	az := NewAuthorizer()
	caller := &CallerIdentity{Role: RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"}

	deniedCases := []AuthzRequest{
		{Action: ActionGet, ResourceKind: "worker", ResourceName: "beta-dev", ResourceTeam: "beta-team"},
		{Action: ActionReady, ResourceKind: "worker", ResourceName: "beta-dev", ResourceTeam: "beta-team"},
		{Action: ActionWake, ResourceKind: "worker", ResourceName: "beta-dev", ResourceTeam: "beta-team"},
		{Action: ActionDelete, ResourceKind: "team", ResourceName: "beta-team"},
		{Action: ActionGateway, ResourceKind: "gateway"},
	}
	for _, req := range deniedCases {
		if err := az.Authorize(caller, req); err == nil {
			t.Errorf("team-leader cross-team %s %s should be denied", req.Action, req.ResourceKind)
		}
	}
}

func TestAuthorizer_WorkerSelfOnly(t *testing.T) {
	az := NewAuthorizer()
	caller := &CallerIdentity{Role: RoleWorker, Username: "alice", WorkerName: "alice"}

	// Self-actions should be allowed
	selfAllowed := []AuthzRequest{
		{Action: ActionReady, ResourceKind: "worker", ResourceName: "alice"},
		{Action: ActionSTS, ResourceKind: "worker", ResourceName: "alice"},
		{Action: ActionGet, ResourceKind: "worker", ResourceName: "alice"},
		{Action: ActionStatus, ResourceKind: "worker", ResourceName: "alice"},
		{Action: ActionGet, ResourceKind: "status"},
	}
	for _, req := range selfAllowed {
		if err := az.Authorize(caller, req); err != nil {
			t.Errorf("worker self %s %s should be allowed, got: %v", req.Action, req.ResourceKind, err)
		}
	}

	// Other worker's resources should be denied
	otherDenied := []AuthzRequest{
		{Action: ActionReady, ResourceKind: "worker", ResourceName: "bob"},
		{Action: ActionSTS, ResourceKind: "worker", ResourceName: "bob"},
		{Action: ActionGet, ResourceKind: "worker", ResourceName: "bob"},
	}
	for _, req := range otherDenied {
		if err := az.Authorize(caller, req); err == nil {
			t.Errorf("worker accessing other %s %s %s should be denied", req.Action, req.ResourceKind, req.ResourceName)
		}
	}
}

func TestAuthorizer_WorkerCannotMutate(t *testing.T) {
	az := NewAuthorizer()
	caller := &CallerIdentity{Role: RoleWorker, Username: "alice", WorkerName: "alice"}

	mutations := []AuthzRequest{
		{Action: ActionCreate, ResourceKind: "worker"},
		{Action: ActionUpdate, ResourceKind: "worker", ResourceName: "alice"},
		{Action: ActionDelete, ResourceKind: "worker", ResourceName: "alice"},
		{Action: ActionWake, ResourceKind: "worker", ResourceName: "alice"},
		{Action: ActionCreate, ResourceKind: "team"},
	}
	for _, req := range mutations {
		if err := az.Authorize(caller, req); err == nil {
			t.Errorf("worker should not be allowed %s %s", req.Action, req.ResourceKind)
		}
	}
}

// TestAuthorizer_WorkerCredentialsRefreshMatrixToken covers the self-scoped
// POST /api/v1/credentials/matrix-token route: a Worker must be able to
// refresh its own Matrix token (the handler uses caller.Username, the route
// never embeds a target ResourceName) and must still be allowed STS, while
// other credential actions are denied. Previously this route landed in the
// "credentials" branch which only permitted ActionSTS, so the worker could
// not recover from a Matrix 401.
func TestAuthorizer_WorkerCredentialsRefreshMatrixToken(t *testing.T) {
	az := NewAuthorizer()
	caller := &CallerIdentity{Role: RoleWorker, Username: "alice", WorkerName: "alice"}

	allowed := []AuthzRequest{
		{Action: ActionRefreshMatrixToken, ResourceKind: "credentials"},
		{Action: ActionSTS, ResourceKind: "credentials"},
	}
	for _, req := range allowed {
		if err := az.Authorize(caller, req); err != nil {
			t.Errorf("worker should be allowed %s credentials, got: %v", req.Action, err)
		}
	}

	// ActionRefreshMatrixToken is credentials-scoped only (it refreshes the
	// caller's own Matrix token via the credentials route); it is not a worker
	// resource action, so a worker-kind request is denied.
	if err := az.Authorize(caller, AuthzRequest{Action: ActionRefreshMatrixToken, ResourceKind: "worker", ResourceName: "alice"}); err == nil {
		t.Error("worker refresh-matrix-token on worker kind should be denied (credentials-scoped action)")
	}

	// Other credential actions remain denied.
	denied := []AuthzRequest{
		{Action: ActionCreate, ResourceKind: "credentials"},
		{Action: ActionGet, ResourceKind: "credentials"},
		{Action: ActionDelete, ResourceKind: "credentials"},
	}
	for _, req := range denied {
		if err := az.Authorize(caller, req); err == nil {
			t.Errorf("worker should be denied %s credentials", req.Action)
		}
	}
}

// TestAuthorizer_TeamLeaderCredentialsRefreshMatrixToken ensures a team leader
// can also self-refresh its Matrix token via the same self-scoped credential route.
func TestAuthorizer_TeamLeaderCredentialsRefreshMatrixToken(t *testing.T) {
	az := NewAuthorizer()
	caller := &CallerIdentity{Role: RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"}

	allowed := []AuthzRequest{
		{Action: ActionRefreshMatrixToken, ResourceKind: "credentials"},
		{Action: ActionSTS, ResourceKind: "credentials"},
	}
	for _, req := range allowed {
		if err := az.Authorize(caller, req); err != nil {
			t.Errorf("team-leader should be allowed %s credentials, got: %v", req.Action, err)
		}
	}

	if err := az.Authorize(caller, AuthzRequest{Action: ActionGet, ResourceKind: "credentials"}); err == nil {
		t.Error("team-leader should be denied get credentials")
	}
}

func TestAuthorizer_NilCaller(t *testing.T) {
	az := NewAuthorizer()
	if err := az.Authorize(nil, AuthzRequest{Action: ActionGet, ResourceKind: "worker"}); err == nil {
		t.Error("nil caller should be denied")
	}
}

func TestAuthorizer_TeamLeaderProjectAccess(t *testing.T) {
	az := NewAuthorizer()
	caller := &CallerIdentity{Role: RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"}

	allowed := []AuthzRequest{
		{Action: ActionList, ResourceKind: "project"},
		{Action: ActionGet, ResourceKind: "project", ResourceName: "p1", ResourceTeam: "alpha-team"},
		{Action: ActionUpdate, ResourceKind: "project", ResourceName: "p1", ResourceTeam: "alpha-team"},
	}
	for _, req := range allowed {
		if err := az.Authorize(caller, req); err != nil {
			t.Errorf("team-leader should be allowed %s %s, got: %v", req.Action, req.ResourceKind, err)
		}
	}

	denied := []AuthzRequest{
		{Action: ActionGet, ResourceKind: "project", ResourceName: "p2", ResourceTeam: "beta-team"},
		{Action: ActionUpdate, ResourceKind: "project", ResourceName: "p2", ResourceTeam: "beta-team"},
		{Action: ActionDelete, ResourceKind: "project", ResourceName: "p1", ResourceTeam: "alpha-team"},
	}
	for _, req := range denied {
		if err := az.Authorize(caller, req); err == nil {
			t.Errorf("team-leader %s %s should be denied", req.Action, req.ResourceKind)
		}
	}
}

func TestAuthorizer_WorkerProjectDenied(t *testing.T) {
	az := NewAuthorizer()
	caller := &CallerIdentity{Role: RoleWorker, Username: "alpha-dev", Team: "alpha-team"}
	for _, req := range []AuthzRequest{
		{Action: ActionList, ResourceKind: "project"},
		{Action: ActionGet, ResourceKind: "project", ResourceName: "p1", ResourceTeam: "alpha-team"},
	} {
		if err := az.Authorize(caller, req); err == nil {
			t.Errorf("worker %s %s should be denied", req.Action, req.ResourceKind)
		}
	}
}
