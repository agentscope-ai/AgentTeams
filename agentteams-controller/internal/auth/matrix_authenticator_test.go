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
		newHuman("maizong", "maizong", 2, "market-team"),
		newHuman("sunzong", "sunzong", 2, "biz-team", "sysdev-team"),
	)
	fw.userID = "@sunzong:matrix.local"

	id, err := auth.Authenticate(context.Background(), "matrix-token")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if id.Role != RoleHuman {
		t.Fatalf("role=%q, want human (read-only L2, not team-leader)", id.Role)
	}
	if id.Username != "sunzong" {
		t.Fatalf("username=%q, want sunzong", id.Username)
	}
	if len(id.Teams) != 2 || id.Teams[0] != "biz-team" || id.Teams[1] != "sysdev-team" {
		t.Fatalf("teams=%v, want [biz-team sysdev-team]", id.Teams)
	}
}

func TestMatrixAuthenticator_UnknownUserDenied(t *testing.T) {
	auth, fw := newMatrixAuthTest(t, newHuman("maizong", "maizong", 2, "market-team"))
	fw.userID = "@stranger:matrix.local"

	if _, err := auth.Authenticate(context.Background(), "matrix-token"); err == nil {
		t.Fatal("expected error for unknown matrix user")
	} else if !strings.Contains(err.Error(), "no L2 human matches") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMatrixAuthenticator_NonL2Denied(t *testing.T) {
	auth, fw := newMatrixAuthTest(t, newHuman("luo", "luo", 1))
	fw.userID = "@luo:matrix.local"

	if _, err := auth.Authenticate(context.Background(), "matrix-token"); err == nil {
		t.Fatal("expected error for level-1 human")
	} else if !strings.Contains(err.Error(), "not an L2") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMatrixAuthenticator_WhoamiFailure(t *testing.T) {
	auth, fw := newMatrixAuthTest(t, newHuman("maizong", "maizong", 2, "market-team"))
	fw.err = errors.New("matrix down")

	if _, err := auth.Authenticate(context.Background(), "matrix-token"); err == nil {
		t.Fatal("expected error when whoami fails")
	}
}

func TestCompositeAuthenticator_FallsBackToMatrix(t *testing.T) {
	matrixAuth, fw := newMatrixAuthTest(t, newHuman("maizong", "maizong", 2, "market-team"))
	fw.userID = "@maizong:matrix.local"

	// First authenticator always fails (e.g. SA TokenReview rejecting a
	// Matrix token); the composite must fall through to the Matrix path.
	alwaysFail := &matrixAuthFail{}
	composite := NewCompositeAuthenticator(alwaysFail, matrixAuth)

	id, err := composite.Authenticate(context.Background(), "matrix-token")
	if err != nil {
		t.Fatalf("composite authenticate: %v", err)
	}
	if id.Username != "maizong" || len(id.Teams) != 1 || id.Teams[0] != "market-team" {
		t.Fatalf("identity=%+v, want maizong/market-team", id)
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
	sso := newHuman("maizong", "maizong", 2, "market-team")
	sso.Status.MatrixUserID = "@3f9a2b:matrix.local" // derived from issuer+subject hash
	auth, fw := newMatrixAuthTest(t, sso)

	// Token belongs to the SSO-derived Matrix user id, NOT the localpart.
	fw.userID = "@3f9a2b:matrix.local"
	id, err := auth.Authenticate(context.Background(), "matrix-token")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if id.Username != "maizong" || len(id.Teams) != 1 || id.Teams[0] != "market-team" {
		t.Fatalf("identity=%+v, want maizong/market-team", id)
	}

	// A token for a localpart-only match must NOT match the reconciled SSO
	// human (its authoritative id is the hash, not the username).
	auth2, fw2 := newMatrixAuthTest(t, sso)
	fw2.userID = "@maizong:matrix.local"
	if _, err := auth2.Authenticate(context.Background(), "matrix-token"); err == nil {
		t.Fatal("expected error: reconciled SSO human must not match by localpart")
	}
}
