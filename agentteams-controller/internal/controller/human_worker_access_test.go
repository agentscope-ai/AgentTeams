package controller

import (
	"context"
	"reflect"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestHumanWorkerGrantsUseProvisionedIdentityAndNamespace(t *testing.T) {
	now := metav1.Now()
	human := func(name, namespace, mxid string) *v1beta1.Human {
		return &v1beta1.Human{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       v1beta1.HumanSpec{AccessibleWorkers: []string{"worker", "worker"}},
			Status:     v1beta1.HumanStatus{MatrixUserID: mxid},
		}
	}
	deleting := human("deleted", "default", "@deleted:external")
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{finalizerName}
	c := newTeamTestClient(t,
		human("alice", "default", "@sso-alice:external"),
		human("bob", "default", "@bob:external"),
		human("pending", "default", ""),
		human("other", "elsewhere", "@other:external"), deleting,
	)
	grants, err := humanWorkerGrants(context.Background(), c, "default")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"@bob:external", "@sso-alice:external"}; !reflect.DeepEqual(grants["worker"], want) {
		t.Fatalf("grants=%v, want %v", grants, want)
	}
}

func TestWorkerHumanAccessGrantRevokeDeleteWithoutRecreate(t *testing.T) {
	for _, runtime := range []string{"qwenpaw", "openclaw"} {
		t.Run(runtime, func(t *testing.T) {
			ctx := context.Background()
			worker := newWorker("solo", v1beta1.WorkerSpec{
				Runtime: runtime, WorkerName: "runtime-alias", Model: "qwen-plus",
				ChannelPolicy: &v1beta1.ChannelPolicySpec{
					GroupAllowExtra: []string{"@manual:local"},
					DmDenyExtra:     []string{"@blocked:local"},
				},
			})
			human := &v1beta1.Human{
				ObjectMeta: metav1.ObjectMeta{Name: "human-cr", Namespace: "default"},
				Spec:       v1beta1.HumanSpec{AccessibleWorkers: []string{"solo"}},
				Status:     v1beta1.HumanStatus{MatrixUserID: "@actual-user:external"},
			}
			bob := human.DeepCopy()
			bob.Name = "bob"
			bob.Status.MatrixUserID = "@bob:external"
			rig := newWorkerRig(t, worker, human, bob)
			var initialHash string
			check := func(wantGrant bool) {
				t.Helper()
				rig.deployer.ClearCalls()
				out, _, err := rig.reconcile("solo")
				if err != nil {
					t.Fatal(err)
				}
				if initialHash != "" && out.Status.SpecHash != initialHash {
					t.Fatal("Human change recreated Worker hash")
				}
				initialHash = out.Status.SpecHash
				var spec v1beta1.WorkerSpec
				if runtime == "qwenpaw" {
					calls := rig.deployer.Calls.DeployMemberRuntimeConfig
					if len(calls) != 1 {
						t.Fatalf("runtime config calls=%d", len(calls))
					}
					spec = calls[0].Spec
				} else {
					calls := rig.deployer.Calls.DeployWorkerConfig
					if len(calls) != 1 {
						t.Fatalf("config calls=%d", len(calls))
					}
					if !calls[0].ReconcileChannelPolicy {
						t.Fatal("standalone Human allowlist reconciliation must be enabled")
					}
					spec = calls[0].Spec
				}
				for _, ids := range [][]string{spec.ChannelPolicy.GroupAllowExtra, spec.ChannelPolicy.DmAllowExtra} {
					if !stringSliceContains(ids, bob.Status.MatrixUserID) {
						t.Fatal("another Human's independent access was lost")
					}
					if stringSliceContains(ids, human.Status.MatrixUserID) != wantGrant {
						t.Fatalf("grant=%v, want %v", ids, wantGrant)
					}
				}
				if !stringSliceContains(spec.ChannelPolicy.GroupAllowExtra, "@manual:local") ||
					!stringSliceContains(spec.ChannelPolicy.DmDenyExtra, "@blocked:local") {
					t.Fatal("explicit policy was lost")
				}
				if stringSliceContains(out.Spec.ChannelPolicy.GroupAllowExtra, human.Status.MatrixUserID) {
					t.Fatal("derived grant persisted into Worker spec")
				}
			}
			check(true)
			check(true)
			rig.backend.SimulatePodDeletion("solo")
			check(true)
			rig.backend.ClearCalls()
			human.Spec.AccessibleWorkers = nil
			if err := rig.client.Update(ctx, human); err != nil {
				t.Fatal(err)
			}
			check(false)
			human.Spec.AccessibleWorkers = []string{"solo"}
			if err := rig.client.Update(ctx, human); err != nil {
				t.Fatal(err)
			}
			check(true)
			if err := rig.client.Delete(ctx, human); err != nil {
				t.Fatal(err)
			}
			check(false)
			creates, deletes, _, _, _ := rig.backend.CallSnapshot()
			if len(creates) != 0 || len(deletes) != 0 {
				t.Fatalf("Human changes must not recreate Worker: creates=%v deletes=%v", creates, deletes)
			}
		})
	}
}

func TestTeamHumanWorkerGrantsRespectDenyAndRuntimeOwnership(t *testing.T) {
	ctx := context.Background()
	human := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Spec:       v1beta1.HumanSpec{AccessibleWorkers: []string{"dev"}},
		Status:     v1beta1.HumanStatus{MatrixUserID: "@alice:external"},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "default"},
		Spec:       v1beta1.TeamSpec{WorkerMembers: []v1beta1.TeamWorkerRef{{Name: "lead", Role: "team_leader"}, {Name: "dev"}}},
	}
	lead := newWorker("lead", v1beta1.WorkerSpec{Runtime: "qwenpaw", Model: "qwen-plus"})
	dev := newWorker("dev", v1beta1.WorkerSpec{Runtime: "qwenpaw", Model: "qwen-plus"})
	rig := newTeamReconcileRig(t, team, lead, dev, human)
	_, _, err := rig.reconcile("team")
	if err != nil {
		t.Fatal(err)
	}
	var devReq *service.MemberRuntimeConfigDeployRequest
	for i := range rig.deployer.Calls.DeployMemberRuntimeConfig {
		req := &rig.deployer.Calls.DeployMemberRuntimeConfig[i]
		if req.Name == "dev" {
			devReq = req
		}
		if req.Name == "lead" && req.Spec.ChannelPolicy != nil && stringSliceContains(req.Spec.ChannelPolicy.DmAllowExtra, human.Status.MatrixUserID) {
			t.Fatal("direct Worker grant leaked to leader")
		}
	}
	if devReq == nil || devReq.Spec.ChannelPolicy == nil || !stringSliceContains(devReq.Spec.ChannelPolicy.DmAllowExtra, human.Status.MatrixUserID) {
		t.Fatal("Team-owned runtime config omitted direct Human grant")
	}
	human.Spec.AccessibleWorkers = nil
	if err := rig.client.Update(ctx, human); err != nil {
		t.Fatal(err)
	}
	rig.deployer.ClearCalls()
	if _, _, err := rig.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	for _, req := range rig.deployer.Calls.DeployMemberRuntimeConfig {
		if req.Spec.ChannelPolicy != nil && stringSliceContains(req.Spec.ChannelPolicy.DmAllowExtra, human.Status.MatrixUserID) {
			t.Fatal("revoked direct grant survived Team projection")
		}
	}
	var saved v1beta1.Worker
	if err := rig.client.Get(ctx, client.ObjectKeyFromObject(dev), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Spec.ChannelPolicy != nil {
		t.Fatal("Team persisted derived policy in Worker")
	}

	member := teamWorkerMember{ref: v1beta1.TeamWorkerRef{Name: "dev"}, runtimeName: "dev", humanMatrixIDs: []string{human.Status.MatrixUserID}}
	member.worker.Spec.ChannelPolicy = &v1beta1.ChannelPolicySpec{DmDenyExtra: []string{human.Status.MatrixUserID}}
	policy := rig.r.teamChannelPolicy(team, []teamWorkerMember{member}, "lead", member, RoleTeamWorker)
	if stringSliceContains(policy.DMAllowFrom, human.Status.MatrixUserID) {
		t.Fatal("direct Human grant overrode explicit DM deny")
	}
	if !stringSliceContains(policy.GroupAllowFrom, human.Status.MatrixUserID) {
		t.Fatal("group grant lost")
	}
}

func TestHumanAccessEventsEnqueueWorkersAndTeams(t *testing.T) {
	human := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Spec:       v1beta1.HumanSpec{AccessibleWorkers: []string{"dev"}},
	}
	worker := newWorker("dev", v1beta1.WorkerSpec{})
	team := &v1beta1.Team{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "default"}, Spec: v1beta1.TeamSpec{WorkerMembers: []v1beta1.TeamWorkerRef{{Name: "dev"}}}}
	c := newTeamTestClient(t, worker, team)
	workers := (&WorkerReconciler{Client: c}).humanToWorkerRequests(context.Background(), human)
	teams := (&TeamReconciler{Client: c}).humanToTeamRequests(context.Background(), human)
	if len(workers) != 1 || workers[0].Name != "dev" || len(teams) != 1 || teams[0].Name != "team" {
		t.Fatalf("workers=%v teams=%v", workers, teams)
	}
	// Revocation no longer lists the Worker; standalone fan-out must still
	// reconcile it. Team update handlers also map the old Human object above.
	human.Spec.AccessibleWorkers = nil
	if len((&WorkerReconciler{Client: c}).humanToWorkerRequests(context.Background(), human)) != 1 {
		t.Fatal("revocation did not enqueue Worker")
	}
}
