//go:build integration

package controller_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"github.com/hiclaw/hiclaw-controller/test/testutil/fixtures"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ---------------------------------------------------------------------------
// Existing tests (Phase 1-3)
// ---------------------------------------------------------------------------

func TestWorkerCreate_HappyPath(t *testing.T) {
	resetMocks()

	workerName := fixtures.UniqueName("test-create")
	worker := fixtures.NewTestWorker(workerName)

	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("failed to create Worker CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, worker)
	})

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

	var w v1beta1.Worker
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &w); err != nil {
		t.Fatalf("failed to get Worker: %v", err)
	}

	if w.Status.ObservedGeneration != w.Generation {
		t.Errorf("ObservedGeneration=%d, want %d", w.Status.ObservedGeneration, w.Generation)
	}
	if w.Status.MatrixUserID == "" {
		t.Error("MatrixUserID should be set after creation")
	}
	if w.Status.RoomID == "" {
		t.Error("RoomID should be set after creation")
	}
	provCount, _, _, _ := mockProv.CallCounts()
	if provCount == 0 {
		t.Error("ProvisionWorker should have been called")
	}
	_, _, deployConfigCount, _, _ := mockDeploy.CallCounts()
	if deployConfigCount == 0 {
		t.Error("DeployWorkerConfig should have been called")
	}
}

func TestWorkerCreate_ProvisionFailure_SetsFailedPhase(t *testing.T) {
	resetMocks()

	mockProv.ProvisionWorkerFn = func(_ context.Context, _ service.WorkerProvisionRequest) (*service.WorkerProvisionResult, error) {
		return nil, fmt.Errorf("simulated provision failure")
	}

	workerName := fixtures.UniqueName("test-fail")
	worker := fixtures.NewTestWorker(workerName)

	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("failed to create Worker CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, worker)
	})

	assertEventually(t, func() error {
		var w v1beta1.Worker
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &w); err != nil {
			return err
		}
		if w.Status.Phase != "Failed" {
			return fmt.Errorf("phase=%q, want Failed", w.Status.Phase)
		}
		if w.Status.Message == "" {
			return fmt.Errorf("message should contain failure reason")
		}
		return nil
	})
}

func TestWorkerDelete_CleansUpAll(t *testing.T) {
	resetMocks()

	workerName := fixtures.UniqueName("test-delete")
	worker := fixtures.NewTestWorker(workerName)

	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("failed to create Worker CR: %v", err)
	}

	waitForRunning(t, worker)

	mockProv.ClearCalls()
	mockDeploy.ClearCalls()

	if err := k8sClient.Delete(ctx, worker); err != nil {
		t.Fatalf("failed to delete Worker CR: %v", err)
	}

	assertEventually(t, func() error {
		var w v1beta1.Worker
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &w)
		if err == nil {
			return fmt.Errorf("worker still exists (phase=%q)", w.Status.Phase)
		}
		return client.IgnoreNotFound(err)
	})

	_, deprovCount, _, leaveRoomsCount := mockProv.CallCounts()
	if leaveRoomsCount == 0 {
		t.Error("LeaveAllWorkerRooms should have been called")
	}
	if deprovCount == 0 {
		t.Error("DeprovisionWorker should have been called")
	}
	_, _, _, _, cleanupCount := mockDeploy.CallCounts()
	if cleanupCount == 0 {
		t.Error("CleanupOSSData should have been called")
	}
}

func TestWorkerFinalizer_AddedOnCreate(t *testing.T) {
	resetMocks()

	workerName := fixtures.UniqueName("test-finalizer")
	worker := fixtures.NewTestWorker(workerName)

	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("failed to create Worker CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, worker)
	})

	assertEventually(t, func() error {
		var w v1beta1.Worker
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &w); err != nil {
			return err
		}
		for _, f := range w.Finalizers {
			if f == "hiclaw.io/cleanup" {
				return nil
			}
		}
		return fmt.Errorf("finalizer hiclaw.io/cleanup not found in %v", w.Finalizers)
	})
}

func TestWorkerUpdate_SpecChange_RecreatesPod(t *testing.T) {
	resetMocks()

	workerName := fixtures.UniqueName("test-update")
	worker := fixtures.NewTestWorker(workerName)

	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("failed to create Worker CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, worker)
	})

	waitForRunning(t, worker)

	// Reset fully: clear state so we can use StatusFn to simulate pre-existing pod.
	// This test predates stateful mock; keep using StatusFn for explicit control.
	mockBackend.Reset()
	mockBackend.StatusFn = func(_ context.Context, _ string) (*backend.WorkerResult, error) {
		return &backend.WorkerResult{Status: backend.StatusRunning}, nil
	}

	updateSpecField(t, worker, func(w *v1beta1.Worker) {
		w.Spec.Model = "claude-sonnet-4-20250514"
	})

	assertEventually(t, func() error {
		var w v1beta1.Worker
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &w); err != nil {
			return err
		}
		if w.Status.ObservedGeneration != w.Generation {
			return fmt.Errorf("ObservedGeneration=%d, want %d", w.Status.ObservedGeneration, w.Generation)
		}
		return nil
	})

	creates, deletes, _, _, _ := mockBackend.CallSnapshot()
	if len(deletes) == 0 {
		t.Error("backend.Delete should have been called to remove old container")
	}
	if len(creates) == 0 {
		t.Error("backend.Create should have been called to create new container")
	}
}

// ---------------------------------------------------------------------------
// Phase 4: Lifecycle state change tests
// ---------------------------------------------------------------------------

func TestWorkerStateChange_StopAndResume(t *testing.T) {
	resetMocks()

	workerName := fixtures.UniqueName("test-stop-resume")
	worker := fixtures.NewTestWorker(workerName)

	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("failed to create Worker CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, worker)
	})

	waitForRunning(t, worker)

	// --- Running → Stopped ---
	mockBackend.ClearCalls()

	updateSpecField(t, worker, func(w *v1beta1.Worker) {
		w.Spec.State = ptrString("Stopped")
	})

	assertEventually(t, func() error {
		var w v1beta1.Worker
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &w); err != nil {
			return err
		}
		if w.Status.Phase != "Stopped" {
			return fmt.Errorf("phase=%q, want Stopped", w.Status.Phase)
		}
		return nil
	})

	_, deletes, _, stops, _ := mockBackend.CallSnapshot()
	if len(stops)+len(deletes) == 0 {
		t.Error("backend.Stop or Delete should have been called when transitioning to Stopped")
	}

	// --- Stopped → Running ---
	mockBackend.ClearCalls()

	updateSpecField(t, worker, func(w *v1beta1.Worker) {
		w.Spec.State = nil
	})

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

	creates, _, _, _, _ := mockBackend.CallSnapshot()
	if len(creates) == 0 {
		t.Error("backend.Create should have been called when resuming from Stopped")
	}
}

func TestWorkerStateChange_SleepAndWake(t *testing.T) {
	resetMocks()

	workerName := fixtures.UniqueName("test-sleep-wake")
	worker := fixtures.NewTestWorker(workerName)

	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("failed to create Worker CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, worker)
	})

	waitForRunning(t, worker)

	// --- Running → Sleeping ---
	mockBackend.ClearCalls()

	updateSpecField(t, worker, func(w *v1beta1.Worker) {
		w.Spec.State = ptrString("Sleeping")
	})

	assertEventually(t, func() error {
		var w v1beta1.Worker
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &w); err != nil {
			return err
		}
		if w.Status.Phase != "Sleeping" {
			return fmt.Errorf("phase=%q, want Sleeping", w.Status.Phase)
		}
		return nil
	})

	_, _, _, stops, _ := mockBackend.CallSnapshot()
	if len(stops) == 0 {
		t.Error("backend.Stop should have been called when transitioning to Sleeping")
	}

	// --- Sleeping → Running ---
	mockBackend.ClearCalls()

	updateSpecField(t, worker, func(w *v1beta1.Worker) {
		w.Spec.State = nil
	})

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

	creates, _, _, _, _ := mockBackend.CallSnapshot()
	if len(creates) == 0 {
		t.Error("backend.Create should have been called when waking from Sleeping")
	}
}

// ---------------------------------------------------------------------------
// Phase 4: Pod resilience tests
// ---------------------------------------------------------------------------

func TestWorkerPodDeleted_Recreates(t *testing.T) {
	resetMocks()

	workerName := fixtures.UniqueName("test-pod-deleted")
	worker := fixtures.NewTestWorker(workerName)

	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("failed to create Worker CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, worker)
	})

	waitForRunning(t, worker)

	// Simulate external pod deletion
	mockBackend.SimulatePodDeletion(workerName)
	mockBackend.ClearCalls()

	triggerReconcile(t, worker)

	assertEventually(t, func() error {
		creates, _, _, _, _ := mockBackend.CallSnapshot()
		if len(creates) == 0 {
			return fmt.Errorf("waiting for backend.Create to be called (pod recreation)")
		}
		return nil
	})

	// Phase should still be Running after recreation
	var w v1beta1.Worker
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &w); err != nil {
		t.Fatalf("failed to get Worker: %v", err)
	}
	if w.Status.Phase != "Running" {
		t.Errorf("phase=%q after pod recreation, want Running", w.Status.Phase)
	}
}

// ---------------------------------------------------------------------------
// Phase 4: Idempotency and convergence tests
// ---------------------------------------------------------------------------

func TestWorkerCreate_Idempotent_NoDoubleProvision(t *testing.T) {
	resetMocks()

	workerName := fixtures.UniqueName("test-idempotent")
	worker := fixtures.NewTestWorker(workerName)

	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("failed to create Worker CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, worker)
	})

	waitForRunning(t, worker)

	provCountBefore, _, refreshCountBefore, _ := mockProv.CallCounts()

	// Trigger another reconcile via annotation update (no spec change)
	triggerReconcile(t, worker)

	// Wait for the new reconcile to complete — RefreshCredentials count increases
	assertEventually(t, func() error {
		_, _, refreshCount, _ := mockProv.CallCounts()
		if refreshCount <= refreshCountBefore {
			return fmt.Errorf("RefreshCredentials count=%d, want >%d (reconcile not triggered yet)",
				refreshCount, refreshCountBefore)
		}
		return nil
	})

	provCountAfter, _, _, _ := mockProv.CallCounts()
	if provCountAfter != provCountBefore {
		t.Errorf("ProvisionWorker called %d times, want %d (should not re-provision after Running)",
			provCountAfter, provCountBefore)
	}
}

func TestWorkerUpdate_NoInfiniteRecreate(t *testing.T) {
	resetMocks()

	workerName := fixtures.UniqueName("test-no-loop")
	worker := fixtures.NewTestWorker(workerName)

	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("failed to create Worker CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, worker)
	})

	waitForRunning(t, worker)

	mockBackend.ClearCalls()

	updateSpecField(t, worker, func(w *v1beta1.Worker) {
		w.Spec.Model = "gpt-4o-mini"
	})

	// Wait for reconcile to complete
	assertEventually(t, func() error {
		var w v1beta1.Worker
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &w); err != nil {
			return err
		}
		if w.Status.ObservedGeneration != w.Generation {
			return fmt.Errorf("ObservedGeneration=%d, want %d", w.Status.ObservedGeneration, w.Generation)
		}
		return nil
	})

	// Wait a grace period — if there's an infinite loop, Creates will keep growing
	time.Sleep(3 * time.Second)

	creates, _, _, _, _ := mockBackend.CallSnapshot()
	// Expect exactly 1 Create from the update (delete old + create new).
	// With stateful mock, after the recreate cycle Status returns Running,
	// Generation == ObservedGeneration, so no further Creates should occur.
	if len(creates) == 0 {
		t.Error("expected at least 1 Create from spec update")
	}
	if len(creates) > 2 {
		t.Errorf("Create called %d times — possible infinite recreate loop (want <=2)", len(creates))
	}
}

// ---------------------------------------------------------------------------
// Phase 4: Simultaneous state + spec change
// ---------------------------------------------------------------------------

func TestWorkerStateChange_SimultaneousSpecAndState(t *testing.T) {
	resetMocks()

	workerName := fixtures.UniqueName("test-simul")
	worker := fixtures.NewTestWorker(workerName)

	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("failed to create Worker CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, worker)
	})

	waitForRunning(t, worker)

	// --- Simultaneously change state to Stopped AND model ---
	mockBackend.ClearCalls()

	updateSpecField(t, worker, func(w *v1beta1.Worker) {
		w.Spec.State = ptrString("Stopped")
		w.Spec.Model = "claude-sonnet-4-20250514"
	})

	assertEventually(t, func() error {
		var w v1beta1.Worker
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &w); err != nil {
			return err
		}
		if w.Status.Phase != "Stopped" {
			return fmt.Errorf("phase=%q, want Stopped", w.Status.Phase)
		}
		return nil
	})

	// While Stopped, no new pod should be created despite model change
	creates, _, _, _, _ := mockBackend.CallSnapshot()
	if len(creates) > 0 {
		t.Errorf("backend.Create called %d times while Stopped — should not create pod in Stopped state", len(creates))
	}

	// --- Resume to Running with new config ---
	mockBackend.ClearCalls()

	updateSpecField(t, worker, func(w *v1beta1.Worker) {
		w.Spec.State = nil
	})

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

	creates, _, _, _, _ = mockBackend.CallSnapshot()
	if len(creates) == 0 {
		t.Error("backend.Create should have been called when resuming with new config")
	}
}

// ---------------------------------------------------------------------------
// Phase 5: ServiceAccount retry test
// ---------------------------------------------------------------------------

func TestWorkerCreate_ServiceAccountFailure_RetriesOnNextReconcile(t *testing.T) {
	resetMocks()

	saCallCount := 0
	mockProv.EnsureServiceAccountFn = func(_ context.Context, _ string) error {
		saCallCount++
		if saCallCount <= 1 {
			return fmt.Errorf("simulated SA creation failure")
		}
		return nil
	}

	workerName := fixtures.UniqueName("test-sa-retry")
	worker := fixtures.NewTestWorker(workerName)

	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("failed to create Worker CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, worker)
	})

	// SA fails on first reconcile, succeeds on retry -> Worker reaches Running.
	waitForRunning(t, worker)

	ensureSA, _ := mockProv.ServiceAccountCallCounts()
	if ensureSA < 2 {
		t.Errorf("EnsureServiceAccount called %d times, want >=2 (initial failure + retry)", ensureSA)
	}
}

// ---------------------------------------------------------------------------
// Phase 4: Deletion of a failed worker
// ---------------------------------------------------------------------------

func TestWorkerDelete_ProvisionFailed_StillCleans(t *testing.T) {
	resetMocks()

	mockProv.ProvisionWorkerFn = func(_ context.Context, _ service.WorkerProvisionRequest) (*service.WorkerProvisionResult, error) {
		return nil, fmt.Errorf("simulated provision failure")
	}

	workerName := fixtures.UniqueName("test-del-fail")
	worker := fixtures.NewTestWorker(workerName)

	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("failed to create Worker CR: %v", err)
	}

	assertEventually(t, func() error {
		var w v1beta1.Worker
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &w); err != nil {
			return err
		}
		if w.Status.Phase != "Failed" {
			return fmt.Errorf("phase=%q, want Failed", w.Status.Phase)
		}
		return nil
	})

	mockProv.ClearCalls()
	mockDeploy.ClearCalls()

	if err := k8sClient.Delete(ctx, worker); err != nil {
		t.Fatalf("failed to delete Worker CR: %v", err)
	}

	assertEventually(t, func() error {
		var w v1beta1.Worker
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &w)
		if err == nil {
			return fmt.Errorf("worker still exists (phase=%q)", w.Status.Phase)
		}
		return client.IgnoreNotFound(err)
	})

	_, deprovCount2, _, _ := mockProv.CallCounts()
	if deprovCount2 == 0 {
		t.Error("DeprovisionWorker should have been called even for a failed worker")
	}
	_, _, _, _, cleanupCount2 := mockDeploy.CallCounts()
	if cleanupCount2 == 0 {
		t.Error("CleanupOSSData should have been called even for a failed worker")
	}
}

// ---------------------------------------------------------------------------
// spec.env propagation
// ---------------------------------------------------------------------------

func TestWorkerCreate_EnvPassesToBackend(t *testing.T) {
	resetMocks()

	workerName := fixtures.UniqueName("test-env")
	worker := fixtures.NewTestWorker(workerName)
	worker.Spec.Env = map[string]string{
		"USER_FOO":   "bar",
		"USER_EMPTY": "",
		// System-wins: HICLAW_WORKER_NAME is produced by MockEnvBuilder and
		// must override this user-supplied value.
		"HICLAW_WORKER_NAME": "user-should-lose",
	}

	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("failed to create Worker CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, worker)
	})

	waitForRunning(t, worker)

	req, ok := mockBackend.FindCreateReq(workerName)
	if !ok {
		t.Fatalf("no CreateRequest recorded for worker %q", workerName)
	}
	if got := req.Env["USER_FOO"]; got != "bar" {
		t.Errorf("USER_FOO=%q, want %q", got, "bar")
	}
	if got, present := req.Env["USER_EMPTY"]; !present || got != "" {
		t.Errorf("USER_EMPTY present=%v value=%q, want present=true value=\"\"", present, got)
	}
	if got := req.Env["HICLAW_WORKER_NAME"]; got != workerName {
		t.Errorf("HICLAW_WORKER_NAME=%q, want %q (system wins)", got, workerName)
	}
	if got := req.Env["MOCK_ENV"]; got != "true" {
		t.Errorf("MOCK_ENV=%q, want %q (system env preserved)", got, "true")
	}
}

// ---------------------------------------------------------------------------
// Worker MCP config propagation
// ---------------------------------------------------------------------------

func TestWorkerUpdate_MCPServersChange_RewritesMcporterJSON(t *testing.T) {
	resetMocks()

	workerName := fixtures.UniqueName("test-worker-mcp")
	worker := fixtures.NewTestWorker(workerName)
	worker.Spec.McpServers = []v1beta1.MCPServer{
		{Name: "github", URL: "https://gw.example.com/mcp-servers/github/mcp"},
	}

	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("failed to create Worker CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, worker)
	})

	waitForRunning(t, worker)

	clearAllCalls()

	updatedServers := []v1beta1.MCPServer{
		{Name: "github", URL: "https://gw.example.com/mcp-servers/github/mcp"},
		{Name: "jira", URL: "https://gw.example.com/mcp-servers/jira/mcp", Transport: "sse"},
	}
	updateSpecField(t, worker, func(w *v1beta1.Worker) {
		w.Spec.McpServers = updatedServers
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
		var w v1beta1.Worker
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &w); err != nil {
			return err
		}
		if w.Status.ObservedGeneration != w.Generation {
			return fmt.Errorf("ObservedGeneration=%d, want %d", w.Status.ObservedGeneration, w.Generation)
		}
		for _, req := range mockDeploy.Calls.DeployWorkerConfig {
			if req.Name != workerName {
				continue
			}
			if len(req.McpServers) != len(expectedServers) {
				continue
			}
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
		return fmt.Errorf("DeployWorkerConfig not called with updated McpServers=%v (calls=%d)", expectedServers, len(mockDeploy.Calls.DeployWorkerConfig))
	})
}

// ---------------------------------------------------------------------------
// CR Labels → Pod Labels propagation
// ---------------------------------------------------------------------------

// TestWorkerLabels_PropagateFromMetadataAndSpecToBackendCreate walks the
// full reconcile pipeline end-to-end: API server -> informer ->
// reconciler -> mock backend, and asserts the CreateRequest.Labels the
// backend actually receives reflects the documented four-layer merge
// (CR metadata.labels, CR spec.labels, controller system labels) with
// spec beating metadata on key collision and system always beating
// user-supplied reserved keys. This is the integration-test complement
// to the workerMemberContext unit tests — it proves the merge is
// preserved through CRD serialization, DeepCopy, and the entire member
// reconcile path rather than just at the helper level.
func TestWorkerLabels_PropagateFromMetadataAndSpecToBackendCreate(t *testing.T) {
	resetMocks()

	var (
		capMu   = newLabelCapture()
		wantCtl = "test-ctl"
	)
	mockBackend.CreateFn = capMu.CreateFn()

	name := fixtures.UniqueName("labels-worker")
	w := fixtures.NewTestWorker(name)
	w.ObjectMeta.Labels = map[string]string{
		"owner": "alice",
		"team":  "metadata-team",
	}
	w.Spec.Labels = map[string]string{
		"env":                   "prod",
		"team":                  "spec-team",      // overrides metadata
		v1beta1.LabelController: "spec-attacker",  // must be overridden by system
		"hiclaw.io/worker":      "spec-fake-name", // must be overridden by system
	}

	if err := k8sClient.Create(ctx, w); err != nil {
		t.Fatalf("create Worker: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, w) })

	waitForRunning(t, w)

	labels := capMu.LabelsFor(name)
	if labels == nil {
		t.Fatalf("backend Create was never called for %q (captured=%v)", name, capMu.Keys())
	}

	assertLabel(t, labels, "owner", "alice")                   // metadata propagated
	assertLabel(t, labels, "env", "prod")                      // spec propagated
	assertLabel(t, labels, "team", "spec-team")                // spec beats metadata
	assertLabel(t, labels, v1beta1.LabelController, wantCtl)   // system beats user
	assertLabel(t, labels, "hiclaw.io/worker", name)           // system beats user
	assertLabel(t, labels, "hiclaw.io/role", "standalone")     // system
}

// TestWorkerLabels_MetadataLabelsChangeDoesNotRecreatePod verifies the
// documented non-disruption contract: changing only metadata.labels
// after the Pod is Running must NOT trigger backend.Delete +
// backend.Create (which would otherwise cause an unnecessary Matrix
// session reset and mid-task disruption). Only spec changes — which
// bump Generation — are allowed to recreate Pods.
func TestWorkerLabels_MetadataLabelsChangeDoesNotRecreatePod(t *testing.T) {
	resetMocks()

	name := fixtures.UniqueName("labels-noop")
	w := fixtures.NewTestWorker(name)

	if err := k8sClient.Create(ctx, w); err != nil {
		t.Fatalf("create Worker: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, w) })

	waitForRunning(t, w)

	// Snapshot create/delete counts at steady state.
	createsBefore, deletesBefore, _, _, _ := mockBackend.CallSnapshot()

	// Patch only metadata.labels; this must not bump Generation and
	// therefore must not trigger a recreate.
	updateSpecField(t, w, func(got *v1beta1.Worker) {
		if got.ObjectMeta.Labels == nil {
			got.ObjectMeta.Labels = map[string]string{}
		}
		got.ObjectMeta.Labels["newly-added"] = "v1"
	})

	// Force at least one extra reconcile pass so the controller observes
	// the metadata change; triggerReconcile bumps an annotation which
	// fires the watch but still doesn't bump Generation.
	triggerReconcile(t, w)

	// Let a few reconcile cycles flush.
	time.Sleep(2 * time.Second)

	createsAfter, deletesAfter, _, _, _ := mockBackend.CallSnapshot()
	if len(createsAfter) != len(createsBefore) {
		t.Errorf("metadata.labels change triggered backend.Create: before=%d after=%d", len(createsBefore), len(createsAfter))
	}
	if len(deletesAfter) != len(deletesBefore) {
		t.Errorf("metadata.labels change triggered backend.Delete: before=%d after=%d", len(deletesBefore), len(deletesAfter))
	}

	var refreshed v1beta1.Worker
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(w), &refreshed); err != nil {
		t.Fatalf("get Worker: %v", err)
	}
	if refreshed.ObjectMeta.Labels["newly-added"] != "v1" {
		t.Fatalf("metadata.labels not persisted: %v", refreshed.ObjectMeta.Labels)
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func ptrString(s string) *string {
	return &s
}

// clearAllCalls resets call records on all mocks without clearing Fn overrides or state.
func clearAllCalls() {
	mockProv.ClearCalls()
	mockDeploy.ClearCalls()
	mockBackend.ClearCalls()
	mockEnv.ClearCalls()
}

// waitForRunning blocks until the Worker reaches Phase "Running".
func waitForRunning(t *testing.T, worker *v1beta1.Worker) {
	t.Helper()
	assertEventually(t, func() error {
		var w v1beta1.Worker
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &w); err != nil {
			return err
		}
		if w.Status.Phase != "Running" {
			return fmt.Errorf("phase=%q message=%q gen=%d obsGen=%d, want Running",
				w.Status.Phase, w.Status.Message, w.Generation, w.Status.ObservedGeneration)
		}
		return nil
	})
}

// updateSpecField performs a read-modify-write on the Worker spec using the
// given mutator function. Retries on conflict (stale resourceVersion).
func updateSpecField(t *testing.T, worker *v1beta1.Worker, mutate func(w *v1beta1.Worker)) {
	t.Helper()
	assertEventually(t, func() error {
		var w v1beta1.Worker
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &w); err != nil {
			return err
		}
		mutate(&w)
		return k8sClient.Update(ctx, &w)
	})
}

// triggerReconcile forces a reconcile by adding/updating an annotation.
// This does NOT increment Generation (metadata-only change).
func triggerReconcile(t *testing.T, worker *v1beta1.Worker) {
	t.Helper()
	assertEventually(t, func() error {
		var w v1beta1.Worker
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &w); err != nil {
			return err
		}
		if w.Annotations == nil {
			w.Annotations = map[string]string{}
		}
		w.Annotations["hiclaw.io/reconcile-trigger"] = fmt.Sprintf("%d", time.Now().UnixNano())
		return k8sClient.Update(ctx, &w)
	})
}

// assertEventually polls condFn until it returns nil or the timeout expires.
func assertEventually(t *testing.T, condFn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = condFn()
		if lastErr == nil {
			return
		}
		time.Sleep(interval)
	}
	t.Fatalf("condition not met within %v: %v", timeout, lastErr)
}
