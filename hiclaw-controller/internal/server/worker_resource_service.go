package server

import (
	"context"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	authpkg "github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WorkerResourceService holds domain rules for standalone Worker CR lifecycle.
// HTTP handlers stay thin; API request/response shapes are unchanged.
type WorkerResourceService struct {
	client    client.Client
	namespace string
}

func NewWorkerResourceService(c client.Client, namespace string) *WorkerResourceService {
	return &WorkerResourceService{client: c, namespace: namespace}
}

// WorkerDomainError carries HTTP mapping for handler translation without changing API schema.
type WorkerDomainError struct {
	Status  int
	Message string
	K8sErr  error
}

// ValidateCreateStandaloneWorker enforces standalone /workers POST domain rules.
func (s *WorkerResourceService) ValidateCreateStandaloneWorker(ctx context.Context, req CreateWorkerRequest, caller *authpkg.CallerIdentity) (*v1beta1.Worker, *WorkerDomainError) {
	if req.Name == "" {
		return nil, &WorkerDomainError{Status: 400, Message: "name is required"}
	}
	if err := validation.ValidateResourceName(req.Name); err != nil {
		return nil, &WorkerDomainError{Status: 400, Message: err.Error()}
	}

	if team, ok, err := s.findTeamForMember(ctx, req.Name); err != nil {
		return nil, &WorkerDomainError{Status: 500, Message: "create worker: " + err.Error(), K8sErr: err}
	} else if ok {
		return nil, &WorkerDomainError{
			Status:  409,
			Message: "worker name is a member of team " + team + "; manage via PUT /api/v1/teams/" + team,
		}
	}

	if caller != nil && caller.Role == authpkg.RoleTeamLeader {
		return nil, &WorkerDomainError{
			Status:  409,
			Message: "team leaders must manage members via PUT /api/v1/teams/" + caller.Team,
		}
	}
	if req.Team != "" || req.Role != "" || req.TeamLeader != "" {
		return nil, &WorkerDomainError{
			Status:  400,
			Message: "worker.team / worker.role / worker.teamLeader are reserved for team members; use /api/v1/teams",
		}
	}

	containerManaged := true
	if req.ContainerManaged != nil {
		containerManaged = *req.ContainerManaged
	}
	runtime := req.Runtime
	if runtime == "" {
		runtime = backend.RuntimeOpenClaw
	}

	return &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: s.namespace,
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
	}, nil
}

func (s *WorkerResourceService) findTeamForMember(ctx context.Context, name string) (string, bool, error) {
	team, _, ok, err := s.findTeamMember(ctx, name)
	if err != nil || !ok {
		return "", false, err
	}
	return team.Name, true, nil
}

func (s *WorkerResourceService) findTeamMember(ctx context.Context, name string) (*v1beta1.Team, string, bool, error) {
	var indexed v1beta1.TeamList
	if err := s.client.List(ctx, &indexed,
		client.InNamespace(s.namespace),
		client.MatchingFields{teamWorkerMembersField: name},
	); err == nil {
		for i := range indexed.Items {
			t := &indexed.Items[i]
			for _, ref := range t.Spec.WorkerMembers {
				if ref.Name == name {
					return t, ref.Name, true, nil
				}
			}
		}
	}

	var list v1beta1.TeamList
	if err := s.client.List(ctx, &list, client.InNamespace(s.namespace)); err != nil {
		return nil, "", false, err
	}
	for i := range list.Items {
		t := &list.Items[i]
		if t.Spec.Leader.Name == name {
			return t, t.Spec.Leader.Name, true, nil
		}
		for _, w := range t.Spec.Workers {
			if w.Name == name {
				return t, w.Name, true, nil
			}
		}
		for _, ref := range t.Spec.WorkerMembers {
			if ref.Name == name {
				return t, ref.Name, true, nil
			}
		}
	}
	return nil, "", false, nil
}
