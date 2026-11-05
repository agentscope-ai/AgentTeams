package server

import (
	"context"
	"net/http"
	"slices"
	"sort"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// --- Per-human model-access preview (PR-A1b §4) ---
//
// GET /api/v1/model-access is read-only: it resolves which model surfaces the
// CALLER may edit for a given worker (the three grant faces + current values).
// The evaluator is a pure function in internal/auth — no catalog validation,
// no Higress lookup, no runtime behavior change. catalogUnavailable stays
// true until A2 wires the model catalog.
//
// Caller perspective (ported from the #1514 caller-view fix): the preview
// always describes the caller's OWN write position. A human caller sees only
// their own grant table — never the worker owner's — so two humans in the
// same team with different grants see two different previews. Admin/manager
// keep the resource (owner) view, semantics unchanged. A team leader sees no
// grant table at all (leaders are not grant-bound); their canEdit mirrors the
// existing leader worker-write rule (own team). canEdit/writeVia follow this
// release's real write entries (the path a write actually takes), not a guess.
//
// Authorization: the route reuses the worker GET permission (the middleware
// cannot resolve ?worker=, so this handler is the real team boundary —
// out-of-scope workers are hidden as 404, W8 anti-probing).

// reasonNoWriteAccess is reported on surfaces the caller cannot write through
// this release's write entries at all: the write entry's own team-scope
// decision denies the caller (L3 worker-scoped humans, team leaders outside
// their team, worker callers). Same snake_case style as the authpkg Reason*
// codes.
const reasonNoWriteAccess = "no_write_access"

// Response shapes.

// modelAccessUnrestrictedResponse is the no-parameter answer for L1 callers.
type modelAccessUnrestrictedResponse struct {
	Unrestricted bool `json:"unrestricted"`
}

// modelAccessScopesResponse is the no-parameter answer for humans: the raw
// spec.modelAccess grant table (empty array when ungranted).
type modelAccessScopesResponse struct {
	Scopes []v1beta1.ModelAccessScope `json:"scopes"`
}

// modelAccessLockedResponse is the no-parameter answer for SA callers
// (team leader / worker): there is no human grant to evaluate.
type modelAccessLockedResponse struct {
	Locked       bool     `json:"locked"`
	Capabilities struct{} `json:"capabilities"`
}

type modelAccessWorkerResponse struct {
	Worker             string                  `json:"worker"`
	Role               string                  `json:"role"`
	Team               string                  `json:"team"`
	Runtime            string                  `json:"runtime"`
	Locked             bool                    `json:"locked"`
	Capabilities       modelAccessCapabilities `json:"capabilities"`
	Surfaces           modelAccessSurfaces     `json:"surfaces"`
	Current            modelAccessCurrent      `json:"current"`
	CatalogUnavailable bool                    `json:"catalogUnavailable"`
}

type modelAccessCapabilities struct {
	Main      modelAccessSurfaceCapabilities        `json:"main"`
	Spawn     modelAccessSurfaceCapabilities        `json:"spawn"`
	CodingCli map[string]modelAccessCLICapabilities `json:"codingCli"`
}

type modelAccessSurfaceCapabilities struct {
	CanEdit  bool     `json:"canEdit"`
	Inherit  bool     `json:"inherit"`
	Reason   string   `json:"reason,omitempty"`
	WriteVia []string `json:"writeVia"`
}

type modelAccessCLICapabilities struct {
	CanEdit       bool              `json:"canEdit"`
	Inherit       bool              `json:"inherit,omitempty"`
	Reason        string            `json:"reason,omitempty"`
	WriteVia      []string          `json:"writeVia"`
	InheritedFrom *authpkg.ModelRef `json:"inheritedFrom,omitempty"`
}

type modelAccessSurfaces struct {
	Main      modelAccessSurfaceView            `json:"main"`
	Spawn     modelAccessSurfaceView            `json:"spawn"`
	CodingCli map[string]modelAccessSurfaceView `json:"codingCli"`
}

// modelAccessSurfaceView carries the selectable rows of one surface.
// Entries has no omitempty: an empty grant still renders "entries": [].
type modelAccessSurfaceView struct {
	Entries []v1beta1.ModelAccessEntry `json:"entries"`
	Inherit bool                       `json:"inherit,omitempty"`
}

type modelAccessCurrent struct {
	Model         string `json:"model"`
	ModelProvider string `json:"modelProvider"`
	SubagentModel string `json:"subagentModel"`
}

// GetModelAccess serves GET /api/v1/model-access.
func (h *ResourceHandler) GetModelAccess(w http.ResponseWriter, r *http.Request) {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil {
		httputil.WriteError(w, http.StatusForbidden, "caller identity required")
		return
	}
	ctx := r.Context()

	workerName := r.URL.Query().Get("worker")
	if workerName == "" {
		h.writeNoParamModelAccess(ctx, w, caller)
		return
	}
	teamParam := r.URL.Query().Get("team")
	h.writeWorkerModelAccess(ctx, w, caller, workerName, teamParam)
}

// writeNoParamModelAccess answers the parameterless endpoint per caller
// role: L1 unrestricted, humans their raw grant scopes, SA callers a locked
// stub (their identities carry no human grant table).
func (h *ResourceHandler) writeNoParamModelAccess(ctx context.Context, w http.ResponseWriter, caller *authpkg.CallerIdentity) {
	switch caller.Role {
	case authpkg.RoleAdmin, authpkg.RoleManager:
		httputil.WriteJSON(w, http.StatusOK, modelAccessUnrestrictedResponse{Unrestricted: true})
	case authpkg.RoleHuman:
		scopes := []v1beta1.ModelAccessScope{}
		// Fail closed: a Human CR deleted mid-session simply carries no
		// grants.
		var human v1beta1.Human
		if err := h.client.Get(ctx, client.ObjectKey{Name: caller.Username, Namespace: h.namespace}, &human); err == nil &&
			human.Spec.ModelAccess != nil && len(human.Spec.ModelAccess.Scopes) > 0 {
			scopes = human.Spec.ModelAccess.Scopes
		}
		httputil.WriteJSON(w, http.StatusOK, modelAccessScopesResponse{Scopes: scopes})
	default:
		httputil.WriteJSON(w, http.StatusOK, modelAccessLockedResponse{Locked: true})
	}
}

// writeWorkerModelAccess resolves the worker's team/role and the effective
// grant FROM THE CALLER'S PERSPECTIVE: admin/manager preview the worker
// owner's grant (the resource view), a human previews their OWN grant table,
// and a team leader / worker previews no grant (they carry no human grant
// table). canEdit additionally reflects whether this caller can write the
// surface through this release's real write entries.
func (h *ResourceHandler) writeWorkerModelAccess(ctx context.Context, w http.ResponseWriter, caller *authpkg.CallerIdentity, workerName, teamParam string) {
	var worker v1beta1.Worker
	if err := h.client.Get(ctx, client.ObjectKey{Name: workerName, Namespace: h.namespace}, &worker); err != nil {
		if apierrors.IsNotFound(err) {
			httputil.WriteError(w, http.StatusNotFound, "worker not found")
			return
		}
		writeK8sError(w, "get worker", err)
		return
	}

	var teams v1beta1.TeamList
	if err := h.client.List(ctx, &teams, client.InNamespace(h.namespace)); err != nil {
		writeK8sError(w, "list teams", err)
		return
	}

	// Deterministic (team-name sorted) list of the teams that carry the
	// worker as a member.
	type teamMembership struct {
		team string
		role string
	}
	var memberships []teamMembership
	for i := range teams.Items {
		for _, m := range teams.Items[i].Spec.WorkerMembers {
			if m.Name == workerName {
				memberships = append(memberships, teamMembership{team: teams.Items[i].Name, role: m.Role})
			}
		}
	}
	sort.Slice(memberships, func(i, j int) bool { return memberships[i].team < memberships[j].team })

	role := "worker"
	team := ""
	switch {
	case teamParam != "":
		for _, m := range memberships {
			if m.team == teamParam {
				team = m.team
				role = m.role
				break
			}
		}
		if team == "" {
			// Not a member of the pinned team: 404 for every caller.
			httputil.WriteError(w, http.StatusNotFound, "worker not found in team")
			return
		}
	case len(memberships) > 0:
		team = memberships[0].team
		role = memberships[0].role
	}

	// W8 anti-probing: a human sees only workers in their accessibleTeams
	// (or explicitly assigned to them). 404, never 403.
	if caller.Role == authpkg.RoleHuman && !caller.WorkerReadable(team, workerName) {
		httputil.WriteError(w, http.StatusNotFound, "worker not found")
		return
	}

	// Caller-perspective grant resolution. A name-based owner lookup
	// (findWorkerOwner) would render the worker owner's grant to every
	// caller — a human teammate would preview someone else's rows.
	//   - admin / manager: the resource view — the owning human's grant
	//     (nobody -> locked view, never a 500);
	//   - human: the caller's OWN Human CR grant table; a CR deleted
	//     mid-session carries no grants (fail closed, never a 500);
	//   - team leader / worker: no grant table at all — they are not
	//     grant-bound, so they never see anyone's grant rows.
	// writeAccess is the write entry's own authorization decision for this
	// caller (the same team-scope predicate the mutation paths apply), so
	// the preview never promises a write the write entry would reject.
	var (
		scope       *v1beta1.ModelAccessScope
		tableSpec   *v1beta1.ModelAccessTableSpec
		writeAccess bool
	)
	switch caller.Role {
	case authpkg.RoleAdmin, authpkg.RoleManager:
		writeAccess = true
		if owner, ok := h.findWorkerOwner(ctx, team, workerName); ok {
			scope, tableSpec = h.resolveHumanScope(ctx, owner, role)
		}
	case authpkg.RoleHuman:
		writeAccess = caller.TeamMatches(team)
		human, err := h.getCallerHuman(ctx, caller.Username)
		if err != nil {
			writeK8sError(w, "get human", err)
			return
		}
		if human != nil {
			scope, tableSpec = h.resolveHumanScope(ctx, human, role)
		}
	case authpkg.RoleTeamLeader:
		// The leader's existing worker-write rule (own team);
		// findWorkerOwner is deliberately not reused.
		writeAccess = caller.TeamMatches(team)
	}

	eff := authpkg.Resolve(scope, tableSpec)

	// Caller-view gate: a surface the write entry does not authorize for
	// this caller is not editable, whatever the grant says. The grant view
	// itself is still rendered — for a human it is their OWN table, so
	// rendering it leaks nothing.
	if !writeAccess {
		eff.Main.CanEdit = false
		eff.Main.Reason = reasonNoWriteAccess
		eff.Spawn.CanEdit = false
		eff.Spawn.Reason = reasonNoWriteAccess
		for cli, acc := range eff.CLI {
			acc.CanEdit = false
			acc.Reason = reasonNoWriteAccess
			eff.CLI[cli] = acc
		}
	} else if caller.Role == authpkg.RoleTeamLeader {
		// The leader is not grant-bound: the own-team write rule alone
		// opens every surface this API can write (BYOK qoder stays closed
		// — it has no API write path for anyone).
		eff.Main.CanEdit = true
		eff.Main.Reason = ""
		eff.Spawn.CanEdit = true
		eff.Spawn.Reason = ""
		for cli, acc := range eff.CLI {
			if acc.Reason == authpkg.ReasonBYOKUnsupported {
				continue
			}
			acc.CanEdit = true
			acc.Reason = ""
			eff.CLI[cli] = acc
		}
	}
	eff.Locked = !eff.Main.CanEdit && !eff.Spawn.CanEdit
	for _, acc := range eff.CLI {
		if acc.CanEdit {
			eff.Locked = false
			break
		}
	}

	runtime := worker.Spec.Runtime
	if runtime == "" {
		runtime = "openclaw"
	}
	// writeVia records the identity that executes the write through this
	// release's REAL write entries — not a guess:
	//   - the model fields (main/spawn) are L1-only: the scoped-writer
	//     field policy rejects model/modelProvider/subagentModel updates
	//     from L2 humans and team leaders, so a non-L1 caller's model
	//     write routes through an L1 identity;
	//   - the coding-CLI settings write accepts L1 and the L2 human of the
	//     worker's team; team leaders are read-only there, so a leader's
	//     CLI write also routes through an L1.
	// "direct" = the caller's own API write; "admin" = routed through an
	// L1. "leader" is not a working write path in this release (reserved
	// for a leader model-write path, which lands with the grant-gated
	// human model writes in #1514).
	modelVia := []string{"admin"}
	cliVia := []string{"admin"}
	switch caller.Role {
	case authpkg.RoleAdmin, authpkg.RoleManager:
		modelVia = []string{"direct"}
		cliVia = []string{"direct"}
	case authpkg.RoleHuman:
		if writeAccess {
			cliVia = []string{"direct"}
		}
	}

	resp := modelAccessWorkerResponse{
		Worker:  workerName,
		Role:    role,
		Team:    team,
		Runtime: runtime,
		Locked:  eff.Locked,
		Capabilities: modelAccessCapabilities{
			Main: modelAccessSurfaceCapabilities{
				CanEdit:  eff.Main.CanEdit,
				Inherit:  eff.Main.Inherit,
				Reason:   eff.Main.Reason,
				WriteVia: modelVia,
			},
			Spawn: modelAccessSurfaceCapabilities{
				CanEdit:  eff.Spawn.CanEdit,
				Inherit:  eff.Spawn.Inherit,
				Reason:   eff.Spawn.Reason,
				WriteVia: modelVia,
			},
			CodingCli: make(map[string]modelAccessCLICapabilities, len(eff.CLI)),
		},
		Surfaces: modelAccessSurfaces{
			Main:      modelAccessSurfaceView{Entries: entriesOrEmpty(eff.Main.Entries)},
			Spawn:     modelAccessSurfaceView{Entries: entriesOrEmpty(eff.Spawn.Entries), Inherit: eff.Spawn.Inherit},
			CodingCli: map[string]modelAccessSurfaceView{},
		},
		Current: modelAccessCurrent{
			Model:         worker.Spec.Model,
			ModelProvider: worker.Spec.ModelProvider,
			SubagentModel: worker.Spec.SubagentModel,
		},
		CatalogUnavailable: true,
	}
	for cli, acc := range eff.CLI {
		resp.Capabilities.CodingCli[cli] = modelAccessCLICapabilities{
			CanEdit:       acc.CanEdit,
			Inherit:       acc.Inherit,
			Reason:        acc.Reason,
			WriteVia:      cliVia,
			InheritedFrom: acc.InheritedFrom,
		}
		// surfaces.codingCli lists only the CLIs the grant actually
		// covers (rows or an inherit flag) — the selectable rows the UI
		// offers.
		if len(acc.Entries) > 0 || acc.Inherit {
			resp.Surfaces.CodingCli[cli] = modelAccessSurfaceView{
				Entries: entriesOrEmpty(acc.Entries),
				Inherit: acc.Inherit,
			}
		}
	}
	httputil.WriteJSON(w, http.StatusOK, resp)
}

// findWorkerOwner returns the human that owns the worker for grant
// purposes (first by name among all matches), or ok=false. It serves the
// admin/manager resource view only — human callers resolve their OWN grant
// (getCallerHuman + resolveHumanScope), and team leaders are not
// grant-bound at all. A team of "" (standalone worker) is never owned by
// anyone.
func (h *ResourceHandler) findWorkerOwner(ctx context.Context, team, workerName string) (*v1beta1.Human, bool) {
	var humans v1beta1.HumanList
	// A list failure degenerates to "no owner" -> locked view: a read
	// endpoint never 500s on this path.
	if err := h.client.List(ctx, &humans, client.InNamespace(h.namespace)); err != nil {
		return nil, false
	}
	sort.Slice(humans.Items, func(i, j int) bool { return humans.Items[i].Name < humans.Items[j].Name })
	for i := range humans.Items {
		hu := &humans.Items[i]
		if !slices.Contains(hu.Spec.AccessibleTeams, team) {
			continue
		}
		if len(hu.Spec.AccessibleWorkers) > 0 && !slices.Contains(hu.Spec.AccessibleWorkers, workerName) {
			continue
		}
		return hu, true
	}
	return nil, false
}

// getCallerHuman loads the caller's own Human CR. A deleted CR degrades to
// (nil, nil) — it simply carries no grants (fail closed, never a 500). Any
// other read error is returned so the handler maps it to 500.
func (h *ResourceHandler) getCallerHuman(ctx context.Context, username string) (*v1beta1.Human, error) {
	var human v1beta1.Human
	if err := h.client.Get(ctx, client.ObjectKey{Name: username, Namespace: h.namespace}, &human); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &human, nil
}

// resolveHumanScope selects the role's scope from a human's grant table and
// follows the table reference. A dangling table reference (deleted after the
// grant was written) fails closed: no grant, locked view — a read endpoint
// never 500s on a missing indirect reference.
func (h *ResourceHandler) resolveHumanScope(ctx context.Context, human *v1beta1.Human, role string) (*v1beta1.ModelAccessScope, *v1beta1.ModelAccessTableSpec) {
	scope := authpkg.EffectiveScope(human.Spec.ModelAccess, role)
	if scope == nil || scope.Table == "" {
		return scope, nil
	}
	var table v1beta1.ModelAccessTable
	if err := h.client.Get(ctx, client.ObjectKey{Name: scope.Table, Namespace: h.namespace}, &table); err != nil {
		return scope, nil
	}
	return scope, &table.Spec
}

// entriesOrEmpty renders a nil entry slice as [] rather than null.
func entriesOrEmpty(e []v1beta1.ModelAccessEntry) []v1beta1.ModelAccessEntry {
	if e == nil {
		return []v1beta1.ModelAccessEntry{}
	}
	return e
}
