package server

import (
	"context"
	"fmt"
	"net/http"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/httputil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// createTeamDecoupled handles the new decoupled format: creates independent
// Worker CRs then a Team CR referencing them via spec.workerMembers.
func (h *ResourceHandler) createTeamDecoupled(w http.ResponseWriter, r *http.Request, req *CreateTeamRequest) {
	if err := validateTeamMembers(req.Members); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()

	// Pre-flight: check for name collisions with existing Worker CRs and team members.
	for _, m := range req.Members {
		var existing v1beta1.Worker
		if err := h.client.Get(ctx, client.ObjectKey{Name: m.Name, Namespace: h.namespace}, &existing); err == nil {
			httputil.WriteError(w, http.StatusConflict, fmt.Sprintf("worker %q already exists", m.Name))
			return
		}
		if teamName, ok, err := h.findTeamForMember(ctx, m.Name); err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "list teams: "+err.Error())
			return
		} else if ok {
			httputil.WriteError(w, http.StatusConflict, fmt.Sprintf("name %q is already a member of team %q", m.Name, teamName))
			return
		}
	}

	memberRuntimes := make(map[string]string, len(req.Members))
	for _, m := range req.Members {
		memberRuntime := m.Runtime
		if m.Role == "team_leader" {
			var err error
			memberRuntime, err = normalizeLeaderRuntimeForCreate(m.Runtime)
			if err != nil {
				httputil.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		memberRuntimes[m.Name] = memberRuntime
	}

	// Create Worker CRs atomically (with rollback on failure).
	var created []string
	for _, m := range req.Members {
		worker := &v1beta1.Worker{
			ObjectMeta: metav1.ObjectMeta{
				Name:      m.Name,
				Namespace: h.namespace,
			},
			Spec: v1beta1.WorkerSpec{
				WorkerName:         m.WorkerName,
				Model:              m.Model,
				ModelProvider:      m.ModelProvider,
				Runtime:            memberRuntimes[m.Name],
				Image:              m.Image,
				Identity:           m.Identity,
				Soul:               m.Soul,
				Agents:             m.Agents,
				Skills:             m.Skills,
				McpServers:         m.McpServers,
				Package:            m.Package,
				Expose:             m.Expose,
				ChannelPolicy:      m.ChannelPolicy,
				Resources:          m.Resources,
				AgentIdentity:      m.AgentIdentity,
				CredentialBindings: m.CredentialBindings,
				ContainerManaged:   m.ContainerManaged,
				IdleTimeout:        m.IdleTimeout,
				State:              m.State,
			},
		}
		h.stampControllerLabel(&worker.ObjectMeta)
		if err := h.client.Create(ctx, worker); err != nil {
			h.rollbackWorkers(ctx, created)
			writeK8sError(w, fmt.Sprintf("create worker %q", m.Name), err)
			return
		}
		created = append(created, m.Name)
	}

	// Build Team CR with workerMembers references.
	var refs []v1beta1.TeamWorkerRef
	for _, m := range req.Members {
		refs = append(refs, v1beta1.TeamWorkerRef{Name: m.Name, Role: m.Role})
	}

	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: h.namespace,
		},
		Spec: v1beta1.TeamSpec{
			TeamName:       req.TeamName,
			Description:    req.Description,
			Admin:          req.Admin,
			HumanMembers:   req.HumanMembers,
			PeerMentions:   req.PeerMentions,
			ChannelPolicy:  req.ChannelPolicy,
			WorkerMembers:  refs,
			HeartbeatEvery: req.HeartbeatEvery,
		},
	}
	h.stampControllerLabel(&team.ObjectMeta)

	if err := h.client.Create(ctx, team); err != nil {
		h.rollbackWorkers(ctx, created)
		writeK8sError(w, "create team", err)
		return
	}

	httputil.WriteJSON(w, http.StatusCreated, teamToResponse(team))
}

// validateTeamMembers checks that members contain exactly one team_leader,
// no duplicates, and only valid roles.
func validateTeamMembers(members []TeamMemberRequest) error {
	seen := make(map[string]bool, len(members))
	var leaders []string
	for _, m := range members {
		if m.Name == "" {
			return fmt.Errorf("each member must have a non-empty name")
		}
		if seen[m.Name] {
			return fmt.Errorf("duplicate member name: %q", m.Name)
		}
		seen[m.Name] = true
		switch m.Role {
		case "team_leader":
			leaders = append(leaders, m.Name)
		case "worker":
			// ok
		default:
			return fmt.Errorf("invalid role %q for member %q; must be \"team_leader\" or \"worker\"", m.Role, m.Name)
		}
	}
	if len(leaders) == 0 {
		return fmt.Errorf("members must contain exactly one member with role \"team_leader\"")
	}
	if len(leaders) > 1 {
		return fmt.Errorf("members contains multiple leaders: %v", leaders)
	}
	return nil
}

// rollbackWorkers is a best-effort cleanup that deletes previously created Worker CRs.
func (h *ResourceHandler) rollbackWorkers(ctx context.Context, names []string) {
	for _, name := range names {
		_ = client.IgnoreNotFound(h.client.Delete(ctx, &v1beta1.Worker{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.namespace},
		}))
	}
}
