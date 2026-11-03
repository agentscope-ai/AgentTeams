package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeWhoami struct {
	userID string
	err    error
}

func (f *fakeWhoami) Whoami(_ context.Context, token string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.userID, nil
}

func newHuman(name, username string, level int, teams ...string) *v1beta1.Human {
	return &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1beta1.HumanSpec{
			Username:        username,
			PermissionLevel: level,
			AccessibleTeams: teams,
		},
	}
}

func newMatrixAuthTest(t *testing.T, humans ...*v1beta1.Human) (*MatrixTokenAuthenticator, *fakeWhoami) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	objs := make([]runtime.Object, 0, len(humans))
	for _, h := range humans {
		objs = append(objs, h)
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	fw := &fakeWhoami{}
	return NewMatrixTokenAuthenticator(k8s, "default", fw), fw
}

func TestMatrixAuthenticator_ResolvesL2Human(t *testing.T) {
	auth, fw := newMatrixAuthTest(t,
		newHuman("alice", "alice", 2, "market-team"),
		newHuman("bob", "bob", 2, "biz-team", "sysdev-team"),
	)
	fw.userID = "@bob:matrix.local"

	id, err := auth.Authenticate(context.Background(), "matrix-token")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if id.Role != RoleHuman {
		t.Fatalf("role=%q, want human (read-only L2, not team-leader)", id.Role)
	}
	if id.Username != "bob" {
		t.Fatalf("username=%q, want bob", id.Username)
	}
	if len(id.Teams) != 2 || id.Teams[0] != "biz-team" || id.Teams[1] != "sysdev-team" {
		t.Fatalf("teams=%v, want [biz-team sysdev-team]", id.Teams)
	}
}

func TestMatrixAuthenticator_L2HumanCarriesCapabilities(t *testing.T) {
	h := newHuman("alice", "alice", 2, "market-team")
	h.Spec.Capabilities = []string{"approval_policy", "channel_secrets"}
	auth, fw := newMatrixAuthTest(t, h)
	fw.userID = "@alice:matrix.local"

	id, err := auth.Authenticate(context.Background(), "matrix-token")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if len(id.Capabilities) != 2 || id.Capabilities[0] != "approval_policy" || id.Capabilities[1] != "channel_secrets" {
		t.Fatalf("capabilities=%v, want [approval_policy channel_secrets]", id.Capabilities)
	}
	if !HasCapability(id, CapabilityApprovalPolicy) || !HasCapability(id, CapabilityChannelSecrets) {
		t.Fatal("HasCapability should be true for the granted values")
	}
	if HasCapability(id, CapabilityExternalSources) {
		t.Fatal("HasCapability(external_sources) = true, want false (not granted)")
	}
}

func TestMatrixAuthenticator_HumanWithoutCapabilitiesHasEmptySet(t *testing.T) {
	auth, fw := newMatrixAuthTest(t, newHuman("alice", "alice", 2, "market-team"))
	fw.userID = "@alice:matrix.local"

	id, err := auth.Authenticate(context.Background(), "matrix-token")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if len(id.Capabilities) != 0 {
		t.Fatalf("capabilities=%v, want empty (no field on the CR)", id.Capabilities)
	}
	for _, cap := range allCapabilities() {
		if HasCapability(id, cap) {
			t.Errorf("capability-less human: HasCapability(%s) = true, want false", cap)
		}
	}
}

func TestMatrixAuthenticator_UnknownUserDenied(t *testing.T) {
	auth, fw := newMatrixAuthTest(t, newHuman("alice", "alice", 2, "market-team"))
	fw.userID = "@stranger:matrix.local"

	if _, err := auth.Authenticate(context.Background(), "matrix-token"); err == nil {
		t.Fatal("expected error for unknown matrix user")
	} else if !strings.Contains(err.Error(), "no human with a supported permission level") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMatrixAuthenticator_UnsupportedLevelDenied(t *testing.T) {
	auth, fw := newMatrixAuthTest(t, newHuman("carol", "carol", 1))
	fw.userID = "@carol:matrix.local"

	if _, err := auth.Authenticate(context.Background(), "matrix-token"); err == nil {
		t.Fatal("expected error for level-1 human (admin SA territory)")
	} else if !strings.Contains(err.Error(), "unsupported permissionLevel") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestMatrixAuthenticator_ResolvesL3Human guards the L3 (worker-scoped)
// branch: permissionLevel=3 + accessibleWorkers resolves to a read-only
// human identity carrying exactly the assigned workers (#1220 §2/Q2).
func TestMatrixAuthenticator_ResolvesL3Human(t *testing.T) {
	h := newHuman("viewer", "viewer", 3)
	h.Spec.AccessibleWorkers = []string{"team-a-dev", "solo-helper"}
	auth, fw := newMatrixAuthTest(t, h)
	fw.userID = "@viewer:matrix.local"

	id, err := auth.Authenticate(context.Background(), "matrix-token")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if id.Role != RoleHuman {
		t.Fatalf("role=%q, want human (read-only L3, not team-leader)", id.Role)
	}
	if id.Username != "viewer" {
		t.Fatalf("username=%q, want viewer", id.Username)
	}
	if len(id.AccessibleWorkers) != 2 || id.AccessibleWorkers[0] != "team-a-dev" || id.AccessibleWorkers[1] != "solo-helper" {
		t.Fatalf("accessibleWorkers=%v, want [team-a-dev solo-helper]", id.AccessibleWorkers)
	}
	if len(id.Teams) != 0 {
		t.Fatalf("teams=%v, want empty (L3 is worker-scoped, not team-scoped)", id.Teams)
	}
	if len(id.Capabilities) != 0 {
		t.Fatalf("capabilities=%v, want empty (capabilities are L2 fields)", id.Capabilities)
	}
}

// TestMatrixAuthenticator_L3StrictPerLevel guards the level-isolation
// contract: an L3 human whose CR also carries accessibleTeams or
// capabilities gets NEITHER — the permissionLevel is the discriminator,
// and silently widening the scope by extra fields would blur the L2/L3
// boundary (#558: the fake CR carries the full combination on purpose).
func TestMatrixAuthenticator_L3StrictPerLevel(t *testing.T) {
	h := newHuman("viewer", "viewer", 3, "market-team")
	h.Spec.AccessibleWorkers = []string{"team-a-dev"}
	h.Spec.Capabilities = []string{"channel_secrets"}
	auth, fw := newMatrixAuthTest(t, h)
	fw.userID = "@viewer:matrix.local"

	id, err := auth.Authenticate(context.Background(), "matrix-token")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if len(id.AccessibleWorkers) != 1 || id.AccessibleWorkers[0] != "team-a-dev" {
		t.Fatalf("accessibleWorkers=%v, want [team-a-dev]", id.AccessibleWorkers)
	}
	if len(id.Teams) != 0 {
		t.Fatalf("teams=%v, want empty (L3 ignores accessibleTeams)", id.Teams)
	}
	if len(id.Capabilities) != 0 {
		t.Fatalf("capabilities=%v, want empty (L3 ignores capabilities)", id.Capabilities)
	}
}

// TestMatrixAuthenticator_L2IgnoresAccessibleWorkers is the mirror of the
// L3 strictness: an L2 human whose CR carries accessibleWorkers keeps its
// team scope only — the worker leg activates solely at permissionLevel=3.
func TestMatrixAuthenticator_L2IgnoresAccessibleWorkers(t *testing.T) {
	h := newHuman("scoped-user", "scoped-user", 2, "market-team")
	h.Spec.AccessibleWorkers = []string{"team-a-dev"}
	auth, fw := newMatrixAuthTest(t, h)
	fw.userID = "@scoped-user:matrix.local"

	id, err := auth.Authenticate(context.Background(), "matrix-token")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if id.Username != "scoped-user" {
		t.Fatalf("username=%q, want scoped-user", id.Username)
	}
	if len(id.Teams) != 1 || id.Teams[0] != "market-team" {
		t.Fatalf("teams=%v, want [market-team]", id.Teams)
	}
	if len(id.AccessibleWorkers) != 0 {
		t.Fatalf("accessibleWorkers=%v, want empty (L2 ignores accessibleWorkers)", id.AccessibleWorkers)
	}
}

func TestMatrixAuthenticator_WhoamiFailure(t *testing.T) {
	auth, fw := newMatrixAuthTest(t, newHuman("alice", "alice", 2, "market-team"))
	fw.err = errors.New("matrix down")

	if _, err := auth.Authenticate(context.Background(), "matrix-token"); err == nil {
		t.Fatal("expected error when whoami fails")
	}
}

func TestCompositeAuthenticator_FallsBackToMatrix(t *testing.T) {
	matrixAuth, fw := newMatrixAuthTest(t, newHuman("alice", "alice", 2, "market-team"))
	fw.userID = "@alice:matrix.local"

	// First authenticator always fails (e.g. SA TokenReview rejecting a
	// Matrix token); the composite must fall through to the Matrix path.
	alwaysFail := &matrixAuthFail{}
	composite := NewCompositeAuthenticator(alwaysFail, matrixAuth)

	id, err := composite.Authenticate(context.Background(), "matrix-token")
	if err != nil {
		t.Fatalf("composite authenticate: %v", err)
	}
	if id.Username != "alice" || len(id.Teams) != 1 || id.Teams[0] != "market-team" {
		t.Fatalf("identity=%+v, want alice/market-team", id)
	}
}

type matrixAuthFail struct{}

func (m *matrixAuthFail) Authenticate(_ context.Context, _ string) (*CallerIdentity, error) {
	return nil, errors.New("SA token review failed")
}

func TestCompositeAuthenticator_AllFail(t *testing.T) {
	composite := NewCompositeAuthenticator(&matrixAuthFail{}, &matrixAuthFail{})
	if _, err := composite.Authenticate(context.Background(), "token"); err == nil {
		t.Fatal("expected error when all authenticators fail")
	}
}

// TestMatrixAuthenticator_SSOHumanMatchesMatrixUserID guards the external_sso
// identity path: a reconciled SSO human has Status.MatrixUserID that may
// differ from spec.username (deterministic hash of issuer+subject), so
// matching must use the authoritative Matrix user id.
func TestMatrixAuthenticator_SSOHumanMatchesMatrixUserID(t *testing.T) {
	sso := newHuman("alice", "alice", 2, "market-team")
	sso.Status.MatrixUserID = "@3f9a2b:matrix.local" // derived from issuer+subject hash
	auth, fw := newMatrixAuthTest(t, sso)

	// Token belongs to the SSO-derived Matrix user id, NOT the localpart.
	fw.userID = "@3f9a2b:matrix.local"
	id, err := auth.Authenticate(context.Background(), "matrix-token")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if id.Username != "alice" || len(id.Teams) != 1 || id.Teams[0] != "market-team" {
		t.Fatalf("identity=%+v, want alice/market-team", id)
	}

	// A token for a localpart-only match must NOT match the reconciled SSO
	// human (its authoritative id is the hash, not the username).
	auth2, fw2 := newMatrixAuthTest(t, sso)
	fw2.userID = "@alice:matrix.local"
	if _, err := auth2.Authenticate(context.Background(), "matrix-token"); err == nil {
		t.Fatal("expected error: reconciled SSO human must not match by localpart")
	}
}
