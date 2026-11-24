package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil/mocks"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestQoderUsesProjectedRuntimeContract(t *testing.T) {
	deployer := mocks.NewMockDeployer()
	state := &MemberState{MatrixUserID: "@qoder:matrix", RoomID: "!qoder:matrix", ProvResult: &service.WorkerProvisionResult{}}
	member := MemberContext{Name: "qoder", RuntimeName: "qoder", Role: RoleStandalone, Generation: 1, Spec: v1beta1.WorkerSpec{Runtime: backend.RuntimeQoder, Model: "test-model"}}
	if err := ReconcileMemberConfig(context.Background(), MemberDeps{Deployer: deployer}, member, state); err != nil {
		t.Fatal(err)
	}
	if len(deployer.Calls.DeployMemberRuntimeConfig) != 1 || deployer.Calls.DeployMemberRuntimeConfig[0].Runtime != backend.RuntimeQoder {
		t.Fatal("Qoder must receive runtime.yaml")
	}
	if packages, inline, config, skills, _ := deployer.CallCounts(); packages != 0 || inline != 0 || config != 0 || skills != 0 {
		t.Fatal("Qoder must not receive legacy runtime files")
	}
}

func TestQoderDoesNotCallQwenPawConsole(t *testing.T) {
	dials := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { dials++ }))
	defer server.Close()
	r := &WorkerReconciler{KubeMode: "embedded", DefaultRuntime: backend.RuntimeQoder, subagentModelURLFor: func(*v1beta1.Worker, v1beta1.WorkerSpec) string { return server.URL }}
	worker := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "qoder"}}
	spec := v1beta1.WorkerSpec{Runtime: backend.RuntimeQoder, Model: "test-model", SubagentModel: "test-subagent"}
	state := &MemberState{ContainerState: "running"}
	r.applyActiveModelHot(context.Background(), worker, spec, state)
	r.applySubagentModelHot(context.Background(), worker, spec, MemberContext{}, state)
	if dials != 0 {
		t.Fatalf("Qoder received %d QwenPaw console calls", dials)
	}
}
