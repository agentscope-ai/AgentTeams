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
// GET /api/v1/model-access is read-only: it resolves which model surfaces a
// human may edit for a given worker (the three grant faces + current values).
// The evaluator is a pure function in internal/auth — no catalog validation,
// no Higress lookup, no runtime behavior change. catalogUnavailable stays
// true until A2 wires the model catalog.
//
// Authorization: the route reuses the worker GET permission (the middleware
// cannot resolve ?worker=, so this handler is the real team boundary —
// out-of-scope workers are hidden as 404, W8 anti-probing).

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

// writeWorkerModelAccess resolves the worker's team/role, the owning
// human, and the owner's effective grant for the worker's role.
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

	// The owner is the human whose accessibleTeams contain the worker's
	// team and whose accessibleWorkers is empty (team-wide) or lists the
	// worker. Nobody -> locked view (never a 500). L1/manager callers
	// preview the same resolution.
	var scope *v1beta1.ModelAccessScope
	var tableSpec *v1beta1.ModelAccessTableSpec
	if owner, ok := h.findWorkerOwner(ctx, team, workerName); ok {
		scope = authpkg.EffectiveScope(owner.Spec.ModelAccess, role)
		if scope != nil && scope.Table != "" {
			var table v1beta1.ModelAccessTable
			if err := h.client.Get(ctx, client.ObjectKey{Name: scope.Table, Namespace: h.namespace}, &table); err == nil {
				tableSpec = &table.Spec
			}
			// A dangling table reference (deleted after the grant was
			// written) fails closed: no grant, locked view — a read
			// endpoint never 500s on a missing indirect reference.
		}
	}

	eff := authpkg.Resolve(scope, tableSpec)

	runtime := worker.Spec.Runtime
	if runtime == "" {
		runtime = "openclaw"
	}
	// writeVia is informational (how the surface's value is persisted),
	// not policy. The CRD path exists for every runtime; native write
	// paths exist only on the runtimes that expose one.
	writeVia := []string{"crd"}
	if runtime == "openclaw" || runtime == "hermes" {
		writeVia = []string{"crd", "native"}
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
				WriteVia: writeVia,
			},
			Spawn: modelAccessSurfaceCapabilities{
				CanEdit:  eff.Spawn.CanEdit,
				Inherit:  eff.Spawn.Inherit,
				Reason:   eff.Spawn.Reason,
				WriteVia: writeVia,
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
			WriteVia:      []string{"settings"},
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
// purposes (first by name among all matches), or ok=false. A team of
// "" (standalone worker) is never owned by anyone.
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

// entriesOrEmpty renders a nil entry slice as [] rather than null.
func entriesOrEmpty(e []v1beta1.ModelAccessEntry) []v1beta1.ModelAccessEntry {
	if e == nil {
		return []v1beta1.ModelAccessEntry{}
	}
	return e
}
