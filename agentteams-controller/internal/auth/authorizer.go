package auth

import "fmt"

// Action represents an API operation.
type Action string

const (
	ActionCreate             Action = "create"
	ActionUpdate             Action = "update"
	ActionDelete             Action = "delete"
	ActionGet                Action = "get"
	ActionList               Action = "list"
	ActionWake               Action = "wake"
	ActionSleep              Action = "sleep"
	ActionEnsureReady        Action = "ensure-ready"
	ActionReady              Action = "ready"
	ActionSTS                Action = "sts"
	ActionStatus             Action = "status"
	ActionRefreshMatrixToken Action = "refresh-matrix-token"
	ActionGateway            Action = "gateway"
)

// AuthzRequest describes the resource being accessed.
type AuthzRequest struct {
	Action       Action
	ResourceKind string // "worker" | "team" | "human" | "manager" | "gateway" | "status" | "credentials" | "project"
	ResourceName string // target resource name (empty for list operations)
	ResourceTeam string // target resource's team (resolved by handler/middleware)
}

// Authorizer enforces the Role + Team permission matrix.
type Authorizer struct{}

func NewAuthorizer() *Authorizer {
	return &Authorizer{}
}

// Authorize checks whether caller is allowed to perform the requested action.
// Returns nil if allowed, an error describing the denial otherwise.
func (a *Authorizer) Authorize(caller *CallerIdentity, req AuthzRequest) error {
	if caller == nil {
		return fmt.Errorf("authorization denied: no caller identity")
	}

	switch caller.Role {
	case RoleAdmin, RoleManager:
		return nil // full access

	case RoleTeamLeader:
		return a.authorizeTeamLeader(caller, req)

	case RoleHuman:
		return a.authorizeHuman(caller, req)

	case RoleWorker:
		return a.authorizeWorker(caller, req)

	default:
		return fmt.Errorf("authorization denied: unknown role %q", caller.Role)
	}
}

// authorizeHuman is the permission matrix for L2 humans (Human CR
// permissionLevel=2, authenticated by Matrix token). Humans view the teams
// and workers in their accessibleTeams scope; they must NOT manage workers or
// refresh credentials. Since W-PR-2 they may update (pause/resume/replan/
// lifecycle) projects within their accessibleTeams scope, enforced
// code-level by requireSameTeam — a cross-team write is denied even if the
// model ignores any prompt-level guidance. The handler filters list results
// by caller.Teams (accessibleTeams).
func (a *Authorizer) authorizeHuman(caller *CallerIdentity, req AuthzRequest) error {
	switch req.ResourceKind {
	case "status":
		return nil // read-only cluster info

	case "project":
		switch req.Action {
		case ActionList, ActionGet:
			return nil // handler filters by accessibleTeams
		case ActionCreate:
			// W-PR-2: L2 humans may create projects within their accessible
			// teams. The handler resolves the requested team_id and calls
			// checkProjectAccess (the middleware cannot resolve project ->
			// team, so requireSameTeam short-circuits on an empty
			// ResourceTeam; the handler-side check is the real boundary).
			return a.requireSameTeam(caller, req)
		case ActionUpdate:
			// W-PR-2: L2 humans may write (pause/resume/replan/lifecycle)
			// projects within their accessibleTeams scope. requireSameTeam
			// rejects cross-team writes at the code level (the model cannot
			// bypass it), matching the upstream MCP code-level role checks.
			return a.requireSameTeam(caller, req)
		default:
			return deny(caller, req)
		}

	case "team":
		if req.Action == ActionGet || req.Action == ActionList {
			return nil // handler filters by accessibleTeams
		}
		return deny(caller, req)

	case "worker":
		if req.Action == ActionGet || req.Action == ActionList {
			return nil // handler filters by accessibleTeams
		}
		return deny(caller, req)

	default:
		return deny(caller, req)
	}
}

func (a *Authorizer) authorizeTeamLeader(caller *CallerIdentity, req AuthzRequest) error {
	switch req.ResourceKind {
	case "status":
		return nil // read-only cluster info

	case "worker":
		return a.authorizeTeamLeaderWorkerAction(caller, req)

	case "team":
		if req.Action == ActionGet || req.Action == ActionList {
			return nil
		}
		return deny(caller, req)

	case "credentials":
		// Credential endpoints (STS + Matrix token refresh) are always
		// self-scoped: the issued token / refreshed credential is bound to the
		// calling identity, and these routes never embed a target ResourceName
		// (the handler uses caller.Username), so no requireSelf check is needed.
		if req.Action == ActionSTS || req.Action == ActionRefreshMatrixToken {
			return nil
		}
		return deny(caller, req)

	case "project":
		// Projects live under teams/{team}/shared/projects/ (team-scoped) or the
		// global shared/projects/ prefix. Team leaders may list projects, read
		// workflow detail, and (W-PR-2) create/pause/resume/replan projects for
		// their own team only.
		switch req.Action {
		case ActionList:
			return nil // handler filters by team prefix
		case ActionGet, ActionUpdate, ActionCreate:
			return a.requireSameTeam(caller, req)
		default:
			return deny(caller, req)
		}

	default:
		return deny(caller, req)
	}
}

func (a *Authorizer) authorizeTeamLeaderWorkerAction(caller *CallerIdentity, req AuthzRequest) error {
	switch req.Action {
	case ActionGet:
		return a.requireSameTeam(caller, req)
	case ActionList:
		return nil // handler filters by team
	case ActionCreate, ActionUpdate:
		return a.requireSameTeam(caller, req)
	case ActionWake, ActionSleep, ActionEnsureReady, ActionReady, ActionStatus:
		return a.requireSameTeam(caller, req)
	default:
		return deny(caller, req)
	}
}

func (a *Authorizer) authorizeWorker(caller *CallerIdentity, req AuthzRequest) error {
	switch req.ResourceKind {
	case "status":
		return nil

	case "worker":
		return a.authorizeWorkerSelfAction(caller, req)

	case "credentials":
		// Credential endpoints (STS + Matrix token refresh) are always
		// self-scoped: the issued token / refreshed credential is bound to the
		// calling worker, and these routes never embed a target ResourceName
		// (the handler uses caller.Username), so no requireSelf check is needed.
		if req.Action == ActionSTS || req.Action == ActionRefreshMatrixToken {
			return nil
		}
		return deny(caller, req)

	default:
		return deny(caller, req)
	}
}

func (a *Authorizer) authorizeWorkerSelfAction(caller *CallerIdentity, req AuthzRequest) error {
	switch req.Action {
	case ActionReady:
		return a.requireSelf(caller, req)
	case ActionSTS:
		return a.requireSelf(caller, req)
	case ActionGet:
		return a.requireSelf(caller, req)
	case ActionStatus:
		return a.requireSelf(caller, req)
	default:
		return deny(caller, req)
	}
}

func (a *Authorizer) requireSameTeam(caller *CallerIdentity, req AuthzRequest) error {
	if caller.Team == "" && len(caller.Teams) == 0 {
		return fmt.Errorf("authorization denied: team-leader %q has no team", caller.Username)
	}
	if req.ResourceTeam != "" && !caller.TeamMatches(req.ResourceTeam) {
		return fmt.Errorf("authorization denied: team-leader %q cannot access resource in team %s",
			caller.Username, req.ResourceTeam)
	}
	return nil
}

func (a *Authorizer) requireSelf(caller *CallerIdentity, req AuthzRequest) error {
	if req.ResourceName != "" && req.ResourceName != caller.Username {
		return fmt.Errorf("authorization denied: %s %q cannot access resource %q",
			caller.Role, caller.Username, req.ResourceName)
	}
	return nil
}

func deny(caller *CallerIdentity, req AuthzRequest) error {
	return fmt.Errorf("authorization denied: %s %q cannot %s %s",
		caller.Role, caller.Username, req.Action, req.ResourceKind)
}
