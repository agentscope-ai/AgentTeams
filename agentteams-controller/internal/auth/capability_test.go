package auth

import (
	"reflect"
	"testing"
)

func allCapabilities() []Capability {
	return []Capability{
		CapabilityFullAccess,
		CapabilityChannelSecrets,
		CapabilityExternalSources,
		CapabilityApprovalPolicy,
		CapabilitySecretReveal,
	}
}

func TestHasCapabilityRoleBaseline(t *testing.T) {
	// admin / manager: always true, even with an empty capability set.
	for _, role := range []string{RoleAdmin, RoleManager} {
		for _, cap := range allCapabilities() {
			if !HasCapability(&CallerIdentity{Role: role}, cap) {
				t.Errorf("role %s: HasCapability(%s) = false, want true", role, cap)
			}
		}
	}

	// worker: always false, even with a populated set.
	worker := &CallerIdentity{Role: RoleWorker, Capabilities: []string{string(CapabilityFullAccess)}}
	for _, cap := range allCapabilities() {
		if HasCapability(worker, cap) {
			t.Errorf("worker: HasCapability(%s) = true, want false", cap)
		}
	}

	// team-leader: SA identities never carry Capabilities → structurally
	// false (#1220 §5: team leaders never hold capabilities).
	leader := &CallerIdentity{Role: RoleTeamLeader, Username: "lead-1", Team: "t1"}
	for _, cap := range allCapabilities() {
		if HasCapability(leader, cap) {
			t.Errorf("leader: HasCapability(%s) = true, want false", cap)
		}
	}

	// nil caller and unknown role.
	if HasCapability(nil, CapabilityFullAccess) {
		t.Error("nil caller: HasCapability = true, want false")
	}
	if HasCapability(&CallerIdentity{Role: "unknown", Capabilities: []string{string(CapabilityFullAccess)}}, CapabilityFullAccess) {
		t.Error("unknown role: HasCapability = true, want false")
	}
}

func TestHasCapabilityHumanSetMembership(t *testing.T) {
	// L2 with no capabilities: false for every value.
	empty := &CallerIdentity{Role: RoleHuman, Username: "alice"}
	for _, cap := range allCapabilities() {
		if HasCapability(empty, cap) {
			t.Errorf("empty human: HasCapability(%s) = true, want false", cap)
		}
	}

	// L2 with exactly one value: true for that value (and, for the
	// full_access meta value, for every value).
	for _, held := range allCapabilities() {
		caller := &CallerIdentity{Role: RoleHuman, Username: "alice", Capabilities: []string{string(held)}}
		for _, want := range allCapabilities() {
			wantTrue := want == held || held == CapabilityFullAccess
			if got := HasCapability(caller, want); got != wantTrue {
				t.Errorf("human holding %q: HasCapability(%s) = %v, want %v", held, want, got, wantTrue)
			}
		}
	}
}

func TestHasCapabilityFullAccessMeta(t *testing.T) {
	// The full_access meta value implies every capability.
	caller := &CallerIdentity{Role: RoleHuman, Username: "alice", Capabilities: []string{string(CapabilityFullAccess)}}
	for _, cap := range allCapabilities() {
		if !HasCapability(caller, cap) {
			t.Errorf("full_access human: HasCapability(%s) = false, want true", cap)
		}
	}
}

// TestValidCapabilitiesMatchesDocumentedValueSet pins the closed value set
// against #1220 §3 (EN and ZH issue bodies): any drift between the code
// constants and the documented five values fails here.
func TestValidCapabilitiesMatchesDocumentedValueSet(t *testing.T) {
	want := []string{"approval_policy", "channel_secrets", "external_sources", "full_access", "secret_reveal"}
	if got := ValidCapabilityList(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ValidCapabilities drifted from the #1220 §3 value set: got %v, want %v", got, want)
	}
}

func TestNormalizeCapabilitiesDedupesAndSorts(t *testing.T) {
	in := []string{"channel_secrets", "full_access", "channel_secrets", "approval_policy"}
	want := []string{"approval_policy", "channel_secrets", "full_access"}
	if got := NormalizeCapabilities(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeCapabilities(%v) = %v, want %v", in, got, want)
	}
	if got := NormalizeCapabilities(nil); got != nil {
		t.Fatalf("NormalizeCapabilities(nil) = %v, want nil", got)
	}
	if got := NormalizeCapabilities([]string{"approval_policy"}); len(got) != 1 {
		t.Fatalf("NormalizeCapabilities(single) = %v, want 1 element", got)
	}
}
