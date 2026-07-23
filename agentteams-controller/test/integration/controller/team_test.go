//go:build integration

package controller_test

import (
	"fmt"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil/fixtures"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestTeamReferencesExistingWorkerCRs(t *testing.T) {
	resetMocks()

	leader := fixtures.NewTestWorker(fixtures.UniqueName("team-lead"))
	leader.Spec.Runtime = "copaw"
	leader.Spec.Model = "leader-model"
	worker := fixtures.NewTestWorker(fixtures.UniqueName("team-worker"))
	worker.Spec.Runtime = "hermes"
	worker.Spec.Model = "worker-model"
	for _, item := range []*v1beta1.Worker{leader, worker} {
		if err := k8sClient.Create(ctx, item); err != nil {
			t.Fatalf("create Worker %s: %v", item.Name, err)
		}
		waitForRunning(t, item)
	}

	team := fixtures.NewTestTeam(fixtures.UniqueName("team"), leader.Name, worker.Name)
	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, team)
		_ = k8sClient.Delete(ctx, leader)
		_ = k8sClient.Delete(ctx, worker)
	})

	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		if got.Status.Phase != "Active" || !got.Status.LeaderReady || got.Status.ReadyWorkers != 1 {
			return fmt.Errorf("status=%+v", got.Status)
		}
		return nil
	})

	var gotLeader, gotWorker v1beta1.Worker
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(leader), &gotLeader); err != nil {
		t.Fatalf("get leader Worker: %v", err)
	}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &gotWorker); err != nil {
		t.Fatalf("get member Worker: %v", err)
	}
	if gotLeader.Spec.Runtime != "copaw" || gotLeader.Spec.Model != "leader-model" {
		t.Fatalf("Team mutated leader Worker spec: %+v", gotLeader.Spec)
	}
	if gotWorker.Spec.Runtime != "hermes" || gotWorker.Spec.Model != "worker-model" {
		t.Fatalf("Team mutated member Worker spec: %+v", gotWorker.Spec)
	}
}

func TestTeamMissingWorkerReferenceIsDegraded(t *testing.T) {
	resetMocks()

	leader := fixtures.NewTestWorker(fixtures.UniqueName("missing-lead"))
	if err := k8sClient.Create(ctx, leader); err != nil {
		t.Fatalf("create leader Worker: %v", err)
	}
	waitForRunning(t, leader)

	missing := fixtures.UniqueName("missing-worker")
	team := fixtures.NewTestTeam(fixtures.UniqueName("missing-team"), leader.Name, missing)
	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, team)
		_ = k8sClient.Delete(ctx, leader)
	})

	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		if got.Status.Phase != "Degraded" || !strings.Contains(got.Status.Message, missing) {
			return fmt.Errorf("status=%+v", got.Status)
		}
		return nil
	})
}

func TestDeletingTeamPreservesReferencedWorkers(t *testing.T) {
	resetMocks()

	leader := fixtures.NewTestWorker(fixtures.UniqueName("delete-lead"))
	worker := fixtures.NewTestWorker(fixtures.UniqueName("delete-worker"))
	for _, item := range []*v1beta1.Worker{leader, worker} {
		if err := k8sClient.Create(ctx, item); err != nil {
			t.Fatalf("create Worker %s: %v", item.Name, err)
		}
		waitForRunning(t, item)
	}
	team := fixtures.NewTestTeam(fixtures.UniqueName("delete-team"), leader.Name, worker.Name)
	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	waitForTeamActive(t, team)

	if err := k8sClient.Delete(ctx, team); err != nil {
		t.Fatalf("delete Team: %v", err)
	}
	assertEventually(t, func() error {
		var got v1beta1.Team
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got)
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("Team still exists")
	})

	for _, item := range []*v1beta1.Worker{leader, worker} {
		var got v1beta1.Worker
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(item), &got); err != nil {
			t.Fatalf("referenced Worker %s was deleted: %v", item.Name, err)
		}
		_ = k8sClient.Delete(ctx, item)
	}
}

func waitForTeamActive(t *testing.T, team *v1beta1.Team) {
	t.Helper()
	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		if got.Status.Phase != "Active" {
			return fmt.Errorf("phase=%q, want Active", got.Status.Phase)
		}
		return nil
	})
}
