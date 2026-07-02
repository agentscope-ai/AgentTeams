//go:build integration

package controller_test

import (
	"fmt"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/test/testutil/fixtures"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestStatusConditionsReadyAcrossResources(t *testing.T) {
	resetMocks()
	resetManagerMocks()

	worker := fixtures.NewTestWorker(fixtures.UniqueName("cond-worker"))
	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("create Worker: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, worker) })
	waitForRunning(t, worker)
	assertWorkerReadyCondition(t, worker)

	mgr := fixtures.NewTestManager(fixtures.UniqueName("cond-manager"))
	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("create Manager: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, mgr) })
	waitForManagerRunning(t, mgr)
	assertManagerReadyCondition(t, mgr)

	teamName := fixtures.UniqueName("cond-team")
	team := fixtures.NewTestTeam(teamName, teamName+"-lead")
	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	t.Cleanup(func() { _ = deleteAndWait(t, team) })
	waitForTeamPhase(t, team, "Active")
	assertTeamReadyCondition(t, team)

	human := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fixtures.UniqueName("cond-human"),
			Namespace: fixtures.DefaultNamespace,
		},
		Spec: v1beta1.HumanSpec{
			DisplayName:     "Condition Human",
			PermissionLevel: 3,
		},
	}
	if err := k8sClient.Create(ctx, human); err != nil {
		t.Fatalf("create Human: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, human) })
	assertHumanReadyCondition(t, human)
}

func assertWorkerReadyCondition(t *testing.T, worker *v1beta1.Worker) {
	t.Helper()
	assertEventually(t, func() error {
		var got v1beta1.Worker
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &got); err != nil {
			return err
		}
		return assertReadyCondition(got.Status.Conditions, got.Generation, "Worker")
	})
}

func assertManagerReadyCondition(t *testing.T, mgr *v1beta1.Manager) {
	t.Helper()
	assertEventually(t, func() error {
		var got v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &got); err != nil {
			return err
		}
		return assertReadyCondition(got.Status.Conditions, got.Generation, "Manager")
	})
}

func assertTeamReadyCondition(t *testing.T, team *v1beta1.Team) {
	t.Helper()
	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		return assertReadyCondition(got.Status.Conditions, got.Generation, "Team")
	})
}

func assertHumanReadyCondition(t *testing.T, human *v1beta1.Human) {
	t.Helper()
	assertEventually(t, func() error {
		var got v1beta1.Human
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(human), &got); err != nil {
			return err
		}
		return assertReadyCondition(got.Status.Conditions, got.Generation, "Human")
	})
}

func assertReadyCondition(conditions []metav1.Condition, generation int64, kind string) error {
	cond := meta.FindStatusCondition(conditions, v1beta1.ConditionReady)
	if cond == nil {
		return fmt.Errorf("%s Ready condition missing from %v", kind, conditions)
	}
	if cond.Status != metav1.ConditionTrue {
		return fmt.Errorf("%s Ready condition status=%s reason=%s message=%q, want True",
			kind, cond.Status, cond.Reason, cond.Message)
	}
	if cond.ObservedGeneration != generation {
		return fmt.Errorf("%s Ready observedGeneration=%d, want %d",
			kind, cond.ObservedGeneration, generation)
	}
	return nil
}
