//go:build integration

package controller_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"github.com/hiclaw/hiclaw-controller/test/testutil/fixtures"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ---------------------------------------------------------------------------
// Manager Create tests
// ---------------------------------------------------------------------------

func TestManagerCreate_HappyPath(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-create")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Running" {
			return fmt.Errorf("phase=%q, want Running", m.Status.Phase)
		}
		return nil
	})

	var m v1beta1.Manager
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
		t.Fatalf("failed to get Manager: %v", err)
	}

	if m.Status.ObservedGeneration != m.Generation {
		t.Errorf("ObservedGeneration=%d, want %d", m.Status.ObservedGeneration, m.Generation)
	}
	if m.Status.MatrixUserID == "" {
		t.Error("MatrixUserID should be set after creation")
	}
	if m.Status.RoomID == "" {
		t.Error("RoomID should be set after creation")
	}
	provCount, _, _, _ := mockMgrProv.CallCounts()
	if provCount == 0 {
		t.Error("ProvisionManager should have been called")
	}
	_, deployConfigCount, _, _ := mockMgrDeploy.CallCounts()
	if deployConfigCount == 0 {
		t.Error("DeployManagerConfig should have been called")
	}
}

func TestManagerCreate_ProvisionFailure_SetsFailedPhase(t *testing.T) {
	resetManagerMocks()

	mockMgrProv.ProvisionManagerFn = func(_ context.Context, _ service.ManagerProvisionRequest) (*service.ManagerProvisionResult, error) {
		return nil, fmt.Errorf("simulated provision failure")
	}

	mgrName := fixtures.UniqueName("test-mgr-fail")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Failed" {
			return fmt.Errorf("phase=%q, want Failed", m.Status.Phase)
		}
		if m.Status.Message == "" {
			return fmt.Errorf("message should contain failure reason")
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Manager Delete tests
// ---------------------------------------------------------------------------

func TestManagerDelete_CleansUpAll(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-delete")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}

	waitForManagerRunning(t, mgr)

	mockMgrProv.ClearCalls()
	mockMgrDeploy.ClearCalls()

	if err := k8sClient.Delete(ctx, mgr); err != nil {
		t.Fatalf("failed to delete Manager CR: %v", err)
	}

	assertEventually(t, func() error {
		var m v1beta1.Manager
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m)
		if err == nil {
			return fmt.Errorf("manager still exists (phase=%q)", m.Status.Phase)
		}
		return client.IgnoreNotFound(err)
	})

	_, deprovCount, _, leaveRoomsCount := mockMgrProv.CallCounts()
	if leaveRoomsCount == 0 {
		t.Error("LeaveAllManagerRooms should have been called")
	}
	if deprovCount == 0 {
		t.Error("DeprovisionManager should have been called")
	}
	_, _, _, cleanupCount := mockMgrDeploy.CallCounts()
	if cleanupCount == 0 {
		t.Error("CleanupOSSData should have been called")
	}
}

// ---------------------------------------------------------------------------
// Manager Finalizer test
// ---------------------------------------------------------------------------

func TestManagerFinalizer_AddedOnCreate(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-fin")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		for _, f := range m.Finalizers {
			if f == "hiclaw.io/cleanup" {
				return nil
			}
		}
		return fmt.Errorf("finalizer hiclaw.io/cleanup not found in %v", m.Finalizers)
	})
}

// ---------------------------------------------------------------------------
// Manager Update test
// ---------------------------------------------------------------------------

func TestManagerUpdate_SpecChange_RecreatesContainer(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-update")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	mockMgrBackend.Reset()
	mockMgrBackend.StatusFn = func(_ context.Context, _ string) (*backend.WorkerResult, error) {
		return &backend.WorkerResult{Status: backend.StatusRunning}, nil
	}

	updateManagerSpecField(t, mgr, func(m *v1beta1.Manager) {
		m.Spec.Model = "claude-sonnet-4-20250514"
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.ObservedGeneration != m.Generation {
			return fmt.Errorf("ObservedGeneration=%d, want %d", m.Status.ObservedGeneration, m.Generation)
		}
		return nil
	})

	creates, deletes, _, _, _ := mockMgrBackend.CallSnapshot()
	if len(deletes) == 0 {
		t.Error("backend.Delete should have been called to remove old container")
	}
	if len(creates) == 0 {
		t.Error("backend.Create should have been called to create new container")
	}
}

// ---------------------------------------------------------------------------
// Manager Idempotency test
// ---------------------------------------------------------------------------

func TestManagerCreate_Idempotent_NoDoubleProvision(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-idemp")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	provCountBefore, _, refreshCountBefore, _ := mockMgrProv.CallCounts()

	triggerManagerReconcile(t, mgr)

	assertEventually(t, func() error {
		_, _, refreshCount, _ := mockMgrProv.CallCounts()
		if refreshCount <= refreshCountBefore {
			return fmt.Errorf("RefreshManagerCredentials count=%d, want >%d",
				refreshCount, refreshCountBefore)
		}
		return nil
	})

	provCountAfter, _, _, _ := mockMgrProv.CallCounts()
	if provCountAfter != provCountBefore {
		t.Errorf("ProvisionManager called %d times, want %d (should not re-provision after Running)",
			provCountAfter, provCountBefore)
	}
}

// ---------------------------------------------------------------------------
// Manager Lifecycle state change tests
// ---------------------------------------------------------------------------

func TestManagerStateChange_StopAndResume(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-stop")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	// Running -> Stopped
	mockMgrBackend.ClearCalls()

	updateManagerSpecField(t, mgr, func(m *v1beta1.Manager) {
		m.Spec.State = ptrString("Stopped")
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Stopped" {
			return fmt.Errorf("phase=%q, want Stopped", m.Status.Phase)
		}
		return nil
	})

	_, deletes, _, stops, _ := mockMgrBackend.CallSnapshot()
	if len(stops)+len(deletes) == 0 {
		t.Error("backend.Stop or Delete should have been called when transitioning to Stopped")
	}

	// Stopped -> Running
	mockMgrBackend.ClearCalls()

	updateManagerSpecField(t, mgr, func(m *v1beta1.Manager) {
		m.Spec.State = nil
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Running" {
			return fmt.Errorf("phase=%q, want Running", m.Status.Phase)
		}
		return nil
	})

	creates, _, _, _, _ := mockMgrBackend.CallSnapshot()
	if len(creates) == 0 {
		t.Error("backend.Create should have been called when resuming from Stopped")
	}
}

// ---------------------------------------------------------------------------
// Manager Delete of failed manager
// ---------------------------------------------------------------------------

func TestManagerDelete_ProvisionFailed_StillCleans(t *testing.T) {
	resetManagerMocks()

	mockMgrProv.ProvisionManagerFn = func(_ context.Context, _ service.ManagerProvisionRequest) (*service.ManagerProvisionResult, error) {
		return nil, fmt.Errorf("simulated provision failure")
	}

	mgrName := fixtures.UniqueName("test-mgr-delfail")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Failed" {
			return fmt.Errorf("phase=%q, want Failed", m.Status.Phase)
		}
		return nil
	})

	mockMgrProv.ClearCalls()
	mockMgrDeploy.ClearCalls()

	if err := k8sClient.Delete(ctx, mgr); err != nil {
		t.Fatalf("failed to delete Manager CR: %v", err)
	}

	assertEventually(t, func() error {
		var m v1beta1.Manager
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m)
		if err == nil {
			return fmt.Errorf("manager still exists (phase=%q)", m.Status.Phase)
		}
		return client.IgnoreNotFound(err)
	})

	_, deprovCount, _, _ := mockMgrProv.CallCounts()
	if deprovCount == 0 {
		t.Error("DeprovisionManager should have been called even for a failed manager")
	}
	_, _, _, cleanupCount := mockMgrDeploy.CallCounts()
	if cleanupCount == 0 {
		t.Error("CleanupOSSData should have been called even for a failed manager")
	}
}

// ---------------------------------------------------------------------------
// Manager no infinite recreate loop
// ---------------------------------------------------------------------------

func TestManagerUpdate_NoInfiniteRecreate(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-noloop")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	mockMgrBackend.ClearCalls()

	updateManagerSpecField(t, mgr, func(m *v1beta1.Manager) {
		m.Spec.Model = "gpt-4o-mini"
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.ObservedGeneration != m.Generation {
			return fmt.Errorf("ObservedGeneration=%d, want %d", m.Status.ObservedGeneration, m.Generation)
		}
		return nil
	})

	time.Sleep(3 * time.Second)

	creates, _, _, _, _ := mockMgrBackend.CallSnapshot()
	if len(creates) == 0 {
		t.Error("expected at least 1 Create from spec update")
	}
	if len(creates) > 2 {
		t.Errorf("Create called %d times -- possible infinite recreate loop (want <=2)", len(creates))
	}
}

// ---------------------------------------------------------------------------
// Manager Sleeping lifecycle test
// ---------------------------------------------------------------------------

func TestManagerStateChange_SleepAndWake(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-sleep")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	// --- Running -> Sleeping ---
	mockMgrBackend.ClearCalls()

	updateManagerSpecField(t, mgr, func(m *v1beta1.Manager) {
		m.Spec.State = ptrString("Sleeping")
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Sleeping" {
			return fmt.Errorf("phase=%q, want Sleeping", m.Status.Phase)
		}
		return nil
	})

	_, _, _, stops, _ := mockMgrBackend.CallSnapshot()
	if len(stops) == 0 {
		t.Error("backend.Stop should have been called when transitioning to Sleeping")
	}

	// --- Sleeping -> Running ---
	mockMgrBackend.ClearCalls()

	updateManagerSpecField(t, mgr, func(m *v1beta1.Manager) {
		m.Spec.State = nil
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Running" {
			return fmt.Errorf("phase=%q, want Running", m.Status.Phase)
		}
		return nil
	})

	creates, _, _, _, _ := mockMgrBackend.CallSnapshot()
	if len(creates) == 0 {
		t.Error("backend.Create should have been called when waking from Sleeping")
	}
}

// ---------------------------------------------------------------------------
// Manager Pod deleted recreates test
// ---------------------------------------------------------------------------

func TestManagerPodDeleted_Recreates(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-poddel")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	// Simulate external pod deletion via the mock's automatic state tracking.
	// The ContainerName alias (Issue #1 fix) means "hiclaw-manager" is tracked
	// alongside req.Name, so SimulatePodDeletion works for Manager now.
	containerName := managerContainerName(mgrName)
	mockMgrBackend.SimulatePodDeletion(containerName)
	mockMgrBackend.ClearCalls()

	triggerManagerReconcile(t, mgr)

	assertEventually(t, func() error {
		creates, _, _, _, _ := mockMgrBackend.CallSnapshot()
		if len(creates) == 0 {
			return fmt.Errorf("waiting for backend.Create to be called (pod recreation)")
		}
		return nil
	})

	var m v1beta1.Manager
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
		t.Fatalf("failed to get Manager: %v", err)
	}
	if m.Status.Phase != "Running" {
		t.Errorf("phase=%q after pod recreation, want Running", m.Status.Phase)
	}
}

// ---------------------------------------------------------------------------
// Manager simultaneous spec + state change test
// ---------------------------------------------------------------------------

func TestManagerStateChange_SimultaneousSpecAndState(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-simul")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	// --- Simultaneously change state to Stopped AND model ---
	mockMgrBackend.ClearCalls()

	updateManagerSpecField(t, mgr, func(m *v1beta1.Manager) {
		m.Spec.State = ptrString("Stopped")
		m.Spec.Model = "claude-sonnet-4-20250514"
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Stopped" {
			return fmt.Errorf("phase=%q, want Stopped", m.Status.Phase)
		}
		return nil
	})

	creates, _, _, _, _ := mockMgrBackend.CallSnapshot()
	if len(creates) > 0 {
		t.Errorf("backend.Create called %d times while Stopped -- should not create in Stopped state", len(creates))
	}

	// --- Resume to Running with new config ---
	mockMgrBackend.ClearCalls()

	updateManagerSpecField(t, mgr, func(m *v1beta1.Manager) {
		m.Spec.State = nil
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Running" {
			return fmt.Errorf("phase=%q, want Running", m.Status.Phase)
		}
		return nil
	})

	creates, _, _, _, _ = mockMgrBackend.CallSnapshot()
	if len(creates) == 0 {
		t.Error("backend.Create should have been called when resuming with new config")
	}
}

// ---------------------------------------------------------------------------
// Manager error path tests
// ---------------------------------------------------------------------------

func TestManagerCreate_ConfigDeployFailure_KeepsPhase(t *testing.T) {
	resetManagerMocks()

	mockMgrDeploy.DeployManagerConfigFn = func(_ context.Context, _ service.ManagerDeployRequest) error {
		return fmt.Errorf("simulated config deploy failure")
	}

	mgrName := fixtures.UniqueName("test-mgr-cfgfail")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Message == "" {
			return fmt.Errorf("message should contain failure reason")
		}
		return nil
	})

	var m v1beta1.Manager
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
		t.Fatalf("failed to get Manager: %v", err)
	}

	if m.Status.MatrixUserID == "" {
		t.Error("MatrixUserID should be set (provision succeeded before config failure)")
	}
	if m.Status.Phase != "Pending" {
		t.Errorf("Phase=%q, want Pending (infra provisioned but config failed)", m.Status.Phase)
	}
	if m.Status.ObservedGeneration != 0 {
		t.Errorf("ObservedGeneration=%d, want 0 (should not be written on error)", m.Status.ObservedGeneration)
	}
}

func TestManagerCreate_ContainerCreateFailure_ReturnsError(t *testing.T) {
	resetManagerMocks()

	mockMgrBackend.CreateFn = func(_ context.Context, _ backend.CreateRequest) (*backend.WorkerResult, error) {
		return nil, fmt.Errorf("simulated container create failure")
	}

	mgrName := fixtures.UniqueName("test-mgr-ctrfail")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Message == "" {
			return fmt.Errorf("message should contain failure reason")
		}
		return nil
	})

	var m v1beta1.Manager
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
		t.Fatalf("failed to get Manager: %v", err)
	}

	if m.Status.MatrixUserID == "" {
		t.Error("MatrixUserID should be set (infra+config succeeded before container failure)")
	}
	if m.Status.Phase == "Running" {
		t.Error("Phase should not be Running when container creation failed")
	}
}

func TestManagerCreate_ServiceAccountFailure_RetriesOnNextReconcile(t *testing.T) {
	resetManagerMocks()

	saCallCount := 0
	mockMgrProv.EnsureManagerServiceAccountFn = func(_ context.Context, _ string) error {
		saCallCount++
		if saCallCount <= 1 {
			return fmt.Errorf("simulated SA creation failure")
		}
		return nil
	}

	mgrName := fixtures.UniqueName("test-mgr-safail")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	// SA fails on first reconcile, succeeds on retry → Manager reaches Running.
	waitForManagerRunning(t, mgr)

	ensureSA, _ := mockMgrProv.ServiceAccountCallCounts()
	if ensureSA < 2 {
		t.Errorf("EnsureManagerServiceAccount called %d times, want >=2 (initial failure + retry)", ensureSA)
	}
}

func TestManagerUpdate_RefreshCredentialsFail_KeepsPhase(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-reffail")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	// Switch RefreshManagerCredentials to fail
	mockMgrProv.RefreshManagerCredentialsFn = func(_ context.Context, _ string) (*service.RefreshResult, error) {
		return nil, fmt.Errorf("simulated refresh failure")
	}

	triggerManagerReconcile(t, mgr)

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Message == "" {
			return fmt.Errorf("message should contain refresh failure")
		}
		return nil
	})

	var m v1beta1.Manager
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
		t.Fatalf("failed to get Manager: %v", err)
	}

	if m.Status.Phase != "Running" {
		t.Errorf("Phase=%q, want Running (should keep original phase on refresh failure)", m.Status.Phase)
	}
}

// ---------------------------------------------------------------------------
// Manager delete resilience test
// ---------------------------------------------------------------------------

func TestManagerDelete_PartialFailure_StillCompletes(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-delprt")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}

	waitForManagerRunning(t, mgr)

	// Make cleanup operations fail
	mockMgrProv.DeprovisionManagerFn = func(_ context.Context, _ string) error {
		return fmt.Errorf("simulated deprovision failure")
	}
	mockMgrDeploy.CleanupOSSDataFn = func(_ context.Context, _ string) error {
		return fmt.Errorf("simulated OSS cleanup failure")
	}
	mockMgrProv.DeleteCredentialsFn = func(_ context.Context, _ string) error {
		return fmt.Errorf("simulated credential delete failure")
	}

	if err := k8sClient.Delete(ctx, mgr); err != nil {
		t.Fatalf("failed to delete Manager CR: %v", err)
	}

	assertEventually(t, func() error {
		var m v1beta1.Manager
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m)
		if err == nil {
			return fmt.Errorf("manager still exists (phase=%q)", m.Status.Phase)
		}
		return client.IgnoreNotFound(err)
	})
}

// ---------------------------------------------------------------------------
// Manager MCP reauthorization test
// ---------------------------------------------------------------------------

func TestManagerUpdate_MCPServersChange_RewritesMcporterJSON(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-mcp")
	mgr := fixtures.NewTestManagerWithMCPServers(mgrName, []v1beta1.MCPServer{
		{Name: "github", URL: "https://gw.example.com/mcp-servers/github/mcp"},
	})

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	mockMgrProv.ClearCalls()
	mockMgrDeploy.ClearCalls()

	updatedServers := []v1beta1.MCPServer{
		{Name: "github", URL: "https://gw.example.com/mcp-servers/github/mcp"},
		{Name: "jira", URL: "https://gw.example.com/mcp-servers/jira/mcp", Transport: "sse"},
	}
	updateManagerSpecField(t, mgr, func(m *v1beta1.Manager) {
		m.Spec.McpServers = updatedServers
	})

	// The CRD defaults mcpServers[].transport to "http" server-side, so the
	// value the reconciler observes for an entry with an empty Transport is
	// "http". Normalize the expectation to match.
	expectedServers := append([]v1beta1.MCPServer(nil), updatedServers...)
	for i := range expectedServers {
		if expectedServers[i].Transport == "" {
			expectedServers[i].Transport = "http"
		}
	}

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.ObservedGeneration != m.Generation {
			return fmt.Errorf("ObservedGeneration=%d, want %d", m.Status.ObservedGeneration, m.Generation)
		}
		// Require Deployer to have been called with the updated McpServers.
		for _, req := range mockMgrDeploy.Calls.DeployManagerConfig {
			if req.Name != mgrName {
				continue
			}
			if len(req.McpServers) == len(expectedServers) {
				match := true
				for i, s := range expectedServers {
					if req.McpServers[i].Name != s.Name || req.McpServers[i].URL != s.URL || req.McpServers[i].Transport != s.Transport {
						match = false
						break
					}
				}
				if match {
					return nil
				}
			}
		}
		return fmt.Errorf("DeployManagerConfig not called with updated McpServers=%v (calls=%d)", expectedServers, len(mockMgrDeploy.Calls.DeployManagerConfig))
	})
}

// ---------------------------------------------------------------------------
// spec.env propagation
// ---------------------------------------------------------------------------

func TestManagerCreate_EnvPassesToBackend(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-env")
	mgr := fixtures.NewTestManager(mgrName)
	mgr.Spec.Env = map[string]string{
		"USER_MGR":   "hello",
		"USER_EMPTY": "",
		// System-wins: HICLAW_MANAGER_NAME is produced by MockManagerEnvBuilder
		// and must override this user-supplied value.
		"HICLAW_MANAGER_NAME": "user-should-lose",
	}

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	req, ok := mockMgrBackend.LastCreateReq()
	if !ok {
		t.Fatalf("no CreateRequest recorded for manager %q", mgrName)
	}
	if got := req.Env["USER_MGR"]; got != "hello" {
		t.Errorf("USER_MGR=%q, want %q", got, "hello")
	}
	if got, present := req.Env["USER_EMPTY"]; !present || got != "" {
		t.Errorf("USER_EMPTY present=%v value=%q, want present=true value=\"\"", present, got)
	}
	if got := req.Env["HICLAW_MANAGER_NAME"]; got != mgrName {
		t.Errorf("HICLAW_MANAGER_NAME=%q, want %q (system wins)", got, mgrName)
	}
	if got := req.Env["MOCK_ENV"]; got != "true" {
		t.Errorf("MOCK_ENV=%q, want %q (system env preserved)", got, "true")
	}
}

// ---------------------------------------------------------------------------
// Manager Welcome (first-boot onboarding) tests
// ---------------------------------------------------------------------------

// TestManagerWelcome_HappyPath_SendsOnce verifies that once the manager
// container reports Running, the controller calls SendManagerWelcome
// exactly once and persists Status.WelcomeSent=true. Forwarded
// language/timezone must reflect the values wired from controller config
// (see suite_test.go: en / America/Los_Angeles).
func TestManagerWelcome_HappyPath_SendsOnce(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-welcome-happy")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if !m.Status.WelcomeSent {
			return fmt.Errorf("Status.WelcomeSent=false, want true after Running")
		}
		return nil
	})

	calls := mockMgrProv.WelcomeCallsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("SendManagerWelcome called %d times, want exactly 1", len(calls))
	}
	got := calls[0]
	if got.Language != "en" {
		t.Errorf("welcome request Language=%q, want %q (forwarded from controller config)", got.Language, "en")
	}
	if got.Timezone != "America/Los_Angeles" {
		t.Errorf("welcome request Timezone=%q, want %q (forwarded from controller config)", got.Timezone, "America/Los_Angeles")
	}
	if got.RoomID == "" {
		t.Error("welcome request RoomID is empty, want the provisioned admin DM room")
	}
}

// TestManagerWelcome_Idempotent_NoResendOnReconcile verifies the
// WelcomeSent guard: after the first send, subsequent reconciles must
// NOT call SendManagerWelcome again — even if the manager container
// restarts or a spec change re-triggers the loop.
func TestManagerWelcome_Idempotent_NoResendOnReconcile(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-welcome-idemp")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)
	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if !m.Status.WelcomeSent {
			return fmt.Errorf("WelcomeSent not yet true")
		}
		return nil
	})

	mockMgrProv.ClearWelcomeCalls()

	triggerManagerReconcile(t, mgr)

	// Wait for at least one full reconcile to happen by observing
	// RefreshManagerCredentials being called (it runs on every Running
	// reconcile path).
	assertEventually(t, func() error {
		_, _, refreshCount, _ := mockMgrProv.CallCounts()
		if refreshCount == 0 {
			return fmt.Errorf("no reconcile observed yet")
		}
		return nil
	})

	if resends := mockMgrProv.WelcomeCallCount(); resends != 0 {
		t.Errorf("SendManagerWelcome called %d times after WelcomeSent=true, want 0 (must be idempotent)", resends)
	}
}

// TestManagerWelcome_NotJoinedYet_RequeuesUntilJoined exercises the
// pre-claim membership-poll branch: IsManagerJoinedDM reports false on
// the first two attempts (manager hasn't auto-joined the DM room yet),
// then true. SendManagerWelcomeMessage must not be called until the
// membership check passes, Status.WelcomeSent must only flip on the
// successful path, and the controller must NOT touch status while
// waiting (no claim/rollback churn).
func TestManagerWelcome_NotJoinedYet_RequeuesUntilJoined(t *testing.T) {
	resetManagerMocks()

	var joinChecks atomic.Int32
	mockMgrProv.IsManagerJoinedDMFn = func(_ context.Context, _ string) (bool, error) {
		n := joinChecks.Add(1)
		return n >= 3, nil
	}

	mgrName := fixtures.UniqueName("test-mgr-welcome-wait")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if !m.Status.WelcomeSent {
			return fmt.Errorf("WelcomeSent=false, joinChecks=%d sends=%d (want true after the membership poll passes)",
				joinChecks.Load(), mockMgrProv.WelcomeCallCount())
		}
		return nil
	})

	if got := joinChecks.Load(); got < 3 {
		t.Errorf("IsManagerJoinedDM called %d times, want >=3 (waited at least 2 polls before membership landed)", got)
	}
	if sends := mockMgrProv.WelcomeCallCount(); sends != 1 {
		t.Errorf("SendManagerWelcomeMessage called %d times, want exactly 1 (membership-poll branch must not pre-emptively send)", sends)
	}

	// Status must remain Running — the requeue path is non-fatal.
	var m v1beta1.Manager
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
		t.Fatalf("failed to get Manager: %v", err)
	}
	if m.Status.Phase != "Running" {
		t.Errorf("Phase=%q, want Running (welcome requeue must not flip to Failed)", m.Status.Phase)
	}
}

// TestManagerWelcome_LLMAuthNotReady_RequeuesUntilPropagated exercises
// the second readiness gate: even when the manager has joined the DM
// room, the controller must hold off on SendManagerWelcomeMessage until
// IsManagerLLMAuthReady reports true. This guards against the
// well-known Higress WASM key-auth propagation lag (~40-45s on first
// install) — sending the welcome too early lands a prompt the manager
// receives but cannot reply to (its first /v1/chat/completions call
// 401s against the gateway).
//
// The mock keeps IsManagerJoinedDM=true throughout so the test isolates
// the auth-readiness branch; IsManagerLLMAuthReady starts returning
// false and flips to true after a few polls. The controller must
// requeue (no claim, no send) until the auth signal flips, then send
// exactly once.
func TestManagerWelcome_LLMAuthNotReady_RequeuesUntilPropagated(t *testing.T) {
	resetManagerMocks()

	var authChecks atomic.Int32
	mockMgrProv.IsManagerLLMAuthReadyFn = func(_ context.Context, _ string) (bool, error) {
		n := authChecks.Add(1)
		return n >= 3, nil
	}

	mgrName := fixtures.UniqueName("test-mgr-welcome-auth")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if !m.Status.WelcomeSent {
			return fmt.Errorf("WelcomeSent=false, authChecks=%d sends=%d (want true after auth propagates)",
				authChecks.Load(), mockMgrProv.WelcomeCallCount())
		}
		return nil
	})

	if got := authChecks.Load(); got < 3 {
		t.Errorf("IsManagerLLMAuthReady called %d times, want >=3 (waited at least 2 polls before auth propagated)", got)
	}
	if sends := mockMgrProv.WelcomeCallCount(); sends != 1 {
		t.Errorf("SendManagerWelcomeMessage called %d times, want exactly 1 (auth-readiness gate must not pre-emptively send)", sends)
	}

	var m v1beta1.Manager
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
		t.Fatalf("failed to get Manager: %v", err)
	}
	if m.Status.Phase != "Running" {
		t.Errorf("Phase=%q, want Running (welcome requeue must not flip to Failed)", m.Status.Phase)
	}
}

// ---------------------------------------------------------------------------
// Manager test helpers
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// CR Labels → Pod Labels propagation
// ---------------------------------------------------------------------------

// TestManagerLabels_PropagateFromMetadataAndSpecToBackendCreate walks
// the Manager reconcile pipeline end-to-end and asserts the labels
// handed to backend.Create reflect the four-layer merge: CR metadata
// and spec labels both propagate, spec beats metadata on collision,
// and controller-forced system labels (app, hiclaw.io/controller,
// hiclaw.io/manager, hiclaw.io/role, hiclaw.io/runtime) override any
// reserved keys a user stuffed into either layer.
func TestManagerLabels_PropagateFromMetadataAndSpecToBackendCreate(t *testing.T) {
	resetManagerMocks()

	capMu := newLabelCapture()
	mockMgrBackend.CreateFn = capMu.CreateFn()

	mgrName := fixtures.UniqueName("labels-mgr")
	mgr := fixtures.NewTestManager(mgrName)
	mgr.ObjectMeta.Labels = map[string]string{
		"owner": "alice",
		"tier":  "metadata-tier",
	}
	mgr.Spec.Labels = map[string]string{
		"env":                   "prod",
		"tier":                  "spec-tier", // overrides metadata
		v1beta1.LabelController: "spec-attacker",
		"app":                   "spoofed-app",
	}

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("create Manager: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, mgr) })

	waitForManagerRunning(t, mgr)

	// Manager containers are named via managerContainerName(name), which
	// is what the backend sees in req.Name.
	containerName := managerContainerName(mgrName)
	labels := capMu.LabelsFor(containerName)
	if labels == nil {
		// Fall back to capturing by CR name for robustness across
		// prefix changes.
		labels = capMu.LabelsFor(mgrName)
	}
	if labels == nil {
		t.Fatalf("backend Create was never called (captured=%v)", capMu.Keys())
	}

	assertLabel(t, labels, "owner", "alice")
	assertLabel(t, labels, "env", "prod")
	assertLabel(t, labels, "tier", "spec-tier")
	assertLabel(t, labels, v1beta1.LabelController, "test-ctl")
	assertLabel(t, labels, "hiclaw.io/manager", mgrName)
	assertLabel(t, labels, "hiclaw.io/role", "manager")
	// app label must be the ResourcePrefix-derived value, not the
	// user-supplied "spoofed-app"; ManagerReconciler wired in suite
	// construction uses the default (empty) ResourcePrefix which maps
	// to "hiclaw-manager".
	if got := labels["app"]; got == "spoofed-app" || got == "" {
		t.Errorf("app label not set by controller (got %q, full=%v)", got, labels)
	}
}

// TestManagerLabels_MetadataLabelsChangeDoesNotRecreatePod verifies
// the same non-disruption contract as the Worker equivalent: patching
// only metadata.labels must NOT trigger backend.Delete/Create.
func TestManagerLabels_MetadataLabelsChangeDoesNotRecreatePod(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("labels-mgr-noop")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("create Manager: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, mgr) })

	waitForManagerRunning(t, mgr)

	createsBefore, deletesBefore, _, _, _ := mockMgrBackend.CallSnapshot()

	updateManagerSpecField(t, mgr, func(got *v1beta1.Manager) {
		if got.ObjectMeta.Labels == nil {
			got.ObjectMeta.Labels = map[string]string{}
		}
		got.ObjectMeta.Labels["newly-added"] = "v1"
	})
	triggerManagerReconcile(t, mgr)
	time.Sleep(2 * time.Second)

	createsAfter, deletesAfter, _, _, _ := mockMgrBackend.CallSnapshot()
	if len(createsAfter) != len(createsBefore) {
		t.Errorf("metadata.labels change triggered backend.Create: before=%d after=%d", len(createsBefore), len(createsAfter))
	}
	if len(deletesAfter) != len(deletesBefore) {
		t.Errorf("metadata.labels change triggered backend.Delete: before=%d after=%d", len(deletesBefore), len(deletesAfter))
	}
}

// managerContainerName mirrors the controller's naming logic for tests.
func managerContainerName(name string) string {
	if name == "default" {
		return "hiclaw-manager"
	}
	return "hiclaw-manager-" + name
}

func waitForManagerRunning(t *testing.T, mgr *v1beta1.Manager) {
	t.Helper()
	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Running" {
			return fmt.Errorf("phase=%q message=%q gen=%d obsGen=%d, want Running",
				m.Status.Phase, m.Status.Message, m.Generation, m.Status.ObservedGeneration)
		}
		return nil
	})
}

func updateManagerSpecField(t *testing.T, mgr *v1beta1.Manager, mutate func(m *v1beta1.Manager)) {
	t.Helper()
	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		mutate(&m)
		return k8sClient.Update(ctx, &m)
	})
}

func triggerManagerReconcile(t *testing.T, mgr *v1beta1.Manager) {
	t.Helper()
	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Annotations == nil {
			m.Annotations = map[string]string{}
		}
		m.Annotations["hiclaw.io/reconcile-trigger"] = fmt.Sprintf("%d", time.Now().UnixNano())
		return k8sClient.Update(ctx, &m)
	})
}
