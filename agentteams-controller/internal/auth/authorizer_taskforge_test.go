package auth

import "testing"

// TestAuthorizer_TaskForgeMatrix pins the 1.2.7 TaskForge gateway permission
// matrix (task 26 §5.4) for the "task" / "checkpoint" / "runner-module"
// resource kinds registered by internal/server.
//
// Scope notes (design decisions, see task 26 reply):
//   - admin = full task/checkpoint matrix; manager = READ-ONLY on
//     task/checkpoint and NO runner-module access (design 24: the update
//     trigger is human/admin only). Even for admin the runner module is
//     get/update — delete/recreate is an operator operation outside the API.
//   - human/leader task create/update and worker task get/update go through
//     requireSameTeam; the middleware cannot resolve task -> team, so an
//     EMPTY ResourceTeam short-circuits to allow and TaskForge re-checks
//     scope from the signed claims. The cross-team cases below (ResourceTeam
//     filled) pin the code boundary that becomes load-bearing once team
//     resolution exists.
//   - worker self-ownership ("仅自己任务") is enforced by TaskForge from the
//     signed claims (ActorID = worker name), not here — the controller has
//     no task -> worker resolution without the DB.
func TestAuthorizer_TaskForgeMatrix(t *testing.T) {
	az := NewAuthorizer()

	admin := &CallerIdentity{Role: RoleAdmin, Username: "admin"}
	manager := &CallerIdentity{Role: RoleManager, Username: "manager"}
	human := &CallerIdentity{Role: RoleHuman, Username: "alice", Teams: []string{"sysdev"}}
	leader := &CallerIdentity{Role: RoleTeamLeader, Username: "sysdev-lead", Team: "sysdev"}
	worker := &CallerIdentity{Role: RoleWorker, Username: "w-1", Team: "sysdev"}
	teamlessWorker := &CallerIdentity{Role: RoleWorker, Username: "w-2"} // no team at all

	cases := []struct {
		name   string
		caller *CallerIdentity
		req    AuthzRequest
		allow  bool
	}{
		// --- admin: full task/checkpoint matrix, runner module get/update ---
		{"admin task get", admin, AuthzRequest{Action: ActionGet, ResourceKind: "task", ResourceName: "t-1"}, true},
		{"admin task list", admin, AuthzRequest{Action: ActionList, ResourceKind: "task"}, true},
		{"admin task create", admin, AuthzRequest{Action: ActionCreate, ResourceKind: "task"}, true},
		{"admin task update", admin, AuthzRequest{Action: ActionUpdate, ResourceKind: "task", ResourceName: "t-1"}, true},
		{"admin task delete", admin, AuthzRequest{Action: ActionDelete, ResourceKind: "task", ResourceName: "t-1"}, true},
		{"admin checkpoint get", admin, AuthzRequest{Action: ActionGet, ResourceKind: "checkpoint", ResourceName: "t-1"}, true},
		{"admin checkpoint list", admin, AuthzRequest{Action: ActionList, ResourceKind: "checkpoint", ResourceName: "t-1"}, true},
		{"admin checkpoint update", admin, AuthzRequest{Action: ActionUpdate, ResourceKind: "checkpoint", ResourceName: "t-1"}, true},
		{"admin checkpoint delete", admin, AuthzRequest{Action: ActionDelete, ResourceKind: "checkpoint", ResourceName: "t-1"}, true},
		{"admin runner-module get", admin, AuthzRequest{Action: ActionGet, ResourceKind: "runner-module"}, true},
		{"admin runner-module update", admin, AuthzRequest{Action: ActionUpdate, ResourceKind: "runner-module"}, true},
		{"admin runner-module delete", admin, AuthzRequest{Action: ActionDelete, ResourceKind: "runner-module"}, false},

		// --- manager: read-only on task/checkpoint, NO runner module ---
		{"manager task get", manager, AuthzRequest{Action: ActionGet, ResourceKind: "task", ResourceName: "t-1"}, true},
		{"manager task list", manager, AuthzRequest{Action: ActionList, ResourceKind: "task"}, true},
		{"manager task create", manager, AuthzRequest{Action: ActionCreate, ResourceKind: "task"}, false},
		{"manager task update", manager, AuthzRequest{Action: ActionUpdate, ResourceKind: "task", ResourceName: "t-1"}, false},
		{"manager task delete", manager, AuthzRequest{Action: ActionDelete, ResourceKind: "task", ResourceName: "t-1"}, false},
		{"manager checkpoint get", manager, AuthzRequest{Action: ActionGet, ResourceKind: "checkpoint", ResourceName: "t-1"}, true},
		{"manager checkpoint list", manager, AuthzRequest{Action: ActionList, ResourceKind: "checkpoint", ResourceName: "t-1"}, true},
		{"manager checkpoint update", manager, AuthzRequest{Action: ActionUpdate, ResourceKind: "checkpoint", ResourceName: "t-1"}, false},
		{"manager checkpoint delete", manager, AuthzRequest{Action: ActionDelete, ResourceKind: "checkpoint", ResourceName: "t-1"}, false},
		{"manager runner-module get", manager, AuthzRequest{Action: ActionGet, ResourceKind: "runner-module"}, false},
		{"manager runner-module update", manager, AuthzRequest{Action: ActionUpdate, ResourceKind: "runner-module"}, false},
		{"manager runner-module delete", manager, AuthzRequest{Action: ActionDelete, ResourceKind: "runner-module"}, false},

		// --- L2 human: read + create/update in scope, no delete/restore ---
		{"human task get", human, AuthzRequest{Action: ActionGet, ResourceKind: "task", ResourceName: "t-1"}, true},
		{"human task list", human, AuthzRequest{Action: ActionList, ResourceKind: "task"}, true},
		{"human task create", human, AuthzRequest{Action: ActionCreate, ResourceKind: "task"}, true},
		{"human task update", human, AuthzRequest{Action: ActionUpdate, ResourceKind: "task", ResourceName: "t-1"}, true},
		{"human task create cross-team", human, AuthzRequest{Action: ActionCreate, ResourceKind: "task", ResourceTeam: "other"}, false},
		{"human task update cross-team", human, AuthzRequest{Action: ActionUpdate, ResourceKind: "task", ResourceName: "t-1", ResourceTeam: "other"}, false},
		{"human task delete", human, AuthzRequest{Action: ActionDelete, ResourceKind: "task", ResourceName: "t-1"}, false},
		{"human checkpoint get", human, AuthzRequest{Action: ActionGet, ResourceKind: "checkpoint", ResourceName: "t-1"}, true},
		{"human checkpoint list", human, AuthzRequest{Action: ActionList, ResourceKind: "checkpoint", ResourceName: "t-1"}, true},
		{"human checkpoint update", human, AuthzRequest{Action: ActionUpdate, ResourceKind: "checkpoint", ResourceName: "t-1"}, false},
		{"human checkpoint delete", human, AuthzRequest{Action: ActionDelete, ResourceKind: "checkpoint", ResourceName: "t-1"}, false},
		{"human runner-module get", human, AuthzRequest{Action: ActionGet, ResourceKind: "runner-module"}, false},
		{"human runner-module update", human, AuthzRequest{Action: ActionUpdate, ResourceKind: "runner-module"}, false},

		// --- team leader: own team read/create/update, no delete/restore ---
		{"leader task list", leader, AuthzRequest{Action: ActionList, ResourceKind: "task"}, true},
		{"leader task get", leader, AuthzRequest{Action: ActionGet, ResourceKind: "task", ResourceName: "t-1"}, true},
		{"leader task get cross-team", leader, AuthzRequest{Action: ActionGet, ResourceKind: "task", ResourceName: "t-1", ResourceTeam: "other"}, false},
		{"leader task update", leader, AuthzRequest{Action: ActionUpdate, ResourceKind: "task", ResourceName: "t-1"}, true},
		{"leader task create", leader, AuthzRequest{Action: ActionCreate, ResourceKind: "task"}, true},
		{"leader task create cross-team", leader, AuthzRequest{Action: ActionCreate, ResourceKind: "task", ResourceTeam: "other"}, false},
		{"leader task delete", leader, AuthzRequest{Action: ActionDelete, ResourceKind: "task", ResourceName: "t-1"}, false},
		{"leader checkpoint get", leader, AuthzRequest{Action: ActionGet, ResourceKind: "checkpoint", ResourceName: "t-1"}, true},
		{"leader checkpoint list", leader, AuthzRequest{Action: ActionList, ResourceKind: "checkpoint", ResourceName: "t-1"}, true},
		{"leader checkpoint update", leader, AuthzRequest{Action: ActionUpdate, ResourceKind: "checkpoint", ResourceName: "t-1"}, false},
		{"leader runner-module get", leader, AuthzRequest{Action: ActionGet, ResourceKind: "runner-module"}, false},
		{"leader runner-module update", leader, AuthzRequest{Action: ActionUpdate, ResourceKind: "runner-module"}, false},

		// --- worker: get/update only (claim/submit ride on update), team-gated ---
		{"worker task get", worker, AuthzRequest{Action: ActionGet, ResourceKind: "task", ResourceName: "t-1"}, true},
		{"worker task update", worker, AuthzRequest{Action: ActionUpdate, ResourceKind: "task", ResourceName: "t-1"}, true},
		{"worker task get cross-team", worker, AuthzRequest{Action: ActionGet, ResourceKind: "task", ResourceName: "t-1", ResourceTeam: "other"}, false},
		{"worker task update cross-team", worker, AuthzRequest{Action: ActionUpdate, ResourceKind: "task", ResourceName: "t-1", ResourceTeam: "other"}, false},
		{"worker task list", worker, AuthzRequest{Action: ActionList, ResourceKind: "task"}, false},
		{"worker task create", worker, AuthzRequest{Action: ActionCreate, ResourceKind: "task"}, false},
		{"worker task delete", worker, AuthzRequest{Action: ActionDelete, ResourceKind: "task", ResourceName: "t-1"}, false},
		{"worker checkpoint get", worker, AuthzRequest{Action: ActionGet, ResourceKind: "checkpoint", ResourceName: "t-1"}, true},
		{"worker checkpoint list", worker, AuthzRequest{Action: ActionList, ResourceKind: "checkpoint", ResourceName: "t-1"}, false},
		{"worker checkpoint update", worker, AuthzRequest{Action: ActionUpdate, ResourceKind: "checkpoint", ResourceName: "t-1"}, false},
		{"worker runner-module get", worker, AuthzRequest{Action: ActionGet, ResourceKind: "runner-module"}, false},
		{"worker runner-module update", worker, AuthzRequest{Action: ActionUpdate, ResourceKind: "runner-module"}, false},

		// --- team-less worker: requireSameTeam denies everything ---
		{"teamless worker task get", teamlessWorker, AuthzRequest{Action: ActionGet, ResourceKind: "task", ResourceName: "t-1"}, false},
		{"teamless worker task update", teamlessWorker, AuthzRequest{Action: ActionUpdate, ResourceKind: "task", ResourceName: "t-1"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := az.Authorize(tc.caller, tc.req)
			if tc.allow && err != nil {
				t.Fatalf("allowed: got denial: %v", err)
			}
			if !tc.allow && err == nil {
				t.Fatalf("denied: got nil error")
			}
		})
	}
}
