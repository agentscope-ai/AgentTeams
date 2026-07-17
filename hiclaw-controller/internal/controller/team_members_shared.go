package controller

import (
	"context"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/matrix"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (r *TeamReconciler) reconcileMember(ctx context.Context, deps MemberDeps, m MemberContext, ms *v1beta1.TeamMemberStatus) (reconcile.Result, error) {
	// Validate cross-cluster deployment fields before entering phases.
	if err := ValidateMemberDeployment(m); err != nil {
		return reconcile.Result{}, err
	}

	state := &MemberState{}

	// Pre-populate ExistingMatrixUserID when we've already provisioned the
	// member before, forcing the Refresh path instead of Provision.
	if m.IsUpdate {
		m.ExistingMatrixUserID = r.Provisioner.MatrixUserID(m.RuntimeName)
	}

	res, err := ReconcileMemberInfra(ctx, deps, m, state)
	if err != nil {
		return reconcile.Result{}, err
	}
	if res.RequeueAfter > 0 {
		// ReconcileMemberInfra signals a short backoff (rather than an
		// error) only when the Matrix AppService token is not active yet
		// (transient startup race). Stop before flipping ms.Observed or
		// running later phases — Matrix/Gateway/Room are not provisioned —
		// and propagate the sentinel so the caller requeues quickly
		// instead of treating infra as successful.
		return reconcile.Result{}, matrix.ErrAppServiceNotReady
	}
	if err := EnsureModelProviderAuth(ctx, deps, m, state); err != nil {
		return reconcile.Result{}, err
	}
	ms.Observed = true
	if state.RoomID != "" {
		ms.RoomID = state.RoomID
	}
	if state.MatrixUserID != "" {
		ms.MatrixUserID = state.MatrixUserID
	}
	ms.RuntimeName = m.RuntimeName
	if err := EnsureMemberServiceAccount(ctx, deps, m); err != nil {
		return reconcile.Result{}, err
	}
	if err := ReconcileMemberConfig(ctx, deps, m, state); err != nil {
		return reconcile.Result{}, err
	}
	containerRes, err := ReconcileMemberContainer(ctx, deps, m, state)
	if state.ContainerState != "" {
		ms.ContainerState = state.ContainerState
		ms.Phase = computeMemberPhase(ms.Phase, ms.MatrixUserID, m.Spec.DesiredState(), ms.ContainerState, nil)
	}
	if state.Message != "" {
		ms.Message = state.Message
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	if containerRes.RequeueAfter > 0 {
		return containerRes, nil
	}
	if _, err := ReconcileMemberService(ctx, &m, &deps); err != nil {
		return reconcile.Result{}, err
	}
	_ = ReconcileMemberExpose(ctx, deps, m, state)

	if m.Role == RoleTeamWorker {
		ms.ExposedPorts = state.ExposedPorts
	} else {
		ms.ExposedPorts = nil
	}
	ms.SpecHash = m.AppliedSpecHash
	return reconcile.Result{}, nil
}

// summarizeBackendReadiness queries each member's pod/container status from
// the backend and writes ms.Ready per member. Used instead of reading Worker
// CR status because team members no longer have Worker CRs.
//
// On a backend-unreachable path (Backend == nil or DetectWorkerBackend nil)
// this preserves any previously-recorded ms.Ready value — callers should NOT
// treat a false/true gap across reconciles as a transition, since a transient
// backend outage would otherwise flap Phase=Active back to Pending.
func (r *TeamReconciler) summarizeBackendReadiness(ctx context.Context, t *v1beta1.Team, members []MemberContext) (leaderReady bool, readyWorkers int) {
	if r.Backend == nil {
		return false, 0
	}
	for _, m := range members {
		mwb, err := resolveBackendForMember(r.Backend, m.BackendRuntime, m)
		if err != nil {
			logger := log.FromContext(ctx)
			logger.Error(err, "failed to resolve member backend", "member", m.Name, "role", m.Role)
			// Preserve previously-recorded readiness, consistent with the
			// nil-backend early-return contract (see function doc).
			if ms := t.Status.MemberByName(m.Name); ms != nil && ms.Ready {
				if m.Role == RoleTeamLeader {
					leaderReady = true
				} else {
					readyWorkers++
				}
			}
			continue
		}
		result, err := mwb.Status(ctx, m.Name)
		if err != nil {
			logger := log.FromContext(ctx)
			logger.Error(err, "failed to query member backend status", "member", m.Name, "role", m.Role)
			// Preserve previously-recorded readiness in the return values
			// to avoid phase flapping on transient backend errors, consistent
			// with the nil-backend early-return contract (see function doc).
			if ms := t.Status.MemberByName(m.Name); ms != nil && ms.Ready {
				if m.Role == RoleTeamLeader {
					leaderReady = true
				} else {
					readyWorkers++
				}
			}
			continue
		}
		ready := result.Status == backend.StatusRunning || result.Status == backend.StatusReady
		if ms := t.Status.MemberByName(m.Name); ms != nil {
			ms.Ready = ready
			ms.ContainerState = string(result.Status)
			ms.Message = result.Message
			ms.Phase = computeMemberPhase(ms.Phase, ms.MatrixUserID, m.Spec.DesiredState(), ms.ContainerState, nil)
		}
		if m.Role == RoleTeamLeader {
			leaderReady = ready
			continue
		}
		if ready {
			readyWorkers++
		}
	}
	return leaderReady, readyWorkers
}
func (r *TeamReconciler) writeInlineConfigs(t *v1beta1.Team) error {
	return nil
}
