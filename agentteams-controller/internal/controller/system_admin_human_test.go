package controller

import (
	"context"
	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"testing"
)

func TestDeletingSystemAdminHumanDoesNotDeactivateMatrixAccount(t *testing.T) {
	h := newHuman("admin", v1beta1.HumanSpec{PermissionLevel: 1})
	h.Labels = map[string]string{v1beta1.LabelSystemAdmin: "true"}
	rig := newHumanRig(t, h)
	rig.r.SystemAdminUser = "admin"
	current, _, err := rig.reconcile("admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.client.Delete(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rig.reconcile("admin"); err != nil {
		t.Fatal(err)
	}
	if len(rig.prov.Calls.DeactivateHumanUser) != 0 || len(rig.prov.Calls.ForceLeaveRoom) != 0 {
		t.Fatal("built-in account was deactivated or left rooms")
	}
}

func TestSystemAdminHumanDoesNotReprovisionAccountOrRooms(t *testing.T) {
	h := newHuman("admin", v1beta1.HumanSpec{PermissionLevel: 1, DisplayName: "admin"})
	h.Labels = map[string]string{v1beta1.LabelSystemAdmin: "true"}
	h.Status.Rooms = []string{"!existing:localhost"}
	h.Status.Phase = "Degraded"
	h.Status.Message = "old provisioning failure"
	rig := newHumanRig(t, h)
	rig.r.SystemAdminUser = "admin"
	for range 2 {
		actual, _, err := rig.reconcile("admin")
		if err != nil {
			t.Fatal(err)
		}
		if actual.Status.Phase != "Active" || actual.Status.MatrixUserID == "" || actual.Status.InitialPassword != "" {
			t.Fatalf("unexpected status: %+v", actual.Status)
		}
		if len(actual.Status.Rooms) != 1 {
			t.Fatal("existing rooms changed")
		}
	}
	calls := rig.prov.Calls
	if len(calls.RegisterLegacyUser)+len(calls.RegisterAppServiceUser)+len(calls.EnsureHumanUser)+len(calls.SetUserPassword)+len(calls.LoginAsHuman)+len(calls.LoginWithPassword)+len(calls.LoginAppServiceUser)+len(calls.SetDisplayName)+len(calls.JoinRoomAs)+len(calls.LeaveRoomAs)+len(calls.ForceLeaveRoom) != 0 {
		t.Fatalf("system admin Matrix account was modified: %+v", calls)
	}
}
