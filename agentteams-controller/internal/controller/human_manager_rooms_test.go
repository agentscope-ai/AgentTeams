package controller

import (
	"context"
	"errors"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestHumanManagerRoomLifecycle(t *testing.T) {
	human := newHuman("alice", v1beta1.HumanSpec{PermissionLevel: 2})
	manager := &v1beta1.Manager{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Status: v1beta1.ManagerStatus{MatrixUserID: "@manager:localhost", RoomID: "!system-admin:localhost"}}
	rig := newHumanRig(t, human, manager)
	creates := 0
	rig.prov.EnsureHumanManagerRoomFn = func(_ context.Context, req service.HumanManagerRoomRequest) (string, error) {
		creates++
		if req.HumanName != "alice" || req.ManagerName != "default" || req.HumanMatrixID != "@alice:localhost" {
			t.Fatalf("unexpected private room request: %+v", req)
		}
		return "!alice-manager:localhost", nil
	}
	out, _, err := rig.reconcile("alice")
	if err != nil || creates != 0 || len(out.Status.ManagerRooms) != 0 {
		t.Fatalf("L2 must not provision Manager DM: creates=%d err=%v", creates, err)
	}
	out.Spec.PermissionLevel = 1
	if err := rig.client.Update(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	out, _, err = rig.reconcile("alice")
	if err != nil || out.Status.ManagerRooms["default"] != "!alice-manager:localhost" || !containsString(out.Status.Rooms, "!alice-manager:localhost") {
		t.Fatalf("L1 promotion did not join own DM: status=%+v err=%v", out.Status, err)
	}
	if containsString(out.Status.Rooms, "!system-admin:localhost") {
		t.Fatal("joined another administrator's DM")
	}
	out, _, err = rig.reconcile("alice")
	if err != nil || creates != 1 {
		t.Fatalf("steady state reprovisioned room: creates=%d err=%v", creates, err)
	}
	out.Spec.PermissionLevel = 3
	if err := rig.client.Update(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	out, _, err = rig.reconcile("alice")
	if err != nil || containsString(out.Status.Rooms, "!alice-manager:localhost") || len(rig.prov.Calls.KickFromRoom) == 0 {
		t.Fatalf("demotion did not revoke DM: status=%+v err=%v", out.Status, err)
	}
	// The mapping survives revocation so re-promotion restores the user's history.
	out.Spec.PermissionLevel = 1
	if err := rig.client.Update(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	out, _, err = rig.reconcile("alice")
	if err != nil || creates != 2 || !containsString(out.Status.Rooms, "!alice-manager:localhost") {
		t.Fatalf("re-promotion did not restore own DM: status=%+v err=%v", out.Status, err)
	}
}

func TestHumanManagerRoomFailureRetriedWithoutSharingAdminRoom(t *testing.T) {
	manager := &v1beta1.Manager{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Status: v1beta1.ManagerStatus{MatrixUserID: "@manager:localhost", RoomID: "!system-admin:localhost"}}
	rig := newHumanRig(t, newHuman("alice", v1beta1.HumanSpec{PermissionLevel: 1}), manager)
	rig.prov.EnsureHumanManagerRoomFn = func(context.Context, service.HumanManagerRoomRequest) (string, error) {
		return "", errors.New("Matrix temporarily unavailable")
	}
	out, res, err := rig.reconcile("alice")
	if err == nil || res.RequeueAfter == 0 || len(out.Status.Rooms) != 0 || len(out.Status.ManagerRooms) != 0 {
		t.Fatalf("failed creation must retry without fallback: status=%+v result=%+v err=%v", out.Status, res, err)
	}
	// Manager installation is optional; an L1 remains usable without a Manager.
	if err := rig.client.Delete(context.Background(), manager); err != nil {
		t.Fatal(err)
	}
	_, _, err = rig.reconcile("alice")
	if err != nil {
		t.Fatal(err)
	}
}

func TestHumanManagerRequestsOnManagerChange(t *testing.T) {
	rig := newHumanRig(t, newHuman("alice", v1beta1.HumanSpec{PermissionLevel: 1}), newHuman("bob", v1beta1.HumanSpec{PermissionLevel: 2}))
	requests := rig.r.managerToHumanRequests(context.Background(), &v1beta1.Manager{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}})
	if len(requests) != 1 || requests[0].NamespacedName != (client.ObjectKey{Name: "alice", Namespace: "default"}) {
		t.Fatalf("Manager readiness must requeue L1 Humans: %+v", requests)
	}
}

func TestHumanManagerPendingInviteRevokedOnDemotion(t *testing.T) {
	human := newHuman("alice", v1beta1.HumanSpec{PermissionLevel: 3})
	human.Status = v1beta1.HumanStatus{MatrixUserID: "@alice:localhost", InitialPassword: "password", ManagerRooms: map[string]string{"default": "!pending:localhost"}}
	rig := newHumanRig(t, human)
	out, _, err := rig.reconcile("alice")
	if err != nil || len(rig.prov.Calls.KickFromRoom) != 1 || rig.prov.Calls.KickFromRoom[0].RoomID != "!pending:localhost" || len(out.Status.Rooms) != 0 {
		t.Fatalf("pending invite survived demotion: status=%+v err=%v", out.Status, err)
	}
}
