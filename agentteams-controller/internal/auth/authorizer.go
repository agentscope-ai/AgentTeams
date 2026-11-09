package auth

import "fmt"

// Action represents an API operation.
type Action string

const (
	ActionCreate         Action = "create"
	ActionUpdate         Action = "update"
	ActionDelete         Action = "delete"
	ActionGet            Action = "get"
	ActionList           Action = "list"
	ActionWake           Action = "wake"
	ActionSleep          Action = "sleep"
	ActionEnsureReady    Action = "ensure-ready"
	ActionReady          Action = "ready"
	ActionWorkerApproval Action = "worker-approval"
	// ActionRuntimeConfig guards the qwenpaw running-config proxy WRITES
	// (PUT /workers/{name}/runtime-config and the /loops/custom mutations).
	// L2 humans may write workers in their own teams (W8: the authorizer
	// allows cross-team so the handler can hide with 404 instead of the
	// authorizer answering 403); team leaders are read-only (denied here,
	// #1216 pattern); workers are denied by default.
	ActionRuntimeConfig Action = "runtime-config"
	// ActionWorkerTools guards the qwenpaw tool-settings WRITE
	// (PATCH /workers/{name}/tools/{tool}: enable / async-execution of
	// built-in tools). L2 humans may write workers in their own teams
	// (W8: the authorizer allows cross-team so the handler can hide with
	// 404 instead of the authorizer answering 403); team leaders are
	// read-only (denied here, #1216 pattern); workers are denied by
	// default.
	ActionWorkerTools         Action = "worker-tools"
	ActionSTS                 Action = "sts"
	ActionStatus              Action = "status"
	ActionRefreshMatrixToken  Action = "refresh-matrix-token"
	ActionGateway             Action = "gateway"
	ActionWorkspaceFilesWrite Action = "workspace-files-write"
	// ActionSkillPublish authorizes POST /api/v1/skills (skill upload,
	// #1221). Granted to admin (any scope) and L2 humans (own team,
	// scope=team only); explicitly denied to manager, team leader, and
	// worker. The scope/team refinement happens in the handler (403 for
	// non-admin scope=deployment; 404 anti-probing for cross-team).
	ActionSkillPublish       Action = "skill-publish"
	ActionWorkerSkillPreload Action = "worker-skill-preload"
)

// AuthzRequest describes the resource being accessed.
type AuthzRequest struct {
	Action       Action
	ResourceKind string // "worker" | "team" | "human" | "manager" | "gateway" | "status" | "credentials" | "project" | "project-meta"
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
		// Manager is a full-access SA but does NOT participate in the
		// team-skill paths (#1221): publishing a team skill is admin (any
		// scope) and L2 human (own team) only — never manager, never team
		// leader. The manager agent manages workers, not team assets.
		if caller.Role == RoleManager && req.ResourceKind == "skills" && req.Action == ActionSkillPublish {
			return deny(caller, req)
		}
		// TaskForge runner surface (1.2.7, task 26 §5.4). Admin keeps the
		// full task/checkpoint matrix. Manager is READ-ONLY on
		// task/checkpoint (the manager agent observes the task system, it
		// does not drive it) and has NO runner-module access (design 24:
		// the update trigger is human/admin only). Even for admin the
		// runner module is get/update — delete/recreate is an operator
		// operation outside the API.
		switch req.ResourceKind {
		case "runner-module":
			if caller.Role == RoleManager {
				return deny(caller, req)
			}
			if req.Action == ActionGet || req.Action == ActionUpdate {
				return nil
			}
			return deny(caller, req)
		case "task", "checkpoint":
			if caller.Role == RoleManager && (req.Action != ActionGet && req.Action != ActionList) {
				return deny(caller, req)
			}
			return nil // team scope is filtered by the handler
		}
		return nil // full access

	case RoleTeamLeader:
		// Team leaders read their own team's skill catalog (the assign
		// surface) but never publish: uploads are admin (any scope) and
		// L2 human (own team) only (#1221).
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

	case "project-meta", "task-meta":
		// Conditional commit endpoints (#1402 project, #1471 task): same
		// criteria as the "project" Update — L2 humans commit meta only
		// within their accessibleTeams scope. The middleware cannot resolve
		// project/task -> team (empty ResourceTeam short-circuits
		// requireSameTeam); the handler is the real team boundary.
		if req.Action == ActionGet {
			return nil // record handlers filter scoped reads, as for project Get
		}
		if req.Action == ActionUpdate {
			return a.requireSameTeam(caller, req)
		}
		return deny(caller, req)

	case "team":
		if req.Action == ActionGet || req.Action == ActionList {
			return nil // handler filters by accessibleTeams
		}
		return deny(caller, req)

	case "mcp-server":
		// Deployment MCP catalog (GET /api/v1/mcp-servers): L2 humans read
		// the catalog scoped to their accessibleTeams. Like the worker
		// list, the authorizer does not reject cross-team access — the
		// handler filters by caller.Teams (accessibleTeams), hiding
		// out-of-scope entries rather than probing via 403.
		if req.Action == ActionGet || req.Action == ActionList {
			return nil
		}
		return deny(caller, req)

	case "worker":
		if req.Action == ActionGet || req.Action == ActionList {
			return nil // handler filters by accessibleTeams
		}
		// L2 humans may update workers within their accessibleTeams scope
		// (self-service skill / MCP configuration). The middleware cannot
		// resolve worker -> team, so requireSameTeam short-circuits on an
		// empty ResourceTeam; the UpdateWorker handler enforces the real
		// boundary (team scope + field whitelist), matching the W-PR-2
		// project-write pattern.
		if req.Action == ActionUpdate {
			return a.requireSameTeam(caller, req)
		}
		if req.Action == ActionWorkspaceFilesWrite {
			// W3②-rw: L2 humans may write knowledge base files of workers
			// in their own teams. Like ActionGet/ActionList this action is
			// NOT rejected cross-team at the authorizer level: the
			// middleware resolves the worker's team, and a 403 here would
			// let a scoped caller probe which workers exist in other teams
			// (W8 anti-probing). The handler is the real boundary — it
			// hides cross-team workers as 404 and enforces the per-user
			// workspaceFileAccess flag and the knowledge base allowlist.
			return nil
		}
		if req.Action == ActionWorkerApproval {
			// L2 humans may change the tool-approval level of workers in
			// their own teams. Like ActionGet this action is NOT rejected
			// cross-team at the authorizer level: a 403 here would let a
			// scoped caller probe which workers exist in other teams
			// (W8 anti-probing). All real enforcement happens in the
			// HANDLER, not in this authorizer/middleware:
			// ApprovalHandler.approvalScope performs the worker→team
			// resolution, hides cross-team and standalone workers as
			// 404, and denies team leaders (read-only). The authorizer
			// deliberately allows so the handler can hide with 404
			// instead of the authorizer answering 403.
			return nil
		}
		if req.Action == ActionRuntimeConfig {
			// L2 humans may write the running-config of workers in their
			// own teams (the workbench 5-tab). Same W8 reasoning as
			// ActionWorkerApproval: allowed here, hidden as 404 by the
			// handler when cross-team.
			return nil
		}
		if req.Action == ActionWorkerTools {
			// L2 humans may toggle the built-in tools of workers in their
			// own teams (enable / async-execution). Same W8 reasoning as
			// ActionWorkerApproval: allowed here, hidden as 404 by the
			// handler when cross-team.
			return nil
		}
		if req.Action == ActionWorkerSkillPreload {
			// Per-worker skill preload policy (QwenPaw 2.2.1): L2 humans
			// may toggle preload on workers in their own teams. Like
			// ActionGet this action is NOT rejected cross-team at the
			// authorizer level: a 403 here would let a scoped caller probe
			// which workers exist in other teams (W8 anti-probing). All
			// real enforcement happens in the HANDLER: WorkerSkillsHandler
			// performs the worker→team resolution and hides cross-team and
			// standalone workers as 404. Team leaders and worker-role
			// callers are denied at the authorizer level (default) and
			// never reach the handler — team leaders stay read-only on
			// skill runtime policy.
			return nil
		}
		return deny(caller, req)

	case "skills":
		// Skill catalog (list) + team-skill upload (#1221); the team-scope
		// boundary is enforced in the handler (404 anti-probing, same as
		// for L2 humans).
		if req.Action == ActionList || req.Action == ActionSkillPublish {
			return nil
		}
		return deny(caller, req)

	case "audit":
		// L2 humans may read audit events of their own teams (#1245).
		// The middleware cannot resolve the requested team scope, so the
		// handler is the real boundary: ?team= is mandatory for scoped
		// callers, cross-team reads are hidden as 404 (W8 anti-probing),
		// and unscoped reads are L1 only.
		if req.Action == ActionGet {
			return nil
		}
		return deny(caller, req)

	case "coding-runs":
		// Coding-run transcript visibility (read-only, D12 路 A): L2 humans
		// read the coding-run transcripts of teams in their accessibleTeams.
		// Not rejected cross-team at the authorizer (a 403 would let a scoped
		// caller probe which tasks exist); the handler (visibleTaskPrefixes +
		// W4 404) is the real boundary, hiding out-of-scope runs as 404.
		if req.Action == ActionGet || req.Action == ActionList {
			return nil
		}
		return deny(caller, req)

	case "task":
		// TaskForge task actions (1.2.7, task 26 §5.4): L2 humans read
		// and create/update tasks within their accessibleTeams scope.
		// Delete/restore is denied here (approval flow later). The
		// middleware cannot resolve task -> team, so requireSameTeam
		// short-circuits on an empty ResourceTeam; TaskForge re-checks
		// scope from the signed claims.
		switch req.Action {
		case ActionGet, ActionList:
			return nil // handler filters by accessibleTeams
		case ActionCreate, ActionUpdate:
			return a.requireSameTeam(caller, req)
		default:
			return deny(caller, req)
		}

	case "checkpoint":
		// TaskForge checkpoints (1.2.7): L2 humans read their team's
		// checkpoints only.
		if req.Action == ActionGet || req.Action == ActionList {
			return nil
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

	case "mcp-server":
		// Deployment MCP catalog: team leaders read the catalog scoped to
		// their own team (the handler filters by caller team).
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

	case "skills":
		// Skill catalog read-only: leaders read their own team's catalog as
		// the assign surface. Publishing is admin (any scope) and L2 human
		// (own team) only — leader upload is denied here (the handler
		// re-checks; both layers are pinned by tests).
		if req.Action == ActionList {
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

	case "project-meta", "task-meta":
		// Conditional commit endpoints (#1402 project, #1471 task): same
		// criteria as the "project" Update — own team only. The middleware
		// cannot resolve project/task -> team (empty ResourceTeam
		// short-circuits requireSameTeam); the handler enforces team
		// equality after resolving the record.
		if req.Action == ActionGet || req.Action == ActionUpdate {
			return a.requireSameTeam(caller, req)
		}
		return deny(caller, req)

	case "audit":
		// Team leaders may read audit events of their own team (#1245),
		// same pattern as the worker/team read paths: the handler enforces
		// TeamMatches (?team= mandatory, cross-team hidden as 404, W8
		// anti-probing); unscoped reads are L1 only.
		if req.Action == ActionGet {
			return nil
		}
		return deny(caller, req)

	case "coding-runs":
		// Coding-run transcript visibility (read-only): team leaders read
		// their own team's coding-run transcripts. Not rejected cross-team
		// at the authorizer (W8 anti-probing); the handler (visibleTaskPrefixes
		// + W4 404) hides cross-team runs as 404.
		if req.Action == ActionGet || req.Action == ActionList {
			return nil
		}
		return deny(caller, req)

	case "task":
		// TaskForge task actions (1.2.7, task 26 §5.4): team leaders
		// read/update/create their own team's tasks; delete/restore
		// denied (approval flow later). Mirrors the "project" case.
		switch req.Action {
		case ActionList:
			return nil // handler filters by team
		case ActionGet, ActionUpdate, ActionCreate:
			return a.requireSameTeam(caller, req)
		default:
			return deny(caller, req)
		}

	case "checkpoint":
		// TaskForge checkpoints (1.2.7): team leaders read their own
		// team's checkpoints.
		if req.Action == ActionGet || req.Action == ActionList {
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
	case ActionStatus:
		return a.requireSameTeam(caller, req)
	case ActionReady:
		// A Leader is also a Worker runtime and must report its own startup.
		// This is not authority to change another Worker's lifecycle.
		if req.ResourceName == "" {
			return deny(caller, req)
		}
		return a.requireSelf(caller, req)
	// Deployment resource writes and lifecycle control belong to Admin/Manager.
	// ActionWorkerApproval, ActionRuntimeConfig and ActionWorkerTools
	// deliberately fall through to deny: team leaders are READ-ONLY for
	// worker approval, running-config, and tool settings (mirrors the
	// #1216 approval proxy).
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

	case "project-meta", "task-meta":
		// The two record writes opened to workers (#1402 project meta,
		// #1471 task meta): committing through the controller's strict
		// conditional endpoints (PUT /api/v1/projects/{id}/meta,
		// PUT /api/v1/tasks/{id}/meta). requireSameTeam rejects team-less
		// workers here; team EQUALITY is enforced in the handler — the
		// middleware cannot resolve record -> team, so ResourceTeam is empty
		// and the check would otherwise short-circuit.
		//
		// INVARIANT: "project" must never gain a case in this switch —
		// that would open pause/resume/replan/complete/cancel (registered
		// as ActionUpdate + "project") to every worker. Pinned by
		// TestAuthorizer_ProjectMetaCommit. "task" intentionally gains a
		// case below (1.2.7 TaskForge gateway, task 26 §5.4) for the NEW
		// /api/v1/tasks/{id}/{action} routes only; the legacy project and
		// task-meta endpoints keep their "project"/"task-meta" kinds and
		// are unaffected.
		if req.Action == ActionUpdate {
			return a.requireSameTeam(caller, req)
		}
		return deny(caller, req)

	case "task":
		// TaskForge task actions (1.2.7, task 26 §5.4): workers may
		// GET and UPDATE (claim/submit ride on update). Self-ownership
		// ("仅自己任务") is enforced where the data lives — TaskForge
		// checks the signed claims (ActorID = worker name) against the
		// task's assignee — because the controller has no task -> worker
		// resolution without the DB. requireSameTeam gates team-less
		// workers and (once resolvable) cross-team access. "task"
		// intentionally gains a case here (unlike "project"): it covers
		// the NEW /api/v1/tasks/{id}/{action} routes only; the legacy
		// project endpoints stay on "project"/"task-meta" untouched.
		if req.Action == ActionGet || req.Action == ActionUpdate {
			return a.requireSameTeam(caller, req)
		}
		return deny(caller, req)

	case "checkpoint":
		// TaskForge checkpoints (1.2.7, task 26 §5.4): workers only READ
		// checkpoints (ActionGet through requireSameTeam). Checkpoint
		// records are written by the task system itself, not by workers:
		// a worker must never create/update/delete checkpoint state even
		// for its own task, so everything besides ActionGet — including
		// List and Update — is denied.
		if req.Action == ActionGet {
			return a.requireSameTeam(caller, req)
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
