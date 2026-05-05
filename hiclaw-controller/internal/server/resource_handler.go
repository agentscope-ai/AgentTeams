package server

import (
	"context"
	"encoding/json"
	"net/http"
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

	// controllerName is stamped as hiclaw.io/controller on every CR this
	// handler creates, overwriting any value supplied by the client. This
	// enforces that HTTP-created resources always belong to the serving
	// controller instance, regardless of what the caller attempts to set.
	// Empty string means no enforcement (embedded mode).
	controllerName string
}

// NewResourceHandler creates a handler. backend may be nil, in which case
// runtime status is omitted from synthetic team member responses.
// controllerName, when non-empty, is force-stamped as hiclaw.io/controller
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

	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: h.namespace,
		},
		Spec: v1beta1.WorkerSpec{
			Model:         req.Model,
			Runtime:       req.Runtime,
			Image:         req.Image,
			Identity:      req.Identity,
			Soul:          req.Soul,
			Agents:        req.Agents,
			Skills:        req.Skills,
			McpServers:    req.McpServers,
			Package:       req.Package,
			Expose:        req.Expose,
			ChannelPolicy: req.ChannelPolicy,
			State:         req.State,
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
		httputil.WriteJSON(w, http.StatusOK, workerToResponse(&worker))
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

	// Standalone workers only when not filtering by team (team members don't
	// have Worker CRs).
	if teamFilter == "" {
		var list v1beta1.WorkerList
		if err := h.client.List(r.Context(), &list, client.InNamespace(h.namespace)); err != nil {
			writeK8sError(w, "list workers", err)
			return
		}
		for i := range list.Items {
			if isTeamMemberWorker(&list.Items[i]) {
				// Defensive: legacy resource created before the refactor. Skip
				// to avoid duplicating the synthesized team view.
				continue
			}
			workers = append(workers, workerToResponse(&list.Items[i]))
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
		workers = append(workers, h.teamMemberToResponse(r.Context(), team, team.Spec.Leader.Name))
		for _, worker := range team.Spec.Workers {
			workers = append(workers, h.teamMemberToResponse(r.Context(), team, worker.Name))
		}
	}

	httputil.WriteJSON(w, http.StatusOK, WorkerListResponse{Workers: workers, Total: len(workers)})
}

// isTeamMemberWorker reports whether a Worker CR was created by the old
// (pre-refactor) TeamReconciler and should be hidden from the aggregated
// /workers view.
func isTeamMemberWorker(w *v1beta1.Worker) bool {
	if w.Annotations == nil {
		return false
	}
	return w.Annotations["hiclaw.io/team"] != "" ||
		w.Annotations["hiclaw.io/team-leader"] != "" ||
		w.Annotations["hiclaw.io/role"] == "team_leader"
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
	if req.Leader.Name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "leader.name is required")
		return
	}

	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: h.namespace,
		},
		Spec: v1beta1.TeamSpec{
			Description:   req.Description,
			Admin:         req.Admin,
			PeerMentions:  req.PeerMentions,
			ChannelPolicy: req.ChannelPolicy,
			Leader: v1beta1.LeaderSpec{
				Name:              req.Leader.Name,
				Model:             req.Leader.Model,
				Identity:          req.Leader.Identity,
				Soul:              req.Leader.Soul,
				Agents:            req.Leader.Agents,
				Package:           req.Leader.Package,
				Heartbeat:         toHeartbeatSpec(req.Leader.Heartbeat),
				WorkerIdleTimeout: req.Leader.WorkerIdleTimeout,
				ChannelPolicy:     req.Leader.ChannelPolicy,
				State:             req.Leader.State,
			},
		},
	}

	for _, tw := range req.Workers {
		team.Spec.Workers = append(team.Spec.Workers, v1beta1.TeamWorkerSpec{
			Name:          tw.Name,
			Model:         tw.Model,
			Runtime:       tw.Runtime,
			Image:         tw.Image,
			Identity:      tw.Identity,
			Soul:          tw.Soul,
			Agents:        tw.Agents,
			Skills:        tw.Skills,
			McpServers:    tw.McpServers,
			Package:       tw.Package,
			Expose:        tw.Expose,
			ChannelPolicy: tw.ChannelPolicy,
			State:         tw.State,
		})
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
		if req.Admin != nil {
			team.Spec.Admin = req.Admin
		}
		if req.PeerMentions != nil {
			team.Spec.PeerMentions = req.PeerMentions
		}
		if req.ChannelPolicy != nil {
			team.Spec.ChannelPolicy = req.ChannelPolicy
		}
		if req.Leader != nil {
			if req.Leader.Model != "" {
				team.Spec.Leader.Model = req.Leader.Model
			}
			if req.Leader.Identity != "" {
				team.Spec.Leader.Identity = req.Leader.Identity
			}
			if req.Leader.Soul != "" {
				team.Spec.Leader.Soul = req.Leader.Soul
			}
			if req.Leader.Agents != "" {
				team.Spec.Leader.Agents = req.Leader.Agents
			}
			if req.Leader.Package != "" {
				team.Spec.Leader.Package = req.Leader.Package
			}
			if req.Leader.Heartbeat != nil {
				team.Spec.Leader.Heartbeat = toHeartbeatSpec(req.Leader.Heartbeat)
			}
			if req.Leader.WorkerIdleTimeout != "" {
				team.Spec.Leader.WorkerIdleTimeout = req.Leader.WorkerIdleTimeout
			}
			if req.Leader.ChannelPolicy != nil {
				team.Spec.Leader.ChannelPolicy = req.Leader.ChannelPolicy
			}
			if req.Leader.State != nil {
				team.Spec.Leader.State = req.Leader.State
			}
		}
		if req.Workers != nil {
			team.Spec.Workers = nil
			for _, tw := range req.Workers {
				team.Spec.Workers = append(team.Spec.Workers, v1beta1.TeamWorkerSpec{
					Name:          tw.Name,
					Model:         tw.Model,
					Runtime:       tw.Runtime,
					Image:         tw.Image,
					Identity:      tw.Identity,
					Soul:          tw.Soul,
					Agents:        tw.Agents,
					Skills:        tw.Skills,
					McpServers:    tw.McpServers,
					Package:       tw.Package,
					Expose:        tw.Expose,
					ChannelPolicy: tw.ChannelPolicy,
				})
			}
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
			Model:      req.Model,
			Runtime:    req.Runtime,
			Image:      req.Image,
			Soul:       req.Soul,
			Agents:     req.Agents,
			Skills:     req.Skills,
			McpServers: req.McpServers,
			Package:    req.Package,
			State:      req.State,
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
		Name:           w.Name,
		Phase:          w.Status.Phase,
		State:          w.Spec.DesiredState(),
		Model:          w.Spec.Model,
		Runtime:        w.Spec.Runtime,
		Image:          w.Spec.Image,
		ContainerState: w.Status.ContainerState,
		MatrixUserID:   w.Status.MatrixUserID,
		RoomID:         w.Status.RoomID,
		Message:        w.Status.Message,
	}
	if resp.Phase == "" {
		resp.Phase = "Pending"
	}
	if w.Annotations != nil {
		resp.Team = w.Annotations["hiclaw.io/team"]
		resp.Role = w.Annotations["hiclaw.io/role"]
	}
	for _, ep := range w.Status.ExposedPorts {
		resp.ExposedPorts = append(resp.ExposedPorts, ExposedPortInfo{Port: ep.Port, Domain: ep.Domain})
	}
	return resp
}

func teamToResponse(t *v1beta1.Team) TeamResponse {
	resp := TeamResponse{
		Name:              t.Name,
		Phase:             t.Status.Phase,
		Description:       t.Spec.Description,
		LeaderName:        t.Spec.Leader.Name,
		LeaderHeartbeat:   t.Spec.Leader.Heartbeat,
		WorkerIdleTimeout: t.Spec.Leader.WorkerIdleTimeout,
		TeamRoomID:        t.Status.TeamRoomID,
		LeaderDMRoomID:    t.Status.LeaderDMRoomID,
		LeaderReady:       t.Status.LeaderReady,
		ReadyWorkers:      t.Status.ReadyWorkers,
		TotalWorkers:      t.Status.TotalWorkers,
		Message:           t.Status.Message,
	}
	if resp.Phase == "" {
		resp.Phase = "Pending"
	}
	for _, w := range t.Spec.Workers {
		resp.WorkerNames = append(resp.WorkerNames, w.Name)
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
		if t.Spec.Leader.Name == name {
			return t, name, true, nil
		}
		for _, w := range t.Spec.Workers {
			if w.Name == name {
				return t, name, true, nil
			}
		}
	}
	return nil, "", false, nil
}

// teamMemberToResponse synthesizes a WorkerResponse for a Team member without
// creating a Worker CR. Runtime fields (Phase, ContainerState, ExposedPorts)
// are populated from Team.Status and the backend so existing consumers of
// /api/v1/workers see consistent data.
//
// Runtime resolution mirrors the response contract of standalone Worker CRs
// (see ListWorkers/GetWorker at the top of this file, which return
// w.Spec.Runtime verbatim): leader is hardcoded "copaw" to match the
// projection in leaderWorkerSpec(); worker is passed through from
// TeamWorkerSpec.Runtime (possibly empty, meaning "defer to installer
// default"), so Manager skills and `hiclaw get worker` observe the same
// value for Team workers as they would for standalone Workers.
func (h *ResourceHandler) teamMemberToResponse(ctx context.Context, t *v1beta1.Team, memberName string) WorkerResponse {
	isLeader := t.Spec.Leader.Name == memberName
	ms := t.Status.MemberByName(memberName)

	resp := WorkerResponse{
		Name:  memberName,
		Team:  t.Name,
		Phase: "Pending",
		State: "Running",
	}
	if ms != nil {
		resp.RoomID = ms.RoomID
		resp.MatrixUserID = ms.MatrixUserID
	}
	if isLeader {
		resp.Role = "team_leader"
		resp.Runtime = "copaw"
		resp.Model = t.Spec.Leader.Model
		if t.Spec.Leader.State != nil {
			resp.State = *t.Spec.Leader.State
		}
	} else {
		resp.Role = "worker"
		for _, wk := range t.Spec.Workers {
			if wk.Name != memberName {
				continue
			}
			resp.Model = wk.Model
			resp.Image = wk.Image
			resp.Runtime = wk.Runtime
			if wk.State != nil {
				resp.State = *wk.State
			}
			if ms != nil {
				for _, p := range ms.ExposedPorts {
					resp.ExposedPorts = append(resp.ExposedPorts, ExposedPortInfo{Port: p.Port, Domain: p.Domain})
				}
			}
			break
		}
	}

	if h.backend != nil {
		if wb := h.backend.DetectWorkerBackend(ctx); wb != nil {
			if result, err := wb.Status(ctx, memberName); err == nil && result != nil {
				resp.ContainerState = string(result.Status)
				switch result.Status {
				case backend.StatusRunning, backend.StatusReady:
					resp.Phase = "Running"
				case backend.StatusStarting:
					resp.Phase = "Pending"
				case backend.StatusStopped:
					resp.Phase = "Stopped"
				}
			}
		}
	}
	if isLeader && t.Status.LeaderReady {
		resp.Phase = "Running"
	}
	return resp
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
