//go:build integration

package controller_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/controller"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil/fixtures"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil/mocks"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcfg "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const leaderElectionTimeout = 30 * time.Second

func leaderElectionOpts(id string) ctrl.Options {
	ld := 4 * time.Second
	rd := 3 * time.Second
	rp := 1 * time.Second
	return ctrl.Options{
		Scheme:                        testutil.Scheme(),
		LeaderElection:                true,
		LeaderElectionID:              id,
		LeaderElectionNamespace:       "default",
		LeaderElectionReleaseOnCancel: true,
		LeaseDuration:                 &ld,
		RenewDeadline:                 &rd,
		RetryPeriod:                   &rp,
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		Controller: ctrlcfg.Controller{
			SkipNameValidation: ptr.To(true),
		},
	}
}

// TestLeaderElection_SingleInstance_BecomesLeader verifies that a single
// controller instance with leader election enabled can acquire the lease
// and reconcile normally.
func TestLeaderElection_SingleInstance_BecomesLeader(t *testing.T) {
	leaseID := fixtures.UniqueName("le-single")
	mgrCtx, mgrCancel := context.WithCancel(ctx)
	defer mgrCancel()

	prov := mocks.NewMockProvisioner()
	deploy := mocks.NewMockDeployer()
	be := mocks.NewMockWorkerBackend()
	env := mocks.NewMockEnvBuilder()

	mgr, err := ctrl.NewManager(restCfg, leaderElectionOpts(leaseID))
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	reconciler := &controller.WorkerReconciler{
		Client:         mgr.GetClient(),
		Provisioner:    prov,
		Deployer:       deploy,
		Backend:        backend.NewRegistry([]backend.WorkerBackend{be}),
		EnvBuilder:     env,
		ControllerName: "test-ctl",
	}
	if _, err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatalf("failed to setup reconciler: %v", err)
	}

	go func() {
		if err := mgr.Start(mgrCtx); err != nil {
			t.Logf("manager exited: %v", err)
		}
	}()

	select {
	case <-mgr.Elected():
	case <-time.After(leaderElectionTimeout):
		t.Fatal("timed out waiting for leader election")
	}

	workerName := fixtures.UniqueName("le-single-w")
	worker := fixtures.NewTestWorker(workerName)
	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("failed to create Worker CR: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, worker) })

	assertEventually(t, func() error {
		var w v1beta1.Worker
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &w); err != nil {
			return err
		}
		if w.Status.Phase != "Running" {
			return fmt.Errorf("phase=%q, want Running", w.Status.Phase)
		}
		return nil
	})

	// Verify the LE manager's reconciler was called, OR the global suite
	// manager reconciled it (both watch Workers on the same apiserver).
	// The key assertion is that leader election succeeded and the Worker
	// reached Running — detailed exclusivity is tested in TwoInstances test.
	provCount, _, _, _ := prov.CallCounts()
	globalProvCount, _, _, _ := mockProv.CallCounts()
	if provCount == 0 && globalProvCount == 0 {
		t.Error("no reconciler provisioned the worker — something is broken")
	}
	t.Logf("LE manager provision calls: %d, global manager provision calls: %d", provCount, globalProvCount)
}

// TestLeaderElection_TwoInstances_OnlyOneReconciles starts two controller
// managers competing for the same lease and verifies that only the elected
// leader performs reconciliation.
func TestLeaderElection_TwoInstances_OnlyOneReconciles(t *testing.T) {
	leaseID := fixtures.UniqueName("le-dual")
	mgrCtxA, mgrCancelA := context.WithCancel(ctx)
	defer mgrCancelA()
	mgrCtxB, mgrCancelB := context.WithCancel(ctx)
	defer mgrCancelB()

	provA := mocks.NewMockProvisioner()
	deployA := mocks.NewMockDeployer()
	beA := mocks.NewMockWorkerBackend()
	envA := mocks.NewMockEnvBuilder()

	provB := mocks.NewMockProvisioner()
	deployB := mocks.NewMockDeployer()
	beB := mocks.NewMockWorkerBackend()
	envB := mocks.NewMockEnvBuilder()

	mgrA, err := ctrl.NewManager(restCfg, leaderElectionOpts(leaseID))
	if err != nil {
		t.Fatalf("failed to create manager A: %v", err)
	}
	mgrB, err := ctrl.NewManager(restCfg, leaderElectionOpts(leaseID))
	if err != nil {
		t.Fatalf("failed to create manager B: %v", err)
	}

	recA := &controller.WorkerReconciler{
		Client:         mgrA.GetClient(),
		Provisioner:    provA,
		Deployer:       deployA,
		Backend:        backend.NewRegistry([]backend.WorkerBackend{beA}),
		EnvBuilder:     envA,
		ControllerName: "test-ctl-a",
	}
	if _, err := recA.SetupWithManager(mgrA); err != nil {
		t.Fatalf("setup reconciler A: %v", err)
	}

	recB := &controller.WorkerReconciler{
		Client:         mgrB.GetClient(),
		Provisioner:    provB,
		Deployer:       deployB,
		Backend:        backend.NewRegistry([]backend.WorkerBackend{beB}),
		EnvBuilder:     envB,
		ControllerName: "test-ctl-b",
	}
	if _, err := recB.SetupWithManager(mgrB); err != nil {
		t.Fatalf("setup reconciler B: %v", err)
	}

	go func() {
		if err := mgrA.Start(mgrCtxA); err != nil {
			t.Logf("manager A exited: %v", err)
		}
	}()
	go func() {
		if err := mgrB.Start(mgrCtxB); err != nil {
			t.Logf("manager B exited: %v", err)
		}
	}()

	// Wait for at least one to be elected
	select {
	case <-mgrA.Elected():
	case <-mgrB.Elected():
	case <-time.After(leaderElectionTimeout):
		t.Fatal("timed out waiting for any leader election")
	}

	// Small grace period for non-leader to stabilize
	time.Sleep(2 * time.Second)

	workerName := fixtures.UniqueName("le-dual-w")
	worker := fixtures.NewTestWorker(workerName)
	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("failed to create Worker CR: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, worker) })

	assertEventually(t, func() error {
		var w v1beta1.Worker
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &w); err != nil {
			return err
		}
		if w.Status.Phase != "Running" {
			return fmt.Errorf("phase=%q, want Running", w.Status.Phase)
		}
		return nil
	})

	provCountA, _, _, _ := provA.CallCounts()
	provCountB, _, _, _ := provB.CallCounts()
	globalProvCount, _, _, _ := mockProv.CallCounts()

	// The core leader-election guarantee under test: of the two LE-enabled
	// managers competing for the same lease, both must NOT reconcile.
	if provCountA > 0 && provCountB > 0 {
		t.Errorf("both LE instances provisioned: A=%d, B=%d — only leader should reconcile", provCountA, provCountB)
	}

	// Liveness check: someone must have reconciled the Worker to Running.
	// We don't require A or B specifically, because the suite-wide non-LE
	// manager (see suite_test.go) shares the same apiserver and cache and
	// will frequently grab the work first; that race is not what this test
	// is validating.
	if provCountA == 0 && provCountB == 0 && globalProvCount == 0 {
		t.Error("no reconciler provisioned the worker — something is broken")
	}

	t.Logf("provision calls: A=%d, B=%d, global=%d", provCountA, provCountB, globalProvCount)
}

// TestLeaderElection_InitializerRunsOnlyOnLeader verifies that a post-election
// callback (simulating the Initializer) runs exactly once across two competing
// controller instances.
func TestLeaderElection_InitializerRunsOnlyOnLeader(t *testing.T) {
	leaseID := fixtures.UniqueName("le-init")
	mgrCtxA, mgrCancelA := context.WithCancel(ctx)
	defer mgrCancelA()
	mgrCtxB, mgrCancelB := context.WithCancel(ctx)
	defer mgrCancelB()

	var initCounter atomic.Int32

	mgrA, err := ctrl.NewManager(restCfg, leaderElectionOpts(leaseID))
	if err != nil {
		t.Fatalf("failed to create manager A: %v", err)
	}
	mgrB, err := ctrl.NewManager(restCfg, leaderElectionOpts(leaseID))
	if err != nil {
		t.Fatalf("failed to create manager B: %v", err)
	}

	// Simulate Initializer: increment counter after election
	go func() {
		<-mgrA.Elected()
		initCounter.Add(1)
	}()
	go func() {
		<-mgrB.Elected()
		initCounter.Add(1)
	}()

	go func() {
		if err := mgrA.Start(mgrCtxA); err != nil {
			t.Logf("manager A exited: %v", err)
		}
	}()
	go func() {
		if err := mgrB.Start(mgrCtxB); err != nil {
			t.Logf("manager B exited: %v", err)
		}
	}()

	// Wait for at least one election, then allow time for the other to
	// potentially (incorrectly) also fire.
	select {
	case <-mgrA.Elected():
	case <-mgrB.Elected():
	case <-time.After(leaderElectionTimeout):
		t.Fatal("timed out waiting for any leader election")
	}

	// Wait long enough that if the non-leader were to incorrectly fire,
	// it would have done so by now.
	time.Sleep(5 * time.Second)

	count := initCounter.Load()
	if count != 1 {
		t.Errorf("initCounter=%d, want 1 — Initializer should run exactly once", count)
	}
}
