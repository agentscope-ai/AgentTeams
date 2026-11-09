package auth

import (
	"context"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestTeamAdminScopeRefreshesWithCachedToken(t *testing.T) {
	ctx := context.Background()
	a, whoami := newMatrixAuthTest(t, newHuman("alice", "alice", 2, "explicit"))
	whoami.userID = "@alice:matrix.local"
	id, err := a.Authenticate(ctx, "token")
	if err != nil || len(id.Teams) != 1 {
		t.Fatalf("initial identity=%+v err=%v", id, err)
	}
	team := &v1beta1.Team{ObjectMeta: metav1.ObjectMeta{Name: "owned", Namespace: "default"}, Spec: v1beta1.TeamSpec{Admin: &v1beta1.TeamAdminSpec{Name: "alice"}}}
	if err := a.k8s.Create(ctx, team); err != nil {
		t.Fatal(err)
	}
	other := &v1beta1.Team{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default"}, Spec: v1beta1.TeamSpec{Admin: &v1beta1.TeamAdminSpec{Name: "bob"}}}
	if err := a.k8s.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	id, err = a.Authenticate(ctx, "token")
	if err != nil || !id.TeamMatches("owned") || !id.TeamMatches("explicit") || id.TeamMatches("other") {
		t.Fatalf("new admin scope=%+v err=%v", id, err)
	}
	team.Spec.Admin.Name = "bob"
	if err := a.k8s.Update(ctx, team); err != nil {
		t.Fatal(err)
	}
	id, err = a.Authenticate(ctx, "token")
	if err != nil || id.TeamMatches("owned") {
		t.Fatalf("revoked scope=%+v err=%v", id, err)
	}
	var human v1beta1.Human
	if err := a.k8s.Get(ctx, client.ObjectKey{Name: "alice", Namespace: "default"}, &human); err != nil {
		t.Fatal(err)
	}
	if err := a.k8s.Delete(ctx, &human); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(ctx, "token"); err == nil {
		t.Fatal("deleted human retained cached access")
	}
}
