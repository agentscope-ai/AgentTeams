package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	authpkg "github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/httputil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// k8sUpdateMaxRetries is the max attempts for Get→patch spec→Update against
// optimistic locking conflicts when the controller updates status between Get and Update.
const k8sUpdateMaxRetries = 3

func normalizeLeaderRuntimeForCreate(runtime string) (string, error) {
	value := strings.TrimSpace(runtime)
	if value == "" {
		return backend.RuntimeQwenPaw, nil
	}
	return normalizeExplicitLeaderRuntime(value)
}

func normalizeExplicitLeaderRuntime(runtime string) (string, error) {
	value := strings.TrimSpace(runtime)
	if value != backend.RuntimeQwenPaw && value != backend.RuntimeCopaw {
		return "", fmt.Errorf("leader.runtime must be qwenpaw or copaw")
	}
	return value, nil
}

// ResourceHandler handles declarative CRUD operations on CRs.
//
// Post-refactor contract:
//   - /workers (POST/PUT/DELETE) operate on standalone Worker CRs only.
//     Write attempts that target a name belonging to a Team return 409 and
//     direct the caller to /teams/<name>.
//   - /workers (GET/LIST) return an aggregated view: standalone Worker CRs
//     plus synthetic WorkerResponse entries for every member of every Team,
//     enriched with live backend status so existing consumers (CLI, Manager,
//     Element UI) keep functioning without creating child Worker CRs.
type ResourceHandler struct {
	client    client.Client
	namespace string
	backend   *backend.Registry

	// controllerName is stamped as agentteams.io/controller on every CR this
	// handler creates, overwriting any value supplied by the client. This
	// enforces that HTTP-created resources always belong to the serving
	// controller instance, regardless of what the caller attempts to set.
	// Empty string means no enforcement (embedded mode).
	controllerName string
}

// NewResourceHandler creates a handler. backend may be nil, in which case
// runtime status is omitted from synthetic team member responses.
// controllerName, when non-empty, is force-stamped as agentteams.io/controller
// on every CR this handler creates so HTTP-created resources cannot escape
// the serving controller instance's cache scope.
func NewResourceHandler(c client.Client, namespace string, b *backend.Registry, controllerName string) *ResourceHandler {
	return &ResourceHandler{
		client:         c,
		namespace:      namespace,
		backend:        b,
		controllerName: controllerName,
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

	if team, ok, err := h.findTeamForMember(r.Context(), req.Name); err != nil {
		writeK8sError(w, "create worker", err)
		return
	} else if ok {
		httputil.WriteError(w, http.StatusConflict,
			"worker name is a member of team "+team+"; manage via PUT /api/v1/teams/"+team)
		return
	}

	// containerManaged default is true (controller manages container).
	containerManaged := true
	if req.ContainerManaged != nil {
		containerManaged = *req.ContainerManaged
	}

	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: h.namespace,
		},
		Spec: v1beta1.WorkerSpec{
			Model:              req.Model,
			ModelProvider:      req.ModelProvider,
			WorkerName:         req.WorkerName,
			Runtime:            req.Runtime,
			Image:              req.Image,
			Identity:           req.Identity,
			Soul:               req.Soul,
			Agents:             req.Agents,
			Skills:             req.Skills,
			McpServers:         req.McpServers,
			Package:            req.Package,
			Expose:             req.Expose,
			ChannelPolicy:      req.ChannelPolicy,
			Resources:          req.Resources,
			AgentIdentity:      req.AgentIdentity,
			CredentialBindings: req.CredentialBindings,
			Volumes:            req.Volumes,
			Mounts:             req.Mounts,
			ContainerManaged:   &containerManaged,
			IdleTimeout:        req.IdleTimeout,
			State:              req.State,
		},
	}

	// Team leaders managing team members must use /api/v1/teams — they can no
	// longer back-door-create team workers through the standalone /workers
	// API. (Historical annotation-forcing path removed in the team-refactor.)
	caller := authpkg.CallerFromContext(r.Context())
	if caller != nil && caller.Role == authpkg.RoleTeamLeader {
		httputil.WriteError(w, http.StatusConflict,
			"team leaders must manage members via PUT /api/v1/teams/"+caller.Team)
		return
	}
	if req.Team != "" || req.Role != "" || req.TeamLeader != "" {
		httputil.WriteError(w, http.StatusBadRequest,
			"worker.team / worker.role / worker.teamLeader are reserved for team members; use /api/v1/teams")
		return
	}

	h.stampControllerLabel(&worker.ObjectMeta)

	if err := h.client.Create(r.Context(), worker); err != nil {
		writeK8sError(w, "create worker", err)
		return
	}

	httputil.WriteJSON(w, http.StatusCreated, workerToResponse(r.Context(), h.client, h.namespace, worker))
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
		httputil.WriteJSON(w, http.StatusOK, workerToResponse(r.Context(), h.client, h.namespace, &worker))
		return
	case !apierrors.IsNotFound(err):
		writeK8sError(w, "get worker", err)
		return
	}

	// Fall back to synthesizing a response from the Team CR.
	team, member, ok, terr := h.findTeamMember(r.Context(), name)
	if terr != nil {
		writeK8sError(w, "get worker", terr)
		return
	}
	if !ok {
		httputil.WriteError(w, http.StatusNotFound, "get worker: not found")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, h.teamMemberToResponse(r.Context(), team, member))
}

func (h *ResourceHandler) ListWorkers(w http.ResponseWriter, r *http.Request) {
	teamFilter := r.URL.Query().Get("team")

	workers := make([]WorkerResponse, 0)

	// Standalone workers only when not filtering by team. Decoupled team
	// members have Worker CRs, but are skipped here and emitted from the
	// Team loop below so the list has one authoritative team-member view.
	if teamFilter == "" {
		var list v1beta1.WorkerList
		if err := h.client.List(r.Context(), &list, client.InNamespace(h.namespace)); err != nil {
			writeK8sError(w, "list workers", err)
			return
		}
		for i := range list.Items {
			if h.isTeamMemberWorker(r.Context(), &list.Items[i]) {
				// Worker CR is referenced from a Team's
				// spec.workerMembers (decoupled) — skip to avoid
				// duplicating the synthesized team-member view.
				continue
			}
			workers = append(workers, workerToResponse(r.Context(), h.client, h.namespace, &list.Items[i]))
		}
	}

	var teams v1beta1.TeamList
	teamOpts := []client.ListOption{client.InNamespace(h.namespace)}
	if err := h.client.List(r.Context(), &teams, teamOpts...); err != nil {
		writeK8sError(w, "list workers: list teams", err)
		return
	}
	for i := range teams.Items {
		team := &teams.Items[i]
		if teamFilter != "" && team.Name != teamFilter {
			continue
		}
		for _, ref := range team.Spec.WorkerMembers {
			workers = append(workers, h.teamMemberToResponse(r.Context(), team, ref.Name))
		}
	}

	httputil.WriteJSON(w, http.StatusOK, WorkerListResponse{Workers: workers, Total: len(workers)})
}

// isTeamMemberWorker reports whether a Worker CR is currently a member of
// any Team in the namespace, according to Team.spec.workerMembers. Used by
// ListWorkers to suppress the Worker CR entry when the synthesized team-member
// entry is already produced from the Team CR loop.
func (h *ResourceHandler) isTeamMemberWorker(ctx context.Context, w *v1beta1.Worker) bool {
	if w == nil {
		return false
	}
	return authpkg.LookupWorkerTeam(ctx, h.client, h.namespace, w.Name) != ""
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

	if team, ok, err := h.findTeamForMember(r.Context(), name); err != nil {
		writeK8sError(w, "update worker", err)
		return
	} else if ok {
		httputil.WriteError(w, http.StatusConflict,
			"worker is a member of team "+team+"; update via PUT /api/v1/teams/"+team)
		return
	}

	ctx := r.Context()
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
		if req.AgentIdentity != nil {
			worker.Spec.AgentIdentity = req.AgentIdentity
		}
		if req.CredentialBindings != nil {
			worker.Spec.CredentialBindings = req.CredentialBindings
		}
		if req.Volumes != nil {
			worker.Spec.Volumes = req.Volumes
		}
		if req.Mounts != nil {
			worker.Spec.Mounts = req.Mounts
		}
		if req.ContainerManaged != nil {
			worker.Spec.ContainerManaged = req.ContainerManaged
		}
		if req.IdleTimeout != "" {
			worker.Spec.IdleTimeout = req.IdleTimeout
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

		httputil.WriteJSON(w, http.StatusOK, workerToResponse(r.Context(), h.client, h.namespace, &worker))
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
	if createTeamRequestHasLegacyInlineFields(&req) {
		httputil.WriteError(w, http.StatusBadRequest, "legacy leader/workers fields are not supported; use members")
		return
	}

	if len(req.Members) == 0 {
		httputil.WriteError(w, http.StatusBadRequest, "members is required")
		return
	}
	h.createTeamDecoupled(w, r, &req)
}

func createTeamRequestHasLegacyInlineFields(req *CreateTeamRequest) bool {
	return req.Leader.Name != "" ||
		req.Leader.WorkerName != "" ||
		req.Leader.Model != "" ||
		req.Leader.Runtime != "" ||
		req.Leader.Image != "" ||
		req.Leader.Identity != "" ||
		req.Leader.Soul != "" ||
		req.Leader.Agents != "" ||
		req.Leader.Package != "" ||
		len(req.Leader.McpServers) > 0 ||
		req.Leader.AgentIdentity != nil ||
		len(req.Leader.CredentialBindings) > 0 ||
		req.Leader.ChannelPolicy != nil ||
		req.Leader.Heartbeat != nil ||
		req.Leader.WorkerIdleTimeout != "" ||
		req.Leader.State != nil ||
		len(req.Workers) > 0
}

func (h *ResourceHandler) GetTeam(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "team name is required")
		return
	}

	var team v1beta1.Team
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &team); err != nil {
		writeK8sError(w, "get team", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, teamToResponse(&team))
}

func (h *ResourceHandler) ListTeams(w http.ResponseWriter, r *http.Request) {
	var list v1beta1.TeamList
	if err := h.client.List(r.Context(), &list, client.InNamespace(h.namespace)); err != nil {
		writeK8sError(w, "list teams", err)
		return
	}

	teams := make([]TeamResponse, 0, len(list.Items))
	for i := range list.Items {
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
	if req.Leader != nil && strings.TrimSpace(req.Leader.Runtime) != "" {
		runtime, err := normalizeExplicitLeaderRuntime(req.Leader.Runtime)
		if err != nil {
			httputil.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Leader.Runtime = runtime
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
		if req.HumanMembers != nil {
			team.Spec.HumanMembers = req.HumanMembers
		}
		if req.PeerMentions != nil {
			team.Spec.PeerMentions = req.PeerMentions
		}
		if req.ChannelPolicy != nil {
			team.Spec.ChannelPolicy = req.ChannelPolicy
		}
		if err := h.updateDecoupledTeamMembers(ctx, &team, &req); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, err.Error())
			return
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

func (h *ResourceHandler) updateDecoupledTeamMembers(ctx context.Context, team *v1beta1.Team, req *UpdateTeamRequest) error {
	leaderName, workerNames, err := decoupledTeamMemberIndex(team)
	if err != nil {
		return err
	}
	if req.Leader != nil {
		if err := h.updateWorkerCRForTeamLeader(ctx, leaderName, req.Leader); err != nil {
			return err
		}
		if req.Leader.Heartbeat != nil {
			heartbeat := toHeartbeatSpec(req.Leader.Heartbeat)
			if heartbeat != nil && heartbeat.Enabled {
				team.Spec.HeartbeatEvery = heartbeat.Every
			} else {
				team.Spec.HeartbeatEvery = ""
			}
		}
		if req.Leader.WorkerIdleTimeout != "" {
			for workerName := range workerNames {
				if err := h.updateWorkerCRSpec(ctx, workerName, func(spec *v1beta1.WorkerSpec) {
					spec.IdleTimeout = req.Leader.WorkerIdleTimeout
				}); err != nil {
					return err
				}
			}
		}
	}
	for _, workerReq := range req.Workers {
		if workerReq.Name == "" {
			return fmt.Errorf("workers[].name is required for decoupled team updates")
		}
		if _, ok := workerNames[workerReq.Name]; !ok {
			return fmt.Errorf("worker %q is not a non-leader member of team %q", workerReq.Name, team.Name)
		}
		if err := h.updateWorkerCRForTeamWorker(ctx, workerReq); err != nil {
			return err
		}
	}
	return nil
}

func decoupledTeamMemberIndex(team *v1beta1.Team) (leader string, workers map[string]struct{}, err error) {
	workers = make(map[string]struct{}, len(team.Spec.WorkerMembers))
	for _, ref := range team.Spec.WorkerMembers {
		if ref.Name == "" {
			continue
		}
		if ref.Role == "team_leader" {
			if leader != "" {
				return "", nil, fmt.Errorf("team %q has multiple team leaders in workerMembers", team.Name)
			}
			leader = ref.Name
			continue
		}
		workers[ref.Name] = struct{}{}
	}
	if leader == "" {
		return "", nil, fmt.Errorf("team %q has no team_leader in workerMembers", team.Name)
	}
	return leader, workers, nil
}

func (h *ResourceHandler) updateWorkerCRForTeamLeader(ctx context.Context, name string, req *TeamLeaderRequest) error {
	return h.updateWorkerCRSpec(ctx, name, func(spec *v1beta1.WorkerSpec) {
		if req.WorkerName != "" {
			spec.WorkerName = req.WorkerName
		}
		if req.Model != "" {
			spec.Model = req.Model
		}
		if req.ModelProvider != "" {
			spec.ModelProvider = req.ModelProvider
		}
		if req.Runtime != "" {
			spec.Runtime = req.Runtime
		}
		if req.Image != "" {
			spec.Image = req.Image
		}
		if req.Identity != "" {
			spec.Identity = req.Identity
		}
		if req.Soul != "" {
			spec.Soul = req.Soul
		}
		if req.Agents != "" {
			spec.Agents = req.Agents
		}
		if req.Package != "" {
			spec.Package = req.Package
		}
		if req.McpServers != nil {
			spec.McpServers = req.McpServers
		}
		if req.ChannelPolicy != nil {
			spec.ChannelPolicy = req.ChannelPolicy
		}
		if req.Resources != nil {
			spec.Resources = req.Resources
		}
		if req.AgentIdentity != nil {
			spec.AgentIdentity = req.AgentIdentity
		}
		if req.CredentialBindings != nil {
			spec.CredentialBindings = req.CredentialBindings
		}
		if req.State != nil {
			spec.State = req.State
		}
	})
}

func (h *ResourceHandler) updateWorkerCRForTeamWorker(ctx context.Context, req TeamWorkerRequest) error {
	return h.updateWorkerCRSpec(ctx, req.Name, func(spec *v1beta1.WorkerSpec) {
		if req.WorkerName != "" {
			spec.WorkerName = req.WorkerName
		}
		if req.Model != "" {
			spec.Model = req.Model
		}
		if req.ModelProvider != "" {
			spec.ModelProvider = req.ModelProvider
		}
		if req.Runtime != "" {
			spec.Runtime = req.Runtime
		}
		if req.Image != "" {
			spec.Image = req.Image
		}
		if req.Identity != "" {
			spec.Identity = req.Identity
		}
		if req.Soul != "" {
			spec.Soul = req.Soul
		}
		if req.Agents != "" {
			spec.Agents = req.Agents
		}
		if req.Skills != nil {
			spec.Skills = req.Skills
		}
		if req.McpServers != nil {
			spec.McpServers = req.McpServers
		}
		if req.Package != "" {
			spec.Package = req.Package
		}
		if req.Expose != nil {
			spec.Expose = req.Expose
		}
		if req.ChannelPolicy != nil {
			spec.ChannelPolicy = req.ChannelPolicy
		}
		if req.Resources != nil {
			spec.Resources = req.Resources
		}
		if req.AgentIdentity != nil {
			spec.AgentIdentity = req.AgentIdentity
		}
		if req.CredentialBindings != nil {
			spec.CredentialBindings = req.CredentialBindings
		}
		if req.IdleTimeout != "" {
			spec.IdleTimeout = req.IdleTimeout
		}
		if req.State != nil {
			spec.State = req.State
		}
	})
}

func (h *ResourceHandler) updateWorkerCRSpec(ctx context.Context, name string, mutate func(*v1beta1.WorkerSpec)) error {
	for attempt := 0; attempt < k8sUpdateMaxRetries; attempt++ {
		var worker v1beta1.Worker
		if err := h.client.Get(ctx, client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
			return fmt.Errorf("get worker %q for team member update: %w", name, err)
		}
		mutate(&worker.Spec)
		if err := h.client.Update(ctx, &worker); err != nil {
			if apierrors.IsConflict(err) && attempt+1 < k8sUpdateMaxRetries {
				time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
				continue
			}
			return fmt.Errorf("update worker %q for team member update: %w", name, err)
		}
		return nil
	}
	return fmt.Errorf("update worker %q for team member update exhausted retries", name)
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
			Resources:     req.Resources,
			State:         req.State,
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
		if req.ModelProvider != "" {
			mgr.Spec.ModelProvider = req.ModelProvider
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
		if req.Resources != nil {
			mgr.Spec.Resources = req.Resources
		}
		if req.State != nil {
			mgr.Spec.State = req.State
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

func workerToResponse(ctx context.Context, c client.Reader, namespace string, w *v1beta1.Worker) WorkerResponse {
	resp := WorkerResponse{
		Name:           w.Name,
		Phase:          w.Status.Phase,
		State:          w.Spec.DesiredState(),
		Model:          w.Spec.Model,
		Runtime:        w.Spec.Runtime,
		Image:          w.Spec.Image,
		IdleTimeout:    w.Spec.IdleTimeout,
		ContainerState: w.Status.ContainerState,
		MatrixUserID:   w.Status.MatrixUserID,
		RoomID:         w.Status.RoomID,
		LastActiveAt:   w.Status.LastActiveAt,
		LastHeartbeat:  w.Status.LastHeartbeat,
		Message:        w.Status.Message,
	}
	if w.Spec.ContainerManaged != nil {
		resp.ContainerManaged = *w.Spec.ContainerManaged
	}
	if resp.Phase == "" {
		resp.Phase = "Pending"
	}
	// Team/Role come from the Team CR cache (single source of truth) — never
	// from Worker annotations, which can drift out of sync with TeamReconciler
	// in the decoupled model.
	if teamName, isLeader := authpkg.LookupWorkerTeamRole(ctx, c, namespace, w.Name); teamName != "" {
		resp.Team = teamName
		if isLeader {
			resp.Role = "team_leader"
		} else {
			resp.Role = "team_worker"
		}
	}
	for _, ep := range w.Status.ExposedPorts {
		resp.ExposedPorts = append(resp.ExposedPorts, ExposedPortInfo{Port: ep.Port, Domain: ep.Domain})
	}
	return resp
}

func teamToResponse(t *v1beta1.Team) TeamResponse {
	resp := TeamResponse{
		Name:            t.Name,
		TeamName:        t.Spec.EffectiveTeamName(t.Name),
		Phase:           t.Status.Phase,
		Description:     t.Spec.Description,
		Admin:           t.Spec.Admin,
		HumanMembers:    t.Spec.HumanMembers,
		LeaderHeartbeat: heartbeatSpecFromEvery(t.Spec.HeartbeatEvery),
		TeamRoomID:      t.Status.TeamRoomID,
		LeaderDMRoomID:  t.Status.LeaderDMRoomID,
		LeaderReady:     t.Status.LeaderReady,
		ReadyWorkers:    t.Status.ReadyWorkers,
		TotalWorkers:    t.Status.TotalWorkers,
		Message:         t.Status.Message,
	}
	if resp.Phase == "" {
		resp.Phase = "Pending"
	}

	for _, ref := range t.Spec.WorkerMembers {
		resp.Members = append(resp.Members, TeamMemberRefResponse{Name: ref.Name, Role: ref.Role})
		if ref.Role == "team_leader" {
			resp.LeaderName = ref.Name
		} else {
			resp.WorkerNames = append(resp.WorkerNames, ref.Name)
		}
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

func toHeartbeatSpec(req *TeamLeaderHeartbeatRequest) *v1beta1.TeamLeaderHeartbeatSpec {
	if req == nil {
		return nil
	}

	spec := &v1beta1.TeamLeaderHeartbeatSpec{
		Every: req.Every,
	}
	if req.Enabled != nil {
		spec.Enabled = *req.Enabled
	}
	if !spec.Enabled && spec.Every == "" {
		return nil
	}
	return spec
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
		Name:            h.Name,
		Phase:           h.Status.Phase,
		DisplayName:     h.Spec.DisplayName,
		MatrixUserID:    h.Status.MatrixUserID,
		InitialPassword: h.Status.InitialPassword,
		Rooms:           h.Status.Rooms,
		Message:         h.Status.Message,
	}
	if resp.Phase == "" {
		resp.Phase = "Pending"
	}
	return resp
}

// findTeamForMember reports whether the given worker name is a member
// (leader or worker) of any Team in the current namespace.
func (h *ResourceHandler) findTeamForMember(ctx context.Context, name string) (string, bool, error) {
	team, _, ok, err := h.findTeamMember(ctx, name)
	if err != nil || !ok {
		return "", false, err
	}
	return team.Name, true, nil
}

// findTeamMember does the same as findTeamForMember but also returns the
// resolved Team CR and the member's name (for response synthesis).
func (h *ResourceHandler) findTeamMember(ctx context.Context, name string) (*v1beta1.Team, string, bool, error) {
	var list v1beta1.TeamList
	if err := h.client.List(ctx, &list, client.InNamespace(h.namespace)); err != nil {
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

// teamMemberToResponse synthesizes a team-scoped WorkerResponse from the
// referenced Worker CR.
func (h *ResourceHandler) teamMemberToResponse(ctx context.Context, t *v1beta1.Team, memberName string) WorkerResponse {
	if role, ok := decoupledMemberRole(t, memberName); ok {
		var worker v1beta1.Worker
		if err := h.client.Get(ctx, client.ObjectKey{Name: memberName, Namespace: h.namespace}, &worker); err == nil {
			resp := workerToResponse(ctx, h.client, h.namespace, &worker)
			resp.Team = t.Name
			resp.Role = role
			return resp
		}

		ms := t.Status.MemberByName(memberName)
		resp := WorkerResponse{
			Name:  memberName,
			Team:  t.Name,
			Role:  role,
			Phase: "Pending",
			State: "Running",
		}
		if ms != nil {
			resp.RoomID = ms.RoomID
			resp.MatrixUserID = ms.MatrixUserID
			for _, p := range ms.ExposedPorts {
				resp.ExposedPorts = append(resp.ExposedPorts, ExposedPortInfo{Port: p.Port, Domain: p.Domain})
			}
		}
		return resp
	}
	return WorkerResponse{Name: memberName, Team: t.Name, Phase: "Pending", State: "Running"}
}

func heartbeatSpecFromEvery(every string) *v1beta1.TeamLeaderHeartbeatSpec {
	if every == "" {
		return nil
	}
	return &v1beta1.TeamLeaderHeartbeatSpec{Enabled: true, Every: every}
}

func decoupledMemberRole(t *v1beta1.Team, memberName string) (string, bool) {
	for _, ref := range t.Spec.WorkerMembers {
		if ref.Name != memberName {
			continue
		}
		if ref.Role == "team_leader" {
			return "team_leader", true
		}
		return "worker", true
	}
	return "", false
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
