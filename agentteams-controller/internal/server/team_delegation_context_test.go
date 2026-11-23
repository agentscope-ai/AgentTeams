package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGetTeamReturnsDelegationContextWithoutOtherWorkersOrSecrets(t *testing.T) {
	team := &v1beta1.Team{}
	team.Name, team.Namespace = "delivery", "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "lead", Role: "team_leader"}, {Name: "missing", Role: "worker"}}
	leader := &v1beta1.Worker{}
	leader.Name, leader.Namespace = "lead", "default"
	leader.Status.MatrixUserID, leader.Status.Phase = "@lead:test", "Running"
	leader.Spec.Runtime, leader.Spec.Skills = "qwenpaw", []string{"delivery"}
	leader.Spec.Env = map[string]string{"SECRET": "private-value"}
	other := &v1beta1.Worker{}
	other.Name, other.Namespace = "unrelated", "default"
	cli := fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).WithObjects(team, leader, other).Build()
	handler := NewResourceHandler(cli, "default", nil, "", nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/teams/delivery", nil)
	req.SetPathValue("name", "delivery")
	rec := httptest.NewRecorder()
	handler.GetTeam(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	var response TeamResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Members) != 2 || response.Members[0].MatrixUserID != "@lead:test" || response.Members[1].Phase != "Missing" {
		t.Fatalf("incomplete delegation context: %+v", response.Members)
	}
	if !response.Members[0].NativeCoordinationSupported || response.Members[1].NativeCoordinationSupported {
		t.Fatal("native transport capability did not match the registered runtime")
	}
	if strings.Contains(rec.Body.String(), "private-value") || strings.Contains(rec.Body.String(), "unrelated") {
		t.Fatal("team context exposed unrelated or private data")
	}
}
