package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	audit "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// k8sUpdateMaxRetries is the max attempts for Get→patch spec→Update against
// optimistic locking conflicts when the controller updates status between Get and Update.
const k8sUpdateMaxRetries = 3

// ResourceHandler handles declarative CRUD operations on CRs.
//
// Team CRs reference independently managed Worker CRs. Worker CRUD always
// operates on Worker CRs; Team CRUD only owns membership and coordination.
type ResourceHandler struct {
	client    client.Client
	namespace string
	backend   *backend.Registry

	defaultWorkerRuntime string

	// controllerName is stamped as agentteams.io/controller on every CR this
	// handler creates, overwriting any value supplied by the client. This
	// enforces that HTTP-created resources always belong to the serving
	// controller instance, regardless of what the caller attempts to set.
	// Empty string means no enforcement (embedded mode).
	controllerName string

	// audit records sensitive-surface events (#1220 §8); nil disables the
	// durable layer (tests).
	audit *audit.Client
}

// NewResourceHandler creates a handler. backend may be nil, in which case
// runtime status is omitted from synthetic team member responses.
// controllerName, when non-empty, is force-stamped as agentteams.io/controller
// on every CR this handler creates so HTTP-created resources cannot escape
// the serving controller instance's cache scope.
func NewResourceHandler(c client.Client, namespace string, b *backend.Registry, controllerName string, a *audit.Client) *ResourceHandler {
	return &ResourceHandler{
		client:         c,
		namespace:      namespace,
		backend:        b,
		controllerName: controllerName,
		audit:          a,
	}
}

// stampControllerLabel force-writes the controller ownership label on meta.
// Callers invoke this on every Create path so the HTTP API cannot be used
// to produce CRs that escape the owning controller's cache scope.
func (h *ResourceHandler) stampControllerLabel(meta *metav1.ObjectMeta) {
	if h.controllerName == "" {
		return
	}
	if meta.Labels == nil {
		meta.Labels = map[string]string{}
	}
	meta.Labels[v1beta1.LabelController] = h.controllerName
}

// --- Workers ---

func (h *ResourceHandler) CreateWorker(w http.ResponseWriter, r *http.Request) {
	var req CreateWorkerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "name is required")
		return
	}

	// containerManaged default is true (controller manages container).
	containerManaged := true
	if req.ContainerManaged != nil {
		containerManaged = *req.ContainerManaged
	}
	runtime := backend.ResolveRuntime(req.Runtime, h.defaultWorkerRuntime)

	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: h.namespace,
		},
		Spec: v1beta1.WorkerSpec{
			Model:            req.Model,
			ModelProvider:    req.ModelProvider,
			WorkerName:       req.WorkerName,
			Runtime:          runtime,
			Image:            req.Image,
			Identity:         req.Identity,
			Soul:             req.Soul,
			Agents:           req.Agents,
			Skills:           req.Skills,
			McpServers:       req.McpServers,
			Package:          req.Package,
			Expose:           req.Expose,
			ChannelPolicy:    req.ChannelPolicy,
			Resources:        req.Resources,
			ContainerManaged: &containerManaged,
			State:            req.State,
		},
	}

	// Team leaders cannot create infrastructure resources; Manager/Admin owns
	// Worker creation and Team leaders only coordinate assigned members.
	caller := authpkg.CallerFromContext(r.Context())
	if caller != nil && caller.Role == authpkg.RoleTeamLeader {
		httputil.WriteError(w, http.StatusConflict, "team leaders cannot create Worker resources")
		return
	}
	h.stampControllerLabel(&worker.ObjectMeta)

	if err := h.client.Create(r.Context(), worker); err != nil {
		writeK8sError(w, "create worker", err)
		return
	}

	httputil.WriteJSON(w, http.StatusCreated, workerToResponse(worker))
}

func (h *ResourceHandler) GetWorker(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required")
		return
	}

	var worker v1beta1.Worker
	err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker)
	switch {
	case err == nil:
		resp := workerToResponse(&worker)
		if team, member, ok, terr := findTeamMember(r.Context(), h.client, h.namespace, name); terr != nil {
			writeK8sError(w, "get worker", terr)
			return
		} else if ok {
			applyTeamMember(&resp, team, member)
		}
		// Scoped readers (team leaders or L2 humans) may only fetch workers
		// in the teams they control; standalone workers are hidden. W8: return
		// 404 (not 403) so scoped callers cannot probe worker existence by
		// name — consistent with the project enumeration fix (W4).
		if caller := authpkg.CallerFromContext(r.Context()); caller != nil &&
			(caller.Role == authpkg.RoleTeamLeader || caller.Role == authpkg.RoleHuman) &&
			!caller.TeamMatches(resp.Team) {
			httputil.WriteError(w, http.StatusNotFound, "get worker: not found")
			return
		}
		httputil.WriteJSON(w, http.StatusOK, resp)
		return
	case !apierrors.IsNotFound(err):
		writeK8sError(w, "get worker", err)
		return
	}

	httputil.WriteError(w, http.StatusNotFound, "get worker: not found")
}

func (h *ResourceHandler) ListWorkers(w http.ResponseWriter, r *http.Request) {
	caller := authpkg.CallerFromContext(r.Context())
	teamFilter := r.URL.Query().Get("team")

	workers := make([]WorkerResponse, 0)

	var list v1beta1.WorkerList
	if err := h.client.List(r.Context(), &list, client.InNamespace(h.namespace)); err != nil {
		writeK8sError(w, "list workers", err)
		return
	}
	for i := range list.Items {
		resp := workerToResponse(&list.Items[i])
		if team, member, ok, terr := findTeamMember(r.Context(), h.client, h.namespace, list.Items[i].Name); terr != nil {
			writeK8sError(w, "list workers: lookup team member", terr)
			return
		} else if ok {
			applyTeamMember(&resp, team, member)
		}
		// Scoped readers (team leaders or L2 humans) only see the workers in
		// the teams they control; standalone workers are hidden.
		if caller != nil && (caller.Role == authpkg.RoleTeamLeader || caller.Role == authpkg.RoleHuman) && !caller.TeamMatches(resp.Team) {
			continue
		}
		if teamFilter != "" && resp.Team != teamFilter {
			continue
		}
		workers = append(workers, resp)
	}

	httputil.WriteJSON(w, http.StatusOK, WorkerListResponse{Workers: workers, Total: len(workers)})
}

func (h *ResourceHandler) UpdateWorker(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required")
		return
	}

	var req UpdateWorkerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	ctx := r.Context()
	if caller := authpkg.CallerFromContext(ctx); caller != nil && caller.Role == authpkg.RoleHuman {
		if status, msg := h.checkHumanWorkerUpdate(ctx, caller, name, &req); status != 0 {
			httputil.WriteError(w, status, msg)
			return
		}
	}
	for attempt := 0; attempt < k8sUpdateMaxRetries; attempt++ {
		var worker v1beta1.Worker
		if err := h.client.Get(ctx, client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
			writeK8sError(w, "get worker for update", err)
			return
		}

		if req.Model != "" {
			worker.Spec.Model = req.Model
		}
		if req.ModelProvider != "" {
			worker.Spec.ModelProvider = req.ModelProvider
		}
		if req.WorkerName != "" {
			worker.Spec.WorkerName = req.WorkerName
		}
		if req.Runtime != "" {
			worker.Spec.Runtime = req.Runtime
		}
		if req.Image != "" {
			worker.Spec.Image = req.Image
		}
		if req.Identity != "" {
			worker.Spec.Identity = req.Identity
		}
		if req.Soul != "" {
			worker.Spec.Soul = req.Soul
		}
		if req.Agents != "" {
			worker.Spec.Agents = req.Agents
		}
		if req.Skills != nil {
			worker.Spec.Skills = req.Skills
		}
		if req.RemoteSkills != nil {
			worker.Spec.RemoteSkills = req.RemoteSkills
		}
		if req.McpServers != nil {
			worker.Spec.McpServers = req.McpServers
		}
		if req.Package != "" {
			worker.Spec.Package = req.Package
		}
		if req.Expose != nil {
			worker.Spec.Expose = req.Expose
		}
		if req.ChannelPolicy != nil {
			worker.Spec.ChannelPolicy = req.ChannelPolicy
		}
		if req.Resources != nil {
			worker.Spec.Resources = req.Resources
		}
		if req.ContainerManaged != nil {
			worker.Spec.ContainerManaged = req.ContainerManaged
		}
		if req.State != nil {
			worker.Spec.State = req.State
		}

		if err := h.client.Update(ctx, &worker); err != nil {
			if apierrors.IsConflict(err) && attempt+1 < k8sUpdateMaxRetries {
				time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
				continue
			}
			writeK8sError(w, "update worker", err)
			return
		}

		httputil.WriteJSON(w, http.StatusOK, workerToResponse(&worker))
		return
	}
}

func (h *ResourceHandler) DeleteWorker(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required")
		return
	}

	if team, ok, err := h.findTeamForMember(r.Context(), name); err != nil {
		writeK8sError(w, "delete worker", err)
		return
	} else if ok {
		httputil.WriteError(w, http.StatusConflict,
			"worker is a member of team "+team+"; remove via PUT/DELETE /api/v1/teams/"+team)
		return
	}

	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.namespace},
	}
	if err := h.client.Delete(r.Context(), worker); err != nil {
		writeK8sError(w, "delete worker", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- Teams ---

func (h *ResourceHandler) CreateTeam(w http.ResponseWriter, r *http.Request) {
	var req CreateTeamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len(req.WorkerMembers) == 0 {
		httputil.WriteError(w, http.StatusBadRequest, "workerMembers is required")
		return
	}
	if err := h.validateTeamWorkerMembers(r.Context(), req.Name, req.WorkerMembers); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: h.namespace,
		},
		Spec: v1beta1.TeamSpec{
			Description:    req.Description,
			TeamName:       req.TeamName,
			Admin:          req.Admin,
			HumanMembers:   req.HumanMembers,
			WorkerMembers:  req.WorkerMembers,
			HeartbeatEvery: req.HeartbeatEvery,
			PeerMentions:   req.PeerMentions,
			ChannelPolicy:  req.ChannelPolicy,
		},
	}

	h.stampControllerLabel(&team.ObjectMeta)

	if err := h.client.Create(r.Context(), team); err != nil {
		writeK8sError(w, "create team", err)
		return
	}

	httputil.WriteJSON(w, http.StatusCreated, teamToResponse(team))
}

func (h *ResourceHandler) GetTeam(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "team name is required")
		return
	}

	var team v1beta1.Team
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &team); err != nil {
		if apierrors.IsNotFound(err) {
			httputil.WriteError(w, http.StatusNotFound, "get team: not found")
			return
		}
		writeK8sError(w, "get team", err)
		return
	}

	// Scoped readers (team leaders or L2 humans) may only fetch the teams
	// they control. W8: return 404 (not 403) so scoped callers cannot probe
	// team existence by name — consistent with the project enumeration fix
	// (W4). The team exists but is out of scope, so it is hidden the same
	// way a non-existent team is.
	if caller := authpkg.CallerFromContext(r.Context()); caller != nil &&
		(caller.Role == authpkg.RoleTeamLeader || caller.Role == authpkg.RoleHuman) &&
		!caller.TeamMatches(name) {
		httputil.WriteError(w, http.StatusNotFound, "get team: not found")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, teamToResponse(&team))
}

func (h *ResourceHandler) ListTeams(w http.ResponseWriter, r *http.Request) {
	caller := authpkg.CallerFromContext(r.Context())
	var list v1beta1.TeamList
	if err := h.client.List(r.Context(), &list, client.InNamespace(h.namespace)); err != nil {
		writeK8sError(w, "list teams", err)
		return
	}

	teams := make([]TeamResponse, 0, len(list.Items))
	for i := range list.Items {
		// Scoped readers (team leaders or L2 humans) only see the teams they
		// control; admin/manager see everything.
		if caller != nil && (caller.Role == authpkg.RoleTeamLeader || caller.Role == authpkg.RoleHuman) && !caller.TeamMatches(list.Items[i].Name) {
			continue
		}
		teams = append(teams, teamToResponse(&list.Items[i]))
	}

	httputil.WriteJSON(w, http.StatusOK, TeamListResponse{Teams: teams, Total: len(teams)})
}

func (h *ResourceHandler) UpdateTeam(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "team name is required")
		return
	}

	var req UpdateTeamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.WorkerMembers != nil {
		if err := h.validateTeamWorkerMembers(r.Context(), name, req.WorkerMembers); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	ctx := r.Context()
	for attempt := 0; attempt < k8sUpdateMaxRetries; attempt++ {
		var team v1beta1.Team
		if err := h.client.Get(ctx, client.ObjectKey{Name: name, Namespace: h.namespace}, &team); err != nil {
			writeK8sError(w, "get team for update", err)
			return
		}

		if req.Description != "" {
			team.Spec.Description = req.Description
		}
		if req.TeamName != "" {
			team.Spec.TeamName = req.TeamName
		}
		if req.Admin != nil {
			team.Spec.Admin = req.Admin
		}
		if req.PeerMentions != nil {
			team.Spec.PeerMentions = req.PeerMentions
		}
		if req.ChannelPolicy != nil {
			team.Spec.ChannelPolicy = req.ChannelPolicy
		}
		if req.WorkerMembers != nil {
			team.Spec.WorkerMembers = req.WorkerMembers
		}
		if req.HeartbeatEvery != nil {
			team.Spec.HeartbeatEvery = *req.HeartbeatEvery
		}

		if err := h.client.Update(ctx, &team); err != nil {
			if apierrors.IsConflict(err) && attempt+1 < k8sUpdateMaxRetries {
				time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
				continue
			}
			writeK8sError(w, "update team", err)
			return
		}

		httputil.WriteJSON(w, http.StatusOK, teamToResponse(&team))
		return
	}
}

func (h *ResourceHandler) DeleteTeam(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "team name is required")
		return
	}

	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.namespace},
	}
	if err := h.client.Delete(r.Context(), team); err != nil {
		writeK8sError(w, "delete team", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- Humans ---

func (h *ResourceHandler) CreateHuman(w http.ResponseWriter, r *http.Request) {
	var req CreateHumanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "name is required")
		return
	}

	human := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: h.namespace,
		},
		Spec: v1beta1.HumanSpec{
			DisplayName:       req.DisplayName,
			Email:             req.Email,
			PermissionLevel:   req.PermissionLevel,
			AccessibleTeams:   req.AccessibleTeams,
			AccessibleWorkers: req.AccessibleWorkers,
			Note:              req.Note,
		},
	}

	h.stampControllerLabel(&human.ObjectMeta)

	if err := h.client.Create(r.Context(), human); err != nil {
		writeK8sError(w, "create human", err)
		return
	}

	httputil.WriteJSON(w, http.StatusCreated, humanToResponse(human))
}

func (h *ResourceHandler) GetHuman(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "human name is required")
		return
	}

	var human v1beta1.Human
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &human); err != nil {
		writeK8sError(w, "get human", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, humanToResponse(&human))
}

func (h *ResourceHandler) UpdateHuman(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "human name is required")
		return
	}

	var req UpdateHumanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	ctx := r.Context()
	caller := authpkg.CallerFromContext(ctx)
	for attempt := 0; attempt < k8sUpdateMaxRetries; attempt++ {
		var human v1beta1.Human
		if err := h.client.Get(ctx, client.ObjectKey{Name: name, Namespace: h.namespace}, &human); err != nil {
			writeK8sError(w, "get human for update", err)
			return
		}
		capsBefore := human.Spec.Capabilities

		if req.PermissionLevel != nil && (*req.PermissionLevel < 1 || *req.PermissionLevel > 3) {
			httputil.WriteError(w, http.StatusBadRequest, "permissionLevel must be 1 (admin), 2 (team), or 3 (worker)")
			return
		}
		if err := h.validateHumanReferences(ctx, req.AccessibleTeams, req.AccessibleWorkers); err != nil {
			if errors.Is(err, errDanglingReference) {
				httputil.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			// Backend lookup failure (K8s API timeout, permission, or
			// service error): a server problem, not a client error.
			writeK8sError(w, "validate human references", err)
			return
		}
		if req.Capabilities != nil {
			if err := validateCapabilities(req.Capabilities); err != nil {
				httputil.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
		}

		if req.DisplayName != nil {
			human.Spec.DisplayName = *req.DisplayName
		}
		if req.Email != nil {
			human.Spec.Email = *req.Email
		}
		if req.PermissionLevel != nil {
			human.Spec.PermissionLevel = *req.PermissionLevel
		}
		if req.AccessibleTeams != nil {
			human.Spec.AccessibleTeams = *req.AccessibleTeams
		}
		if req.AccessibleWorkers != nil {
			human.Spec.AccessibleWorkers = *req.AccessibleWorkers
		}
		if req.Capabilities != nil {
			// Store the canonical form (deduped + sorted); validation above
			// already rejected values outside the closed set.
			human.Spec.Capabilities = authpkg.NormalizeCapabilities(*req.Capabilities)
		}
		if req.Note != nil {
			human.Spec.Note = *req.Note
		}

		if err := h.client.Update(ctx, &human); err != nil {
			if apierrors.IsConflict(err) && attempt+1 < k8sUpdateMaxRetries {
				time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
				continue
			}
			writeK8sError(w, "update human", err)
			return
		}

		// Audit wiring point (#1220 §8 event table, line 1): a capability
		// grant/revoke is a sensitive-surface change and is recorded on
		// both audit layers.
		if req.Capabilities != nil {
			h.auditCapabilityChange(ctx, name, caller, capsBefore, human.Spec.Capabilities)
		}

		httputil.WriteJSON(w, http.StatusOK, humanToResponse(&human))
		return
	}
}

// auditCapabilityChange emits one event per changed capability value
// (#1220 §8 event table, line 1: capability grant/revoke). Audit failures
// never break the update: the audit client logs durable-layer errors
// internally.
func (h *ResourceHandler) auditCapabilityChange(ctx context.Context, name string, caller *authpkg.CallerIdentity, before, after []string) {
	if h.audit == nil || caller == nil {
		return
	}
	beforeN := authpkg.NormalizeCapabilities(before)
	afterN := authpkg.NormalizeCapabilities(after)
	for _, cap := range diffStringSlices(beforeN, afterN) {
		h.audit.Record(ctx, audit.Event{
			Who: caller.Username, Role: caller.Role, Target: name,
			Action: "capability_grant", Capability: cap,
			Before: beforeN, After: afterN,
		})
	}
	for _, cap := range diffStringSlices(afterN, beforeN) {
		h.audit.Record(ctx, audit.Event{
			Who: caller.Username, Role: caller.Role, Target: name,
			Action: "capability_revoke", Capability: cap,
			Before: beforeN, After: afterN,
		})
	}
}

// diffStringSlices returns the elements present in b but not in a.
func diffStringSlices(a, b []string) []string {
	set := make(map[string]struct{}, len(a))
	for _, s := range a {
		set[s] = struct{}{}
	}
	var out []string
	for _, s := range b {
		if _, ok := set[s]; !ok {
			out = append(out, s)
		}
	}
	return out
}

// validateCapabilities rejects capability values outside the closed set
// (#1220 §3). The error message lists the valid values so the client can
// self-correct; the set itself is pinned in internal/auth
// (TestValidCapabilitiesMatchesDocumentedValueSet).
func validateCapabilities(caps *[]string) error {
	var invalid []string
	for _, c := range *caps {
		if !authpkg.IsValidCapability(authpkg.Capability(c)) {
			invalid = append(invalid, c)
		}
	}
	if len(invalid) > 0 {
		return fmt.Errorf("capabilities contains unknown value(s) %s; valid values: %s",
			strings.Join(invalid, ", "), strings.Join(authpkg.ValidCapabilityList(), ", "))
	}
	return nil
}

// errDanglingReference marks validation errors where a referenced Team or
// Worker does not exist (a client error, mapped to 400 by the caller).
// Any other error from validateHumanReferences is a backend lookup
// failure (mapped to a server error).
var errDanglingReference = errors.New("dangling reference")

// validateHumanReferences rejects permission grants that point at missing
// Teams or Workers: a dangling reference silently widens nothing but leaves
// the human unable to reach a resource they believe they can. Missing
// references are returned wrapped in errDanglingReference; backend lookup
// failures (List/Get errors other than NotFound) are returned unwrapped so
// the caller can surface them as a server error instead of a 400.
func (h *ResourceHandler) validateHumanReferences(ctx context.Context, teams *[]string, workers *[]string) error {
	if teams != nil {
		var teamList v1beta1.TeamList
		if err := h.client.List(ctx, &teamList, client.InNamespace(h.namespace)); err != nil {
			return fmt.Errorf("list teams: %w", err)
		}
		existing := make(map[string]struct{}, len(teamList.Items))
		for i := range teamList.Items {
			existing[teamList.Items[i].Name] = struct{}{}
		}
		var missing []string
		for _, t := range *teams {
			if t == "" {
				continue
			}
			if _, ok := existing[t]; !ok {
				missing = append(missing, t)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("%w: accessibleTeams references missing teams: %s", errDanglingReference, strings.Join(missing, ", "))
		}
	}
	if workers != nil {
		var missing []string
		for _, wn := range *workers {
			if wn == "" {
				continue
			}
			var worker v1beta1.Worker
			if err := h.client.Get(ctx, client.ObjectKey{Name: wn, Namespace: h.namespace}, &worker); err != nil {
				if apierrors.IsNotFound(err) {
					missing = append(missing, wn)
					continue
				}
				return fmt.Errorf("get worker %s: %w", wn, err)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("%w: accessibleWorkers references missing workers: %s", errDanglingReference, strings.Join(missing, ", "))
		}
	}
	return nil
}

func (h *ResourceHandler) ListHumans(w http.ResponseWriter, r *http.Request) {
	var list v1beta1.HumanList
	if err := h.client.List(r.Context(), &list, client.InNamespace(h.namespace)); err != nil {
		writeK8sError(w, "list humans", err)
		return
	}

	humans := make([]HumanResponse, 0, len(list.Items))
	for i := range list.Items {
		humans = append(humans, humanToResponse(&list.Items[i]))
	}

	httputil.WriteJSON(w, http.StatusOK, HumanListResponse{Humans: humans, Total: len(humans)})
}

func (h *ResourceHandler) DeleteHuman(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "human name is required")
		return
	}

	human := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.namespace},
	}
	if err := h.client.Delete(r.Context(), human); err != nil {
		writeK8sError(w, "delete human", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- Managers ---

func (h *ResourceHandler) CreateManager(w http.ResponseWriter, r *http.Request) {
	var req CreateManagerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Model == "" {
		httputil.WriteError(w, http.StatusBadRequest, "model is required")
		return
	}

	mgr := &v1beta1.Manager{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: h.namespace,
		},
		Spec: v1beta1.ManagerSpec{
			Model:         req.Model,
			ModelProvider: req.ModelProvider,
			Runtime:       req.Runtime,
			Image:         req.Image,
			Soul:          req.Soul,
			Agents:        req.Agents,
			Skills:        req.Skills,
			McpServers:    req.McpServers,
			Package:       req.Package,
			State:         req.State,
			Resources:     req.Resources,
		},
	}
	if req.Config != nil {
		mgr.Spec.Config = *req.Config
	}

	h.stampControllerLabel(&mgr.ObjectMeta)

	if err := h.client.Create(r.Context(), mgr); err != nil {
		writeK8sError(w, "create manager", err)
		return
	}

	httputil.WriteJSON(w, http.StatusCreated, managerToResponse(mgr))
}

func (h *ResourceHandler) GetManager(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "manager name is required")
		return
	}

	var mgr v1beta1.Manager
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &mgr); err != nil {
		writeK8sError(w, "get manager", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, managerToResponse(&mgr))
}

func (h *ResourceHandler) ListManagers(w http.ResponseWriter, r *http.Request) {
	var list v1beta1.ManagerList
	if err := h.client.List(r.Context(), &list, client.InNamespace(h.namespace)); err != nil {
		writeK8sError(w, "list managers", err)
		return
	}

	managers := make([]ManagerResponse, 0, len(list.Items))
	for i := range list.Items {
		managers = append(managers, managerToResponse(&list.Items[i]))
	}

	httputil.WriteJSON(w, http.StatusOK, ManagerListResponse{Managers: managers, Total: len(managers)})
}

func (h *ResourceHandler) UpdateManager(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "manager name is required")
		return
	}

	var req UpdateManagerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	ctx := r.Context()
	for attempt := 0; attempt < k8sUpdateMaxRetries; attempt++ {
		var mgr v1beta1.Manager
		if err := h.client.Get(ctx, client.ObjectKey{Name: name, Namespace: h.namespace}, &mgr); err != nil {
			writeK8sError(w, "get manager for update", err)
			return
		}

		if req.Model != "" {
			mgr.Spec.Model = req.Model
		}
		if req.ModelProvider != nil {
			mgr.Spec.ModelProvider = *req.ModelProvider
		}
		if req.Runtime != "" {
			mgr.Spec.Runtime = req.Runtime
		}
		if req.Image != "" {
			mgr.Spec.Image = req.Image
		}
		if req.Soul != "" {
			mgr.Spec.Soul = req.Soul
		}
		if req.Agents != "" {
			mgr.Spec.Agents = req.Agents
		}
		if req.Skills != nil {
			mgr.Spec.Skills = req.Skills
		}
		if req.McpServers != nil {
			mgr.Spec.McpServers = req.McpServers
		}
		if req.Package != "" {
			mgr.Spec.Package = req.Package
		}
		if req.Config != nil {
			mgr.Spec.Config = *req.Config
		}
		if req.State != nil {
			mgr.Spec.State = req.State
		}
		if req.Resources != nil {
			mgr.Spec.Resources = req.Resources
		}

		if err := h.client.Update(ctx, &mgr); err != nil {
			if apierrors.IsConflict(err) && attempt+1 < k8sUpdateMaxRetries {
				time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
				continue
			}
			writeK8sError(w, "update manager", err)
			return
		}

		httputil.WriteJSON(w, http.StatusOK, managerToResponse(&mgr))
		return
	}
}

func (h *ResourceHandler) DeleteManager(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "manager name is required")
		return
	}

	mgr := &v1beta1.Manager{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.namespace},
	}
	if err := h.client.Delete(r.Context(), mgr); err != nil {
		writeK8sError(w, "delete manager", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- Conversion helpers ---

func workerToResponse(w *v1beta1.Worker) WorkerResponse {
	resp := WorkerResponse{
		Name:             w.Name,
		WorkerName:       w.Spec.WorkerName,
		Phase:            w.Status.Phase,
		State:            w.Spec.DesiredState(),
		Model:            w.Spec.Model,
		Runtime:          w.Spec.Runtime,
		Image:            w.Spec.Image,
		Identity:         w.Spec.Identity,
		Soul:             w.Spec.Soul,
		Agents:           w.Spec.Agents,
		Skills:           w.Spec.Skills,
		McpServers:       w.Spec.McpServers,
		Package:          w.Spec.Package,
		BackendRuntime:   w.Spec.GetBackendRuntime(),
		ContainerManaged: w.Spec.DesiredContainerMan(),
		ChannelPolicy:    w.Spec.ChannelPolicy,
		ContainerState:   w.Status.ContainerState,
		MatrixUserID:     w.Status.MatrixUserID,
		RoomID:           w.Status.RoomID,
		Message:          w.Status.Message,
	}
	if resp.Phase == "" {
		resp.Phase = "Pending"
	}
	for _, ep := range w.Status.ExposedPorts {
		resp.ExposedPorts = append(resp.ExposedPorts, ExposedPortInfo{Port: ep.Port, Domain: ep.Domain})
	}
	return resp
}

func teamToResponse(t *v1beta1.Team) TeamResponse {
	resp := TeamResponse{
		Name:           t.Name,
		TeamName:       t.Spec.EffectiveTeamName(t.Name),
		Phase:          t.Status.Phase,
		Description:    t.Spec.Description,
		Admin:          t.Spec.Admin,
		HumanMembers:   t.Spec.HumanMembers,
		WorkerMembers:  t.Spec.WorkerMembers,
		HeartbeatEvery: t.Spec.HeartbeatEvery,
		TeamRoomID:     t.Status.TeamRoomID,
		LeaderDMRoomID: t.Status.LeaderDMRoomID,
		LeaderReady:    t.Status.LeaderReady,
		ReadyWorkers:   t.Status.ReadyWorkers,
		TotalWorkers:   t.Status.TotalWorkers,
		Message:        t.Status.Message,
	}
	if resp.Phase == "" {
		resp.Phase = "Pending"
	}
	for _, ref := range t.Spec.WorkerMembers {
		if ref.Role == "team_leader" {
			resp.LeaderName = ref.Name
			continue
		}
		resp.WorkerNames = append(resp.WorkerNames, ref.Name)
	}
	for _, ms := range t.Status.Members {
		if len(ms.ExposedPorts) == 0 {
			continue
		}
		if resp.WorkerExposedPorts == nil {
			resp.WorkerExposedPorts = make(map[string][]ExposedPortInfo)
		}
		for _, p := range ms.ExposedPorts {
			resp.WorkerExposedPorts[ms.Name] = append(resp.WorkerExposedPorts[ms.Name], ExposedPortInfo{Port: p.Port, Domain: p.Domain})
		}
	}
	return resp
}

func managerToResponse(m *v1beta1.Manager) ManagerResponse {
	resp := ManagerResponse{
		Name:         m.Name,
		Phase:        m.Status.Phase,
		State:        m.Spec.DesiredState(),
		Model:        m.Spec.Model,
		Runtime:      m.Spec.Runtime,
		Image:        m.Spec.Image,
		MatrixUserID: m.Status.MatrixUserID,
		RoomID:       m.Status.RoomID,
		Version:      m.Status.Version,
		Message:      m.Status.Message,
		WelcomeSent:  m.Status.WelcomeSent,
	}
	if resp.Phase == "" {
		resp.Phase = "Pending"
	}
	return resp
}

func humanToResponse(h *v1beta1.Human) HumanResponse {
	resp := HumanResponse{
		Name:              h.Name,
		Phase:             h.Status.Phase,
		DisplayName:       h.Spec.DisplayName,
		Email:             h.Spec.Email,
		PermissionLevel:   h.Spec.PermissionLevel,
		AccessibleTeams:   h.Spec.AccessibleTeams,
		AccessibleWorkers: h.Spec.AccessibleWorkers,
		Capabilities:      h.Spec.Capabilities,
		Note:              h.Spec.Note,
		MatrixUserID:      h.Status.MatrixUserID,
		InitialPassword:   h.Status.InitialPassword,
		Rooms:             h.Status.Rooms,
		Message:           h.Status.Message,
	}
	if resp.Phase == "" {
		resp.Phase = "Pending"
	}
	return resp
}

// findTeamForMember reports whether the given worker name is a member
// (leader or worker) of any Team in the current namespace.
func (h *ResourceHandler) findTeamForMember(ctx context.Context, name string) (string, bool, error) {
	team, _, ok, err := findTeamMember(ctx, h.client, h.namespace, name)
	if err != nil || !ok {
		return "", false, err
	}
	return team.Name, true, nil
}

// checkHumanWorkerUpdate enforces the L2 human boundary on worker updates.
// The worker must be a member of one of the caller's accessibleTeams —
// standalone workers are hidden from L2 readers (ListWorkers), so they are
// hidden here as well (404 keeps the endpoint probe-resistant). The request
// may only touch the public-catalog skill assignment (skills). remoteSkills
// (arbitrary external registries with credential-bearing source URIs) and
// mcpServers (the gateway consumer key is injected into every entry, so an
// L2-controlled URL is a credential-exfiltration path) require an elevated
// capability pending the L2 permission design; everything else (model,
// image, identity, resources, ...) is the team owner's domain.
// TestL2WorkerUpdateFieldPolicyCoversAllRequestFields pins the policy so no
// field of UpdateWorkerRequest becomes L2-writable by omission.
// Returns (0, "") when the update is allowed.
func (h *ResourceHandler) checkHumanWorkerUpdate(ctx context.Context, caller *authpkg.CallerIdentity, name string, req *UpdateWorkerRequest) (int, string) {
	team, _, ok, err := findTeamMember(ctx, h.client, h.namespace, name)
	if err != nil {
		return http.StatusInternalServerError, "lookup worker team: " + err.Error()
	}
	if !ok {
		return http.StatusNotFound, "worker: not found"
	}
	// Out-of-scope workers are hidden from L2 readers on the read path
	// (GET → 404, LIST → filtered). The update path must not reopen that
	// probe surface: a 403 here would let a scoped human enumerate workers
	// it cannot see and learn which team owns them (W8).
	if !caller.TeamMatches(team.Name) {
		return http.StatusNotFound, "worker: not found"
	}
	var forbidden []string
	if req.WorkerName != "" {
		forbidden = append(forbidden, "workerName")
	}
	if req.Model != "" {
		forbidden = append(forbidden, "model")
	}
	if req.ModelProvider != "" {
		forbidden = append(forbidden, "modelProvider")
	}
	if req.Runtime != "" {
		forbidden = append(forbidden, "runtime")
	}
	if req.Image != "" {
		forbidden = append(forbidden, "image")
	}
	if req.Identity != "" {
		forbidden = append(forbidden, "identity")
	}
	if req.Soul != "" {
		forbidden = append(forbidden, "soul")
	}
	if req.Agents != "" {
		forbidden = append(forbidden, "agents")
	}
	// Credential-bearing surfaces: remoteSkills (registry source URIs may
	// embed tokens) and mcpServers (GenerateMcporterConfig injects the
	// gateway bearer key into every entry, URL used verbatim — an
	// attacker-controlled URL exfiltrates it). Elevated capability pending
	// the L2 permission design.
	if req.RemoteSkills != nil {
		forbidden = append(forbidden, "remoteSkills")
	}
	if req.McpServers != nil {
		forbidden = append(forbidden, "mcpServers")
	}
	if req.Package != "" {
		forbidden = append(forbidden, "package")
	}
	if req.Expose != nil {
		forbidden = append(forbidden, "expose")
	}
	if req.ChannelPolicy != nil {
		forbidden = append(forbidden, "channelPolicy")
	}
	if req.Resources != nil {
		forbidden = append(forbidden, "resources")
	}
	if req.ContainerManaged != nil {
		forbidden = append(forbidden, "containerManaged")
	}
	if req.State != nil {
		forbidden = append(forbidden, "state")
	}
	if len(forbidden) > 0 {
		return http.StatusBadRequest,
			"L2 humans may only update the skills field (public-catalog assignment); remoteSkills and mcpServers require an elevated capability; not allowed: " + strings.Join(forbidden, ", ")
	}
	return 0, ""
}

func (h *ResourceHandler) validateTeamWorkerMembers(ctx context.Context, teamName string, members []v1beta1.TeamWorkerRef) error {
	seen := make(map[string]struct{}, len(members))
	leaders := 0
	for _, ref := range members {
		if ref.Name == "" {
			return fmt.Errorf("workerMembers.name is required")
		}
		if _, ok := seen[ref.Name]; ok {
			return fmt.Errorf("Worker %s is listed more than once", ref.Name)
		}
		seen[ref.Name] = struct{}{}
		switch ref.Role {
		case "team_leader":
			leaders++
		case "worker":
		default:
			return fmt.Errorf("Worker %s has invalid role %q", ref.Name, ref.Role)
		}

		var worker v1beta1.Worker
		if err := h.client.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: h.namespace}, &worker); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("referenced Worker %s does not exist", ref.Name)
			}
			return fmt.Errorf("get referenced Worker %s: %w", ref.Name, err)
		}
	}
	if leaders != 1 {
		return fmt.Errorf("workerMembers must contain exactly one team_leader")
	}

	var teams v1beta1.TeamList
	if err := h.client.List(ctx, &teams, client.InNamespace(h.namespace)); err != nil {
		return fmt.Errorf("list Teams: %w", err)
	}
	for i := range teams.Items {
		team := &teams.Items[i]
		if team.Name == teamName {
			continue
		}
		for _, ref := range team.Spec.WorkerMembers {
			if _, ok := seen[ref.Name]; ok {
				return fmt.Errorf("Worker %s is already a member of Team %s", ref.Name, team.Name)
			}
		}
	}
	return nil
}

// findTeamMember resolves the Team CR and member name used to enrich Worker
// responses from both declarative and lifecycle endpoints.
func findTeamMember(ctx context.Context, c client.Client, namespace, name string) (*v1beta1.Team, string, bool, error) {
	var list v1beta1.TeamList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, "", false, err
	}
	for i := range list.Items {
		t := &list.Items[i]
		for _, ref := range t.Spec.WorkerMembers {
			if ref.Name == name {
				return t, ref.Name, true, nil
			}
		}
	}
	return nil, "", false, nil
}

func applyTeamMember(resp *WorkerResponse, t *v1beta1.Team, memberName string) {
	resp.Team = t.Name
	resp.Role = teamMemberRole(t, memberName)
	if ms := t.Status.MemberByName(memberName); ms != nil {
		if resp.RoomID == "" {
			resp.RoomID = ms.RoomID
		}
		if resp.MatrixUserID == "" {
			resp.MatrixUserID = ms.MatrixUserID
		}
	}
}

func teamMemberRole(t *v1beta1.Team, memberName string) string {
	for _, ref := range t.Spec.WorkerMembers {
		if ref.Name != memberName {
			continue
		}
		if ref.Role == "team_leader" {
			return "team_leader"
		}
		return "worker"
	}
	return "worker"
}

// writeK8sError maps K8s API errors to HTTP status codes.
func writeK8sError(w http.ResponseWriter, op string, err error) {
	switch {
	case apierrors.IsNotFound(err):
		httputil.WriteError(w, http.StatusNotFound, op+": not found")
	case apierrors.IsAlreadyExists(err):
		httputil.WriteError(w, http.StatusConflict, op+": already exists")
	case apierrors.IsConflict(err):
		httputil.WriteError(w, http.StatusConflict, op+": conflict (object modified, retry)")
	default:
		httputil.WriteError(w, http.StatusInternalServerError, op+": "+err.Error())
	}
}
