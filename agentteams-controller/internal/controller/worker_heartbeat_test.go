package controller

import (
	"context"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestWorkerReconcileRetainsTeamLeaderHeartbeat(t *testing.T) {
	leader := newWorker("leader", v1beta1.WorkerSpec{Runtime: "openclaw"})
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "default"},
		Spec:       v1beta1.TeamSpec{HeartbeatEvery: "15m", WorkerMembers: []v1beta1.TeamWorkerRef{{Name: "leader", Role: "team_leader"}}},
	}
	rig := newWorkerRig(t, leader, team)
	if _, _, err := rig.reconcile("leader"); err != nil {
		t.Fatal(err)
	}
	got := rig.deployer.Calls.DeployWorkerConfig[0].Heartbeat
	if got == nil || !got.Enabled || got.Every != "15m" {
		t.Fatalf("Worker reconcile lost the Team heartbeat: %#v", got)
	}
}

func TestWorkerHeartbeatDoesNotRecreateContainer(t *testing.T) {
	spec := v1beta1.WorkerSpec{Model: "qwen3.6-plus", Runtime: "openclaw"}
	want := hashAppliedWorkerSpec(spec)
	for _, enabled := range []bool{true, false} {
		spec.LlmHeartbeatEnabled = &enabled
		if hashAppliedWorkerSpec(spec) != want {
			t.Fatal("heartbeat-only edit changes the container hash")
		}
	}
}

func TestWorkerHeartbeatReconcileAppliesWithoutRecreate(t *testing.T) {
	worker := newWorker("solo", v1beta1.WorkerSpec{Runtime: "openclaw", Model: "qwen-plus"})
	worker.Generation = 1
	rig := newWorkerRig(t, worker)
	if _, _, err := rig.reconcile("solo"); err != nil {
		t.Fatal(err)
	}
	for i, enabled := range []bool{false, true, false} {
		rig.backend.ClearCalls()
		rig.deployer.ClearCalls()
		var live v1beta1.Worker
		if err := rig.client.Get(context.Background(), client.ObjectKeyFromObject(worker), &live); err != nil {
			t.Fatal(err)
		}
		live.Spec.LlmHeartbeatEnabled = &enabled
		live.Generation = int64(i + 2)
		if err := rig.client.Update(context.Background(), &live); err != nil {
			t.Fatal(err)
		}
		if _, _, err := rig.reconcile("solo"); err != nil {
			t.Fatal(err)
		}
		calls := rig.deployer.Calls.DeployWorkerConfig
		if len(calls) != 1 || calls[0].Spec.LlmHeartbeatEnabled == nil || *calls[0].Spec.LlmHeartbeatEnabled != enabled {
			t.Fatalf("switch not projected: %#v", calls)
		}
		creates, deletes, _, _, _ := rig.backend.CallSnapshot()
		if len(creates) != 0 || len(deletes) != 0 {
			t.Fatalf("heartbeat edit recreated container: %v %v", creates, deletes)
		}
	}
}
