package auth

import "sort"

// Capability is a named sensitive-surface privilege granted beyond the L2
// role baseline (#1220 §3). This block is the single source of truth for the
// value set: consumers must not hardcode capability strings elsewhere. The
// documented set is pinned by TestValidCapabilitiesMatchesDocumentedValueSet.
type Capability string

const (
	// CapabilityFullAccess is a meta value implying all capabilities.
	// PermissionLevel 1 (admin) likewise implies all of them at the role
	// baseline, without storing the value.
	CapabilityFullAccess Capability = "full_access"
	// CapabilityChannelSecrets authorizes writing channel credentials.
	CapabilityChannelSecrets Capability = "channel_secrets"
	// CapabilityExternalSources authorizes the remoteSkills registry and
	// credential management (the mcpServers/remoteSkills restore path).
	CapabilityExternalSources Capability = "external_sources"
	// CapabilityApprovalPolicy authorizes setting worker approval_level=OFF
	// (L2 tool-approval endpoints, #1216).
	CapabilityApprovalPolicy Capability = "approval_policy"
	// CapabilitySecretReveal is reserved; no v1 consumer (#1220 §6.4).
	CapabilitySecretReveal Capability = "secret_reveal"
)

// ValidCapabilities is the closed value set accepted on the Human CR and the
// human-update API.
var ValidCapabilities = map[Capability]struct{}{
	CapabilityFullAccess:      {},
	CapabilityChannelSecrets:  {},
	CapabilityExternalSources: {},
	CapabilityApprovalPolicy:  {},
	CapabilitySecretReveal:    {},
}

// IsValidCapability reports whether cap is in the closed value set.
func IsValidCapability(cap Capability) bool {
	_, ok := ValidCapabilities[cap]
	return ok
}

// ValidCapabilityList returns the documented value set, sorted — for error
// messages and docs.
func ValidCapabilityList() []string {
	out := make([]string, 0, len(ValidCapabilities))
	for c := range ValidCapabilities {
		out = append(out, string(c))
	}
	sort.Strings(out)
	return out
}

// NormalizeCapabilities dedupes and sorts a capability list (canonical form
// for storage comparison and diffing). Purely mechanical: unknown values are
// NOT filtered — validation happens at admission (human-update handler).
func NormalizeCapabilities(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(list))
	out := make([]string, 0, len(list))
	for _, c := range list {
		if c == "" {
			continue
		}
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// HasCapability reports whether the caller holds cap.
//
// Role baseline first (#1220 §3 check order: role baseline AND team scope
// AND capability — this helper covers the capability half only; callers
// still enforce TeamMatches on the target team):
//   - admin / manager: always true (L1 full access);
//   - worker: always false (workers hold no capabilities);
//   - human / team-leader: set membership, with the full_access meta value
//     implying every capability.
//
// A capability NEVER implies team scope, and never the role baseline in the
// reverse direction. Team leaders are not granted capabilities by any
// provisioner (SA identities never carry Capabilities), so their set
// membership is structurally empty.
func HasCapability(caller *CallerIdentity, cap Capability) bool {
	if caller == nil {
		return false
	}
	switch caller.Role {
	case RoleAdmin, RoleManager:
		return true
	case RoleWorker:
		return false
	case RoleHuman, RoleTeamLeader:
	default:
		return false
	}
	for _, c := range caller.Capabilities {
		if c == string(CapabilityFullAccess) || c == string(cap) {
			return true
		}
	}
	return false
}
