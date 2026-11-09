package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTeamLeaderResourceWritesRejectedBeforeDispatch(t *testing.T) {
	requests := []AuthzRequest{
		{Action: ActionCreate, ResourceKind: "worker"},
		{Action: ActionUpdate, ResourceKind: "worker", ResourceName: "dev", ResourceTeam: "alpha"},
		{Action: ActionDelete, ResourceKind: "worker", ResourceName: "dev", ResourceTeam: "alpha"},
		{Action: ActionWake, ResourceKind: "worker", ResourceName: "dev", ResourceTeam: "alpha"},
		{Action: ActionSleep, ResourceKind: "worker", ResourceName: "dev", ResourceTeam: "alpha"},
		{Action: ActionEnsureReady, ResourceKind: "worker", ResourceName: "dev", ResourceTeam: "alpha"},
		{Action: ActionReady, ResourceKind: "worker", ResourceName: "dev", ResourceTeam: "alpha"},
		{Action: ActionUpdate, ResourceKind: "team", ResourceName: "alpha"},
		{Action: ActionCreate, ResourceKind: "gateway"},
	}
	for _, operation := range requests {
		t.Run(string(operation.Action)+"/"+operation.ResourceKind, func(t *testing.T) {
			for _, role := range []string{RoleTeamLeader, RoleAdmin, RoleManager} {
				mw := newTestMiddleware(map[string]*CallerIdentity{"token": {Role: role, Username: "lead", Team: "alpha"}})
				called := false
				handler := mw.RequireAuthz(operation.Action, operation.ResourceKind, func(*http.Request) string { return operation.ResourceName })(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					called = true
					w.WriteHeader(http.StatusNoContent)
				}))
				req := httptest.NewRequest(http.MethodPost, "/resource-operation", nil)
				req.Header.Set("Authorization", "Bearer token")
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if role == RoleTeamLeader {
					if rec.Code != http.StatusForbidden || called {
						t.Errorf("leader write dispatched=%v, status=%d; want forbidden before dispatch", called, rec.Code)
					}
				} else if rec.Code != http.StatusNoContent || !called {
					t.Errorf("%s write dispatched=%v, status=%d; want allowed", role, called, rec.Code)
				}
			}
		})
	}
}

func TestTeamLeaderRetainsTaskCoordinationAndSelfReady(t *testing.T) {
	az := NewAuthorizer()
	leader := &CallerIdentity{Role: RoleTeamLeader, Username: "lead", Team: "alpha"}
	for _, req := range []AuthzRequest{
		{Action: ActionGet, ResourceKind: "worker", ResourceName: "dev", ResourceTeam: "alpha"},
		{Action: ActionStatus, ResourceKind: "worker", ResourceName: "dev", ResourceTeam: "alpha"},
		{Action: ActionReady, ResourceKind: "worker", ResourceName: "lead", ResourceTeam: "alpha"},
		{Action: ActionCreate, ResourceKind: "project", ResourceTeam: "alpha"},
		{Action: ActionUpdate, ResourceKind: "project", ResourceTeam: "alpha"},
		{Action: ActionUpdate, ResourceKind: "task-meta", ResourceTeam: "alpha"},
		{Action: ActionSTS, ResourceKind: "credentials"},
		{Action: ActionRefreshMatrixToken, ResourceKind: "credentials"},
	} {
		if err := az.Authorize(leader, req); err != nil {
			t.Errorf("%s %s should remain allowed: %v", req.Action, req.ResourceKind, err)
		}
	}
	if err := az.Authorize(leader, AuthzRequest{Action: ActionReady, ResourceKind: "worker"}); err == nil {
		t.Error("ready without a self target must be denied")
	}
}
