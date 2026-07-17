package controller

import (
	"strings"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/slicesx"
)

type channelAllowLists struct {
	GroupAllowFrom []string
	DMAllowFrom    []string
}

// memberChannelIdentity is the minimal view of a team member used by the
// unified channel policy builder for both legacy inline members and decoupled
// Worker CR references.
type memberChannelIdentity struct {
	Name              string
	RuntimeName       string
	Role              MemberRole
	IndividualPolicy  *v1beta1.ChannelPolicySpec
	SkipPeerAllowlist bool // true for the current member when computing peer mentions
}

func (r *TeamReconciler) matrixIDResolver() func(string) string {
	return func(value string) string {
		if value == "" || strings.HasPrefix(value, "@") {
			return value
		}
		if r.Legacy != nil && r.Legacy.Enabled() {
			return r.Legacy.MatrixUserID(value)
		}
		if r.Provisioner != nil {
			return r.Provisioner.MatrixUserID(value)
		}
		return value
	}
}

func (r *TeamReconciler) buildMemberChannelPolicy(
	t *v1beta1.Team,
	members []memberChannelIdentity,
	current memberChannelIdentity,
	leaderRuntimeName string,
) channelAllowLists {
	resolve := r.matrixIDResolver()

	if leaderRuntimeName == "" {
		for _, member := range members {
			if member.Role == RoleTeamLeader {
				leaderRuntimeName = member.RuntimeName
				break
			}
		}
	}

	managerMatrixID := resolve("manager")
	coordinatorIDs := teamCoordinatorIDs(t)

	var systemAdminID string
	if r.SystemAdminUser != "" {
		systemAdminID = resolve(r.SystemAdminUser)
	}

	groupAllow := make([]string, 0)
	dmAllow := make([]string, 0)

	if current.Role == RoleTeamLeader {
		groupAllow = append(groupAllow, managerMatrixID, systemAdminID)
		groupAllow = appendResolved(groupAllow, resolve, coordinatorIDs...)
		for _, member := range members {
			if member.Role == RoleTeamLeader {
				continue
			}
			groupAllow = append(groupAllow, resolve(member.RuntimeName))
		}
		dmAllow = append(dmAllow, managerMatrixID, systemAdminID)
		dmAllow = appendResolved(dmAllow, resolve, coordinatorIDs...)
	} else {
		leaderMatrixID := resolve(leaderRuntimeName)
		groupAllow = append(groupAllow, leaderMatrixID, systemAdminID)
		groupAllow = appendResolved(groupAllow, resolve, coordinatorIDs...)
		if t.Spec.PeerMentions == nil || *t.Spec.PeerMentions {
			for _, member := range members {
				if member.Role == RoleTeamLeader || member.SkipPeerAllowlist {
					continue
				}
				groupAllow = append(groupAllow, resolve(member.RuntimeName))
			}
		}
		dmAllow = append(dmAllow, leaderMatrixID, systemAdminID)
		dmAllow = appendResolved(dmAllow, resolve, coordinatorIDs...)
	}

	policy := mergeChannelPolicy(t.Spec.ChannelPolicy, current.IndividualPolicy)
	if policy != nil {
		groupAllow = applyChannelAllowPolicy(groupAllow, policy.GroupAllowExtra, policy.GroupDenyExtra, resolve)
		dmAllow = applyChannelAllowPolicy(dmAllow, policy.DmAllowExtra, policy.DmDenyExtra, resolve)
	}
	return channelAllowLists{
		GroupAllowFrom: slicesx.UniqueNonEmpty(groupAllow),
		DMAllowFrom:    slicesx.UniqueNonEmpty(dmAllow),
	}
}

func legacyMemberChannelIdentities(members []MemberContext, current MemberContext) []memberChannelIdentity {
	out := make([]memberChannelIdentity, 0, len(members))
	for _, member := range members {
		out = append(out, memberChannelIdentity{
			Name:              member.Name,
			RuntimeName:       member.RuntimeName,
			Role:              member.Role,
			IndividualPolicy:  member.Spec.ChannelPolicy,
			SkipPeerAllowlist: member.Name == current.Name,
		})
	}
	return out
}

func decoupledMemberChannelIdentities(members []decoupledTeamMember, leaderName string, current decoupledTeamMember, role MemberRole) ([]memberChannelIdentity, memberChannelIdentity) {
	out := make([]memberChannelIdentity, 0, len(members))
	for _, member := range members {
		memberRole := RoleTeamWorker
		if member.ref.Name == leaderName {
			memberRole = RoleTeamLeader
		}
		out = append(out, memberChannelIdentity{
			Name:              member.ref.Name,
			RuntimeName:       member.runtimeName,
			Role:              memberRole,
			IndividualPolicy:  member.worker.Spec.ChannelPolicy,
			SkipPeerAllowlist: member.ref.Name == current.ref.Name,
		})
	}
	currentIdentity := memberChannelIdentity{
		Name:             current.ref.Name,
		RuntimeName:      current.runtimeName,
		Role:             role,
		IndividualPolicy: current.worker.Spec.ChannelPolicy,
	}
	return out, currentIdentity
}

func (r *TeamReconciler) legacyChannelPolicy(t *v1beta1.Team, members []MemberContext, current MemberContext, leaderRuntimeName string) channelAllowLists {
	identities := legacyMemberChannelIdentities(members, current)
	currentIdentity := memberChannelIdentity{
		Name:             current.Name,
		RuntimeName:      current.RuntimeName,
		Role:             current.Role,
		IndividualPolicy: current.Spec.ChannelPolicy,
	}
	return r.buildMemberChannelPolicy(t, identities, currentIdentity, leaderRuntimeName)
}

func (r *TeamReconciler) decoupledChannelPolicy(t *v1beta1.Team, members []decoupledTeamMember, leaderName string, current decoupledTeamMember, role MemberRole) channelAllowLists {
	identities, currentIdentity := decoupledMemberChannelIdentities(members, leaderName, current, role)
	leaderRuntimeName := decoupledLeaderMember(members, leaderName).runtimeName
	return r.buildMemberChannelPolicy(t, identities, currentIdentity, leaderRuntimeName)
}

func mergeChannelPolicy(teamPolicy, individualPolicy *v1beta1.ChannelPolicySpec) *v1beta1.ChannelPolicySpec {
	if teamPolicy == nil && individualPolicy == nil {
		return nil
	}
	merged := &v1beta1.ChannelPolicySpec{}
	if teamPolicy != nil {
		merged.GroupAllowExtra = append(merged.GroupAllowExtra, teamPolicy.GroupAllowExtra...)
		merged.GroupDenyExtra = append(merged.GroupDenyExtra, teamPolicy.GroupDenyExtra...)
		merged.DmAllowExtra = append(merged.DmAllowExtra, teamPolicy.DmAllowExtra...)
		merged.DmDenyExtra = append(merged.DmDenyExtra, teamPolicy.DmDenyExtra...)
	}
	if individualPolicy != nil {
		merged.GroupAllowExtra = append(merged.GroupAllowExtra, individualPolicy.GroupAllowExtra...)
		merged.GroupDenyExtra = append(merged.GroupDenyExtra, individualPolicy.GroupDenyExtra...)
		merged.DmAllowExtra = append(merged.DmAllowExtra, individualPolicy.DmAllowExtra...)
		merged.DmDenyExtra = append(merged.DmDenyExtra, individualPolicy.DmDenyExtra...)
	}
	return merged
}

func appendResolved(values []string, resolve func(string) string, items ...string) []string {
	for _, item := range items {
		values = append(values, resolve(item))
	}
	return values
}

func applyChannelAllowPolicy(base, allowExtra, denyExtra []string, resolve func(string) string) []string {
	out := append([]string{}, base...)
	out = appendResolved(out, resolve, allowExtra...)
	deny := make(map[string]struct{}, len(denyExtra)*2)
	for _, item := range denyExtra {
		if item == "" {
			continue
		}
		deny[item] = struct{}{}
		deny[resolve(item)] = struct{}{}
	}
	filtered := make([]string, 0, len(out))
	for _, item := range out {
		if item == "" {
			continue
		}
		if _, ok := deny[item]; ok {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func appendGroupAllowExtra(policy *v1beta1.ChannelPolicySpec, names ...string) *v1beta1.ChannelPolicySpec {
	if len(names) == 0 {
		return policy
	}
	if policy == nil {
		policy = &v1beta1.ChannelPolicySpec{}
	}
	existing := make(map[string]bool, len(policy.GroupAllowExtra))
	for _, v := range policy.GroupAllowExtra {
		existing[v] = true
	}
	for _, n := range names {
		if n != "" && !existing[n] {
			policy.GroupAllowExtra = append(policy.GroupAllowExtra, n)
			existing[n] = true
		}
	}
	return policy
}

func appendDmAllowExtra(policy *v1beta1.ChannelPolicySpec, names ...string) *v1beta1.ChannelPolicySpec {
	if len(names) == 0 {
		return policy
	}
	if policy == nil {
		policy = &v1beta1.ChannelPolicySpec{}
	}
	existing := make(map[string]bool, len(policy.DmAllowExtra))
	for _, v := range policy.DmAllowExtra {
		existing[v] = true
	}
	for _, n := range names {
		if n != "" && !existing[n] {
			policy.DmAllowExtra = append(policy.DmAllowExtra, n)
			existing[n] = true
		}
	}
	return policy
}
