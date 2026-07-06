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
	ResourceKind string // "worker" | "team" | "human" | "manager" | "gateway" | "status" | "credentials"
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

	case RoleWorker:
		return a.authorizeWorker(caller, req)

	default:
		return fmt.Errorf("authorization denied: unknown role %q", caller.Role)
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

// requireSameTeam fails closed: a team-leader is only granted access when
// the resource's team was positively resolved AND matches the caller's
// team. An empty/unresolved ResourceTeam is NOT proof the resource is
// team-less (see Middleware.resolveResourceTeam) — it just as easily means
// "the resource exists in another team and we couldn't/didn't look it up",
// so treating it as "allow" would let a team-leader token reach any
// resource whose team could not be determined. The one exception is
// ActionCreate, where there is no existing target resource to resolve a
// team for — the caller can only be creating a resource in its own team,
// so ResourceTeam is expected to be pre-populated by the handler/caller of
// Authorize (never resolved from a lookup) or is inherently self-scoped.
func (a *Authorizer) requireSameTeam(caller *CallerIdentity, req AuthzRequest) error {
	if caller.Team == "" {
		return fmt.Errorf("authorization denied: team-leader %q has no team", caller.Username)
	}
	if req.Action == ActionCreate {
		if req.ResourceTeam != "" && req.ResourceTeam != caller.Team {
			return fmt.Errorf("authorization denied: team-leader %q (team %s) cannot access resource in team %s",
				caller.Username, caller.Team, req.ResourceTeam)
		}
		return nil
	}
	if req.ResourceTeam == "" || req.ResourceTeam != caller.Team {
		return fmt.Errorf("authorization denied: team-leader %q (team %s) cannot access resource in team %q",
			caller.Username, caller.Team, req.ResourceTeam)
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
