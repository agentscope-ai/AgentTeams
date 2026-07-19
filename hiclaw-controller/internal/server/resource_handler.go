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
	"github.com/hiclaw/hiclaw-controller/internal/validation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// k8sUpdateMaxRetries is the max attempts for Get→patch spec→Update against
// optimistic locking conflicts when the controller updates status between Get and Update.
const k8sUpdateMaxRetries = 3

const teamWorkerMembersField = "spec.workerMembers.name"

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
	workers   *WorkerResourceService
	health    *WorkerHealthProber

	// controllerName is stamped as agentteams.io/controller on every CR this
	// handler creates, overwriting any value supplied by the client. This
	// enforces that HTTP-created resources always belong to the serving
	// controller instance, regardless of what the caller attempts to set.
	// Empty string means no enforcement (embedded mode).
	controllerName string

	// soloOperator, when true, makes CreateHuman default PermissionLevel
	// to 1 (Admin) when the request omits one (zero value). There is only
	// one human in solo mode, so requiring an explicit admin grant on
	// every install is unnecessary friction. Set via SetSoloOperator;
	// defaults to false (unchanged multi-user behavior) so the existing
	// NewResourceHandler constructor signature does not need to change.
	// See docs/implementation-milestone-1.md Step 7 scope guard: this is
	// intentionally the ONLY PermissionLevel touch-point for solo mode —
	// a deeper strip across handlers is explicitly deferred.
	soloOperator bool
}

// SetSoloOperator configures whether this handler runs in single-operator
// mode. Kept as a post-construction setter rather than a NewResourceHandler
// parameter to avoid rippling a signature change across its ~20 existing
// call sites for a single narrow behavior change.
func (h *ResourceHandler) SetSoloOperator(solo bool) {
	h.soloOperator = solo
}

func (h *ResourceHandler) SetWorkerHealthProber(health *WorkerHealthProber) {
	h.health = health
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
		workers:        NewWorkerResourceService(c, namespace),
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

	caller := authpkg.CallerFromContext(r.Context())
	worker, derr := h.workers.ValidateCreateStandaloneWorker(r.Context(), req, caller)
	if derr != nil {
		if derr.K8sErr != nil {
			writeK8sError(w, "create worker", derr.K8sErr)
			return
		}
		httputil.WriteError(w, derr.Status, derr.Message)
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
		if team, member, ok, terr := h.workers.findTeamMember(r.Context(), name); terr != nil {
			writeK8sError(w, "get worker", terr)
			return
		} else if ok {
			h.applyTeamMember(&resp, team, member)
		}
		h.health.AttachHealthChecks(r.Context(), &resp, &worker)
		httputil.WriteJSON(w, http.StatusOK, resp)
		return
	case !apierrors.IsNotFound(err):
		writeK8sError(w, "get worker", err)
		return
	}

	// Fall back to synthesizing a response from the Team CR.
	team, member, ok, terr := h.workers.findTeamMember(r.Context(), name)
	if terr != nil {
		writeK8sError(w, "get worker", terr)
		return
	}
	if !ok {
		httputil.WriteError(w, http.StatusNotFound, "get worker: not found")
		return
	}
	resp := h.teamMemberToResponse(r.Context(), team, member)
	h.health.AttachHealthChecks(r.Context(), &resp, nil)
	httputil.WriteJSON(w, http.StatusOK, resp)
}

func (h *ResourceHandler) ListWorkers(w http.ResponseWriter, r *http.Request) {
	teamFilter := r.URL.Query().Get("team")

	type listEntry struct {
		resp   WorkerResponse
		worker *v1beta1.Worker
	}
	entries := make([]listEntry, 0)

	seen := make(map[string]struct{})
	var list v1beta1.WorkerList
	if err := h.client.List(r.Context(), &list, client.InNamespace(h.namespace)); err != nil {
		writeK8sError(w, "list workers", err)
		return
	}
	for i := range list.Items {
		resp := workerToResponse(&list.Items[i])
		if team, member, ok, terr := h.workers.findTeamMember(r.Context(), list.Items[i].Name); terr != nil {
			writeK8sError(w, "list workers: lookup team member", terr)
			return
		} else if ok {
			h.applyTeamMember(&resp, team, member)
		} else if isTeamMemberWorker(&list.Items[i]) {
			// Defensive: legacy resource created before the refactor. Skip
			// to avoid duplicating the synthesized team view.
			continue
		}
		if teamFilter != "" && resp.Team != teamFilter {
			continue
		}
		entries = append(entries, listEntry{resp: resp, worker: &list.Items[i]})
		seen[resp.Name] = struct{}{}
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
		if team.Spec.Leader.Name != "" {
			if _, ok := seen[team.Spec.Leader.Name]; !ok {
				resp := h.teamMemberToResponse(r.Context(), team, team.Spec.Leader.Name)
				entries = append(entries, listEntry{resp: resp, worker: nil})
				seen[team.Spec.Leader.Name] = struct{}{}
			}
		}
		for _, worker := range team.Spec.Workers {
			if _, ok := seen[worker.Name]; ok {
				continue
			}
			resp := h.teamMemberToResponse(r.Context(), team, worker.Name)
			entries = append(entries, listEntry{resp: resp, worker: nil})
			seen[worker.Name] = struct{}{}
		}
		for _, ref := range team.Spec.WorkerMembers {
			if _, ok := seen[ref.Name]; ok {
				continue
			}
			resp := h.teamMemberToResponse(r.Context(), team, ref.Name)
			entries = append(entries, listEntry{resp: resp, worker: nil})
			seen[ref.Name] = struct{}{}
		}
	}

	if h.health != nil && len(entries) > 0 {
		items := make([]healthAttachItem, len(entries))
		for i := range entries {
			items[i] = healthAttachItem{Resp: &entries[i].resp, Worker: entries[i].worker}
		}
		h.health.AttachHealthChecksBatch(r.Context(), items)
	}

	workers := make([]WorkerResponse, len(entries))
	for i := range entries {
		workers[i] = entries[i].resp
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
	return w.Annotations["agentteams.io/team"] != "" ||
		w.Annotations["agentteams.io/team-leader"] != "" ||
		w.Annotations["agentteams.io/role"] == "team_leader"
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

	if team, ok, err := h.workers.findTeamForMember(r.Context(), name); err != nil {
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

	if team, ok, err := h.workers.findTeamForMember(r.Context(), name); err != nil {
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
	if err := validation.ValidateResourceName(req.Name); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Leader.Name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "leader.name is required")
		return
	}
	if err := validation.ValidateResourceName(req.Leader.Name); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "leader.name: "+err.Error())
		return
	}
	for _, tw := range req.Workers {
		if tw.Name == "" {
			httputil.WriteError(w, http.StatusBadRequest, "workers[].name is required")
			return
		}
		if err := validation.ValidateResourceName(tw.Name); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "workers[].name: "+err.Error())
			return
		}
	}

	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: h.namespace,
		},
		Spec: v1beta1.TeamSpec{
			Description:   req.Description,
			TeamName:      req.TeamName,
			Admin:         req.Admin,
			HumanMembers:  req.HumanMembers,
			PeerMentions:  req.PeerMentions,
			ChannelPolicy: req.ChannelPolicy,
			ModelProvider: req.ModelProvider,
			Leader: v1beta1.LeaderSpec{
				Name:              req.Leader.Name,
				WorkerName:        req.Leader.WorkerName,
				Model:             req.Leader.Model,
				ModelProvider:     req.Leader.ModelProvider,
				Identity:          req.Leader.Identity,
				Soul:              req.Leader.Soul,
				Agents:            req.Leader.Agents,
				Package:           req.Leader.Package,
				McpServers:        req.Leader.McpServers,
				Heartbeat:         toHeartbeatSpec(req.Leader.Heartbeat),
				WorkerIdleTimeout: req.Leader.WorkerIdleTimeout,
				ChannelPolicy:     req.Leader.ChannelPolicy,
				State:             req.Leader.State,
				Resources:         req.Leader.Resources,
			},
		},
	}

	for _, tw := range req.Workers {
		team.Spec.Workers = append(team.Spec.Workers, v1beta1.TeamWorkerSpec{
			Name:          tw.Name,
			WorkerName:    tw.WorkerName,
			Model:         tw.Model,
			ModelProvider: tw.ModelProvider,
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
			Resources:     tw.Resources,
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
		if req.TeamName != "" {
			team.Spec.TeamName = req.TeamName
		}
		if req.ModelProvider != "" {
			team.Spec.ModelProvider = req.ModelProvider
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
			if req.Leader.WorkerName != "" {
				team.Spec.Leader.WorkerName = req.Leader.WorkerName
			}
			if req.Leader.Model != "" {
				team.Spec.Leader.Model = req.Leader.Model
			}
			if req.Leader.ModelProvider != "" {
				team.Spec.Leader.ModelProvider = req.Leader.ModelProvider
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
			if req.Leader.McpServers != nil {
				team.Spec.Leader.McpServers = req.Leader.McpServers
			}
			if req.Leader.ChannelPolicy != nil {
				team.Spec.Leader.ChannelPolicy = req.Leader.ChannelPolicy
			}
			if req.Leader.State != nil {
				team.Spec.Leader.State = req.Leader.State
			}
			if req.Leader.Resources != nil {
				team.Spec.Leader.Resources = req.Leader.Resources
			}
		}
		if req.Workers != nil {
			team.Spec.Workers = nil
			for _, tw := range req.Workers {
				team.Spec.Workers = append(team.Spec.Workers, v1beta1.TeamWorkerSpec{
					Name:          tw.Name,
					WorkerName:    tw.WorkerName,
					Model:         tw.Model,
					ModelProvider: tw.ModelProvider,
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
					Resources:     tw.Resources,
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
	if err := validation.ValidateResourceName(req.Name); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	permissionLevel := req.PermissionLevel
	if h.soloOperator && permissionLevel == 0 {
		// Solo mode: the sole Human defaults to Admin (1) when the caller
		// didn't specify a level. Only applies to the zero-value (unset);
		// an explicit non-zero level from the caller is never overridden.
		permissionLevel = 1
	}

	human := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: h.namespace,
		},
		Spec: v1beta1.HumanSpec{
			DisplayName:       req.DisplayName,
			Email:             req.Email,
			PermissionLevel:   permissionLevel,
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
	if err := validation.ValidateResourceName(req.Name); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
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

// --- Projects ---

func (h *ResourceHandler) CreateProject(w http.ResponseWriter, r *http.Request) {
	var req CreateProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := validation.ValidateResourceName(req.Name); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Team == "" {
		httputil.WriteError(w, http.StatusBadRequest, "team is required")
		return
	}
	if len(req.Repos) == 0 {
		httputil.WriteError(w, http.StatusBadRequest, "at least one repo is required")
		return
	}
	for _, repo := range req.Repos {
		if repo.URL == "" {
			httputil.WriteError(w, http.StatusBadRequest, "repos[].url is required")
			return
		}
		if repo.Access != "rw" && repo.Access != "ro" {
			httputil.WriteError(w, http.StatusBadRequest, "repos[].access must be rw or ro")
			return
		}
	}

	proj := &v1beta1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: h.namespace,
		},
		Spec: v1beta1.ProjectSpec{
			Team:        req.Team,
			Description: req.Description,
			ProjectName: req.ProjectName,
			Repos:       req.Repos,
			Workers:     req.Workers,
			DependsOn:   req.DependsOn,
		},
	}

	h.stampControllerLabel(&proj.ObjectMeta)

	if err := h.client.Create(r.Context(), proj); err != nil {
		writeK8sError(w, "create project", err)
		return
	}

	httputil.WriteJSON(w, http.StatusCreated, projectToResponse(proj))
}

func (h *ResourceHandler) GetProject(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "project name is required")
		return
	}

	var proj v1beta1.Project
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &proj); err != nil {
		writeK8sError(w, "get project", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, projectToResponse(&proj))
}

func (h *ResourceHandler) ListProjects(w http.ResponseWriter, r *http.Request) {
	teamFilter := r.URL.Query().Get("team")

	var list v1beta1.ProjectList
	if err := h.client.List(r.Context(), &list, client.InNamespace(h.namespace)); err != nil {
		writeK8sError(w, "list projects", err)
		return
	}

	projects := make([]ProjectResponse, 0, len(list.Items))
	for i := range list.Items {
		if teamFilter != "" && list.Items[i].Spec.Team != teamFilter {
			continue
		}
		projects = append(projects, projectToResponse(&list.Items[i]))
	}

	httputil.WriteJSON(w, http.StatusOK, ProjectListResponse{Projects: projects, Total: len(projects)})
}

// allowedProjectPhases are the operator-settable phases (decision #18).
// Reconciler-computed phases (Pending/Provisioning/Ready/Degraded/Failed)
// cannot be set through the API.
var allowedProjectPhases = map[string]bool{
	"Completed": true,
	"Archived":  true,
}

func (h *ResourceHandler) UpdateProject(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "project name is required")
		return
	}

	var req UpdateProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Phase != "" && !allowedProjectPhases[req.Phase] {
		httputil.WriteError(w, http.StatusBadRequest, "phase must be one of: Completed, Archived (other phases are reconciler-computed)")
		return
	}
	for _, repo := range req.Repos {
		if repo.Access != "" && repo.Access != "rw" && repo.Access != "ro" {
			httputil.WriteError(w, http.StatusBadRequest, "repos[].access must be rw or ro")
			return
		}
	}

	ctx := r.Context()
	for attempt := 0; attempt < k8sUpdateMaxRetries; attempt++ {
		var proj v1beta1.Project
		if err := h.client.Get(ctx, client.ObjectKey{Name: name, Namespace: h.namespace}, &proj); err != nil {
			writeK8sError(w, "get project for update", err)
			return
		}

		if req.Description != "" {
			proj.Spec.Description = req.Description
		}
		if req.ProjectName != "" {
			proj.Spec.ProjectName = req.ProjectName
		}
		if req.Repos != nil {
			proj.Spec.Repos = req.Repos
		}
		if req.Workers != nil {
			proj.Spec.Workers = req.Workers
		}
		if req.DependsOn != nil {
			proj.Spec.DependsOn = req.DependsOn
		}

		if err := h.client.Update(ctx, &proj); err != nil {
			if apierrors.IsConflict(err) && attempt+1 < k8sUpdateMaxRetries {
				time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
				continue
			}
			writeK8sError(w, "update project", err)
			return
		}

		// Phase is an operator-set status field (decision #18); patch it
		// separately via the status subresource after the spec update lands.
		if req.Phase != "" && req.Phase != proj.Status.Phase {
			statusBase := proj.DeepCopy()
			proj.Status.Phase = req.Phase
			if err := h.client.Status().Patch(ctx, &proj, client.MergeFrom(statusBase)); err != nil {
				writeK8sError(w, "update project status", err)
				return
			}
		}

		httputil.WriteJSON(w, http.StatusOK, projectToResponse(&proj))
		return
	}
}

func (h *ResourceHandler) DeleteProject(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "project name is required")
		return
	}

	proj := &v1beta1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.namespace},
	}
	if err := h.client.Delete(r.Context(), proj); err != nil {
		writeK8sError(w, "delete project", err)
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
		LastHeartbeat:  w.Status.LastHeartbeat,
		LastActiveAt:   w.Status.LastActiveAt,
		LLMCallsLastHeartbeat: w.Status.LLMCallsLastHeartbeat,
		LLMCallsTotal:         w.Status.LLMCallsTotal,
		HealthState:           w.Status.HealthState,
	}
	if w.Spec.ContainerManaged != nil {
		resp.ContainerManaged = *w.Spec.ContainerManaged
	}
	if resp.Phase == "" {
		resp.Phase = "Pending"
	}
	if w.Annotations != nil {
		resp.Team = w.Annotations["agentteams.io/team"]
		resp.Role = w.Annotations["agentteams.io/role"]
	}
	for _, ep := range w.Status.ExposedPorts {
		resp.ExposedPorts = append(resp.ExposedPorts, ExposedPortInfo{Port: ep.Port, Domain: ep.Domain})
	}
	return resp
}

func teamToResponse(t *v1beta1.Team) TeamResponse {
	resp := TeamResponse{
		Name:              t.Name,
		TeamName:          t.Spec.EffectiveTeamName(t.Name),
		Phase:             t.Status.Phase,
		Description:       t.Spec.Description,
		Admin:             t.Spec.Admin,
		HumanMembers:      t.Spec.HumanMembers,
		LeaderName:        t.Spec.Leader.Name,
		LeaderHeartbeat:   t.Spec.Leader.Heartbeat,
		WorkerIdleTimeout: t.Spec.Leader.WorkerIdleTimeout,
		ModelProvider:     t.Spec.ModelProvider,
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

func projectToResponse(p *v1beta1.Project) ProjectResponse {
	resp := ProjectResponse{
		Name:            p.Name,
		Team:            p.Spec.Team,
		Description:     p.Spec.Description,
		ProjectName:     p.Spec.EffectiveProjectName(p.Name),
		Repos:           p.Spec.Repos,
		Workers:         p.Spec.Workers,
		DependsOn:       p.Spec.DependsOn,
		Phase:           p.Status.Phase,
		Message:         p.Status.Message,
		RepoCount:       p.Status.RepoCount,
		RecordedWorkers: p.Status.RecordedWorkers,
		Dependencies:    p.Status.Dependencies,
		Conditions:      p.Status.Conditions,
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

func (h *ResourceHandler) applyTeamMember(resp *WorkerResponse, t *v1beta1.Team, memberName string) {
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
	if t.Spec.Leader.Name == memberName {
		return "team_leader"
	}
	return "worker"
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
	isLeader := teamMemberRole(t, memberName) == "team_leader"
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
		resp.LastHeartbeat = ms.LastHeartbeat
		resp.LastActiveAt = ms.LastActiveAt
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
