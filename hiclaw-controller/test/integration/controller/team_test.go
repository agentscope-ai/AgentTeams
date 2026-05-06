//go:build integration

package controller_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"github.com/hiclaw/hiclaw-controller/test/testutil/fixtures"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// workersRegistryFile mirrors the JSON shape of workers-registry.json. Only
// the fields that Manager-side skills rely on are extracted; the registry
// document itself is write-heavy and its full internal shape lives in
// internal/service/legacy.go.
type workersRegistryFile struct {
	Version int                             `json:"version"`
	Workers map[string]workersRegistryEntry `json:"workers"`
}

type workersRegistryEntry struct {
	MatrixUserID string   `json:"matrix_user_id"`
	RoomID       string   `json:"room_id"`
	Runtime      string   `json:"runtime"`
	Deployment   string   `json:"deployment"`
	Skills       []string `json:"skills"`
	Role         string   `json:"role"`
	TeamID       *string  `json:"team_id"`
	Image        *string  `json:"image"`
}

// readWorkersRegistry fetches and parses agents/<manager>/workers-registry.json
// from the in-memory OSS wired up in suite_test.go. Returns an empty registry
// when the file has not been written yet.
func readWorkersRegistry(t *testing.T) *workersRegistryFile {
	t.Helper()
	key := "agents/" + testManagerName + "/workers-registry.json"
	data, err := testOSS.GetObject(ctx, key)
	if err != nil {
		return &workersRegistryFile{Workers: map[string]workersRegistryEntry{}}
	}
	var reg workersRegistryFile
	if err := json.Unmarshal(data, &reg); err != nil {
		t.Fatalf("parse workers-registry.json: %v", err)
	}
	if reg.Workers == nil {
		reg.Workers = map[string]workersRegistryEntry{}
	}
	return &reg
}

// ---------------------------------------------------------------------------
// Team lifecycle — happy path
// ---------------------------------------------------------------------------

func TestTeamCreate_ProvisionsLeaderAndWorkers(t *testing.T) {
	resetMocks()

	name := fixtures.UniqueName("t-create")
	team := fixtures.NewTestTeam(name, name+"-lead", name+"-dev", name+"-qa")

	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create team: %v", err)
	}
	t.Cleanup(func() { _ = deleteAndWait(t, team) })

	waitForTeamPhase(t, team, "Active")

	var got v1beta1.Team
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
		t.Fatalf("get team: %v", err)
	}

	if got.Status.TeamRoomID == "" {
		t.Error("TeamRoomID should be populated")
	}
	if got.Status.LeaderDMRoomID == "" {
		t.Error("LeaderDMRoomID should be populated")
	}
	if got.Status.TotalWorkers != 2 {
		t.Errorf("TotalWorkers=%d, want 2", got.Status.TotalWorkers)
	}
	if !got.Status.LeaderReady {
		t.Error("LeaderReady should be true after convergence")
	}
	if got.Status.ReadyWorkers != 2 {
		t.Errorf("ReadyWorkers=%d, want 2", got.Status.ReadyWorkers)
	}

	wantObserved := map[string]bool{
		name + "-lead": true,
		name + "-dev":  true,
		name + "-qa":   true,
	}
	for _, ms := range got.Status.Members {
		if !ms.Observed {
			continue
		}
		if !wantObserved[ms.Name] {
			t.Errorf("unexpected observed member %q", ms.Name)
		}
		delete(wantObserved, ms.Name)
	}
	if len(wantObserved) > 0 {
		t.Errorf("missing observed members: %v", wantObserved)
	}

	// RoomID + MatrixUserID must be propagated into Status.Members so the
	// /api/v1/workers/<member> endpoint can synthesize a WorkerResponse.
	// This is the regression guard for test-21-team-project-dag's
	// `hiclaw get workers <member> -o json | jq .roomID` returning empty.
	for _, ms := range got.Status.Members {
		if !ms.Observed {
			continue
		}
		if ms.RoomID == "" {
			t.Errorf("Status.Members[%s].RoomID is empty after provisioning", ms.Name)
		}
		if ms.MatrixUserID == "" {
			t.Errorf("Status.Members[%s].MatrixUserID is empty after provisioning", ms.Name)
		}
	}

	if len(mockProv.Calls.ProvisionTeamRooms) == 0 {
		t.Error("ProvisionTeamRooms should have been called")
	}
	if len(mockDeploy.Calls.EnsureTeamStorage) == 0 {
		t.Error("EnsureTeamStorage should have been called")
	}
	if len(mockDeploy.Calls.InjectCoordinationContext) == 0 {
		t.Error("InjectCoordinationContext should have been called for leader")
	}
	// 1 leader + 2 workers = 3 ProvisionWorker calls on the first convergence
	if got := len(mockProv.Calls.ProvisionWorker); got < 3 {
		t.Errorf("ProvisionWorker count=%d, want >=3 (leader + 2 workers)", got)
	}
}

// TestTeamCreate_WritesWorkersRegistry is the direct regression guard for
// tests/test-18-team-config-verify.sh. Before PR #666 removed the child
// Worker CR per team member, WorkerReconciler.reconcileLegacy used to
// populate workers-registry.json with role/team_id for each member; after
// the refactor TeamReconciler must do the same itself or manager-side
// skills (find-worker, push-worker-skills, update-worker-config, etc.)
// silently break.
//
// The contract this test locks in:
//   - leader has role="team_leader", team_id=<team name>, runtime="copaw"
//   - workers have role="worker", team_id=<team name>, runtime="copaw"
//   - every member has a non-empty room_id and matrix_user_id
//   - leader has no image (hardcoded), workers carry image when declared
func TestTeamCreate_WritesWorkersRegistry(t *testing.T) {
	resetMocks()

	name := fixtures.UniqueName("t-registry")
	leader := name + "-lead"
	w1 := name + "-w1"
	w2 := name + "-w2"
	team := fixtures.NewTestTeam(name, leader, w1, w2)

	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create team: %v", err)
	}
	t.Cleanup(func() { _ = deleteAndWait(t, team) })

	waitForTeamPhase(t, team, "Active")

	// Registry writes are driven by the reconcile loop; poll until all three
	// expected members have landed to avoid racing the in-flight reconcile.
	assertEventually(t, func() error {
		reg := readWorkersRegistry(t)
		for _, n := range []string{leader, w1, w2} {
			if _, ok := reg.Workers[n]; !ok {
				return fmt.Errorf("workers-registry missing entry %q (have: %v)", n, registryKeys(reg))
			}
		}
		return nil
	})

	reg := readWorkersRegistry(t)

	leaderEntry, ok := reg.Workers[leader]
	if !ok {
		t.Fatalf("registry missing leader %q", leader)
	}
	if leaderEntry.Role != "team_leader" {
		t.Errorf("leader role=%q, want team_leader", leaderEntry.Role)
	}
	if leaderEntry.TeamID == nil || *leaderEntry.TeamID != name {
		t.Errorf("leader team_id=%v, want %q", leaderEntry.TeamID, name)
	}
	if leaderEntry.Runtime != "copaw" {
		t.Errorf("leader runtime=%q, want copaw", leaderEntry.Runtime)
	}
	if leaderEntry.MatrixUserID == "" {
		t.Errorf("leader matrix_user_id is empty")
	}
	if leaderEntry.RoomID == "" {
		t.Errorf("leader room_id is empty")
	}
	if leaderEntry.Image != nil {
		t.Errorf("leader image=%v, want nil (leader spec carries no image)", leaderEntry.Image)
	}

	for _, wName := range []string{w1, w2} {
		entry := reg.Workers[wName]
		if entry.Role != "worker" {
			t.Errorf("%s role=%q, want worker", wName, entry.Role)
		}
		if entry.TeamID == nil || *entry.TeamID != name {
			t.Errorf("%s team_id=%v, want %q", wName, entry.TeamID, name)
		}
		if entry.MatrixUserID == "" {
			t.Errorf("%s matrix_user_id is empty", wName)
		}
		if entry.RoomID == "" {
			t.Errorf("%s room_id is empty", wName)
		}
	}
}

// TestTeamCreate_LeaderOnly guards the "leader-only team" contract: after the
// CRD dropped `workers` from its required-list and TeamSpec.Workers gained
// `omitempty`, a Team with just a leader must reconcile to the same terminal
// state as a team with workers — Active phase, Team/LeaderDM rooms populated,
// LeaderReady=true, TotalWorkers=0, a single Status.Members entry for the
// leader with RoomID/MatrixUserID filled in, and exactly one ProvisionWorker
// call on the backend. This locks in the zero-worker path against future
// refactors that might assume `t.Spec.Workers[0]` is addressable.
func TestTeamCreate_LeaderOnly(t *testing.T) {
	resetMocks()

	name := fixtures.UniqueName("t-leader-only")
	team := fixtures.NewTestTeam(name, name+"-lead")

	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create team: %v", err)
	}
	t.Cleanup(func() { _ = deleteAndWait(t, team) })

	waitForTeamPhase(t, team, "Active")

	var got v1beta1.Team
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
		t.Fatalf("get team: %v", err)
	}

	if got.Status.TeamRoomID == "" {
		t.Error("TeamRoomID should be populated even for a leader-only team")
	}
	if got.Status.LeaderDMRoomID == "" {
		t.Error("LeaderDMRoomID should be populated even for a leader-only team")
	}
	if got.Status.TotalWorkers != 0 {
		t.Errorf("TotalWorkers=%d, want 0", got.Status.TotalWorkers)
	}
	if got.Status.ReadyWorkers != 0 {
		t.Errorf("ReadyWorkers=%d, want 0", got.Status.ReadyWorkers)
	}
	if !got.Status.LeaderReady {
		t.Error("LeaderReady should be true after convergence")
	}

	if len(got.Status.Members) != 1 {
		t.Fatalf("Status.Members length=%d, want 1 (leader only): %+v", len(got.Status.Members), got.Status.Members)
	}
	leader := got.Status.Members[0]
	if leader.Name != name+"-lead" {
		t.Errorf("Members[0].Name=%q, want %q", leader.Name, name+"-lead")
	}
	if leader.Role != "team_leader" {
		t.Errorf("Members[0].Role=%q, want %q", leader.Role, "team_leader")
	}
	if !leader.Observed {
		t.Error("Members[0].Observed should be true after provisioning")
	}
	if leader.RoomID == "" {
		t.Error("Members[0].RoomID should be populated after provisioning")
	}
	if leader.MatrixUserID == "" {
		t.Error("Members[0].MatrixUserID should be populated after provisioning")
	}

	if len(mockProv.Calls.ProvisionTeamRooms) == 0 {
		t.Error("ProvisionTeamRooms should have been called")
	}
	if len(mockDeploy.Calls.EnsureTeamStorage) == 0 {
		t.Error("EnsureTeamStorage should have been called")
	}
	// ProvisionWorker is called at least once for the leader. Multiple calls
	// are legitimate because status updates re-enter the reconcile loop; the
	// important property is that the reconciler actually provisions the
	// leader for a leader-only team rather than short-circuiting on the
	// empty workers slice.
	if got := len(mockProv.Calls.ProvisionWorker); got < 1 {
		t.Errorf("ProvisionWorker count=%d, want >=1 (leader)", got)
	}

	// workers-registry.json must carry the leader with role=team_leader so
	// manager-side skills treat a leader-only team the same way they treat a
	// populated team.
	assertEventually(t, func() error {
		if _, ok := readWorkersRegistry(t).Workers[name+"-lead"]; !ok {
			return fmt.Errorf("registry missing leader %q", name+"-lead")
		}
		return nil
	})
	entry := readWorkersRegistry(t).Workers[name+"-lead"]
	if entry.Role != "team_leader" {
		t.Errorf("leader role=%q, want team_leader", entry.Role)
	}
	if entry.TeamID == nil || *entry.TeamID != name {
		t.Errorf("leader team_id=%v, want %q", entry.TeamID, name)
	}
}

// ---------------------------------------------------------------------------
// Team — stale member cleanup
// ---------------------------------------------------------------------------

func TestTeamUpdate_RemovesStaleWorker(t *testing.T) {
	resetMocks()

	name := fixtures.UniqueName("t-stale")
	team := fixtures.NewTestTeam(name, name+"-lead", name+"-w1", name+"-w2")

	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create team: %v", err)
	}
	t.Cleanup(func() { _ = deleteAndWait(t, team) })

	waitForTeamPhase(t, team, "Active")

	mockProv.ClearCalls()
	mockDeploy.ClearCalls()
	mockBackend.ClearCalls()

	// Drop w2 from the spec.
	updateTeamSpec(t, team, func(tt *v1beta1.Team) {
		tt.Spec.Workers = []v1beta1.TeamWorkerSpec{
			{Name: name + "-w1", Model: "gpt-4o"},
		}
	})

	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		if got.Status.TotalWorkers != 1 {
			return fmt.Errorf("TotalWorkers=%d, want 1", got.Status.TotalWorkers)
		}
		for _, ms := range got.Status.Members {
			if ms.Name == name+"-w2" {
				return fmt.Errorf("Status.Members still contains stale %s", ms.Name)
			}
		}
		return nil
	})

	// Stale member should have been deprovisioned.
	found := false
	for _, req := range mockProv.Calls.DeprovisionWorker {
		if req.Name == name+"-w2" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("DeprovisionWorker should have been called for stale %s-w2", name)
	}

	// workers-registry.json must drop the stale entry so manager-side
	// tooling stops resolving it; the surviving leader + worker stay.
	assertEventually(t, func() error {
		reg := readWorkersRegistry(t)
		if _, ok := reg.Workers[name+"-w2"]; ok {
			return fmt.Errorf("stale entry %s-w2 still present in registry", name)
		}
		if _, ok := reg.Workers[name+"-lead"]; !ok {
			return fmt.Errorf("leader %s-lead missing from registry", name)
		}
		if _, ok := reg.Workers[name+"-w1"]; !ok {
			return fmt.Errorf("remaining worker %s-w1 missing from registry", name)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Team — deletion
// ---------------------------------------------------------------------------

func TestTeamDelete_CleansUpAllMembers(t *testing.T) {
	resetMocks()

	name := fixtures.UniqueName("t-delete")
	team := fixtures.NewTestTeam(name, name+"-lead", name+"-w1")

	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create team: %v", err)
	}

	waitForTeamPhase(t, team, "Active")

	mockProv.ClearCalls()
	mockDeploy.ClearCalls()

	if err := k8sClient.Delete(ctx, team); err != nil {
		t.Fatalf("delete team: %v", err)
	}

	assertEventually(t, func() error {
		var got v1beta1.Team
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got)
		if err == nil {
			return fmt.Errorf("team still exists (phase=%q)", got.Status.Phase)
		}
		return client.IgnoreNotFound(err)
	})

	deprovisioned := make(map[string]bool)
	for _, req := range mockProv.Calls.DeprovisionWorker {
		deprovisioned[req.Name] = true
	}
	if !deprovisioned[name+"-lead"] {
		t.Errorf("DeprovisionWorker should have been called for leader %s-lead", name)
	}
	if !deprovisioned[name+"-w1"] {
		t.Errorf("DeprovisionWorker should have been called for worker %s-w1", name)
	}
	if len(mockDeploy.Calls.CleanupOSSData) < 2 {
		t.Errorf("CleanupOSSData count=%d, want >=2 (leader + worker)", len(mockDeploy.Calls.CleanupOSSData))
	}

	// workers-registry.json must no longer reference either member: a deleted
	// team must not leave ghost rows that manager-side tooling would keep
	// trying to contact.
	assertEventually(t, func() error {
		reg := readWorkersRegistry(t)
		if _, ok := reg.Workers[name+"-lead"]; ok {
			return fmt.Errorf("ghost leader %s-lead still in registry", name)
		}
		if _, ok := reg.Workers[name+"-w1"]; ok {
			return fmt.Errorf("ghost worker %s-w1 still in registry", name)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Team — provision failure is surfaced as Failed phase
// ---------------------------------------------------------------------------

func TestTeamCreate_ProvisionRoomsFailure_SetsFailed(t *testing.T) {
	resetMocks()

	mockProv.ProvisionTeamRoomsFn = func(_ context.Context, _ service.TeamRoomRequest) (*service.TeamRoomResult, error) {
		return nil, fmt.Errorf("simulated room failure")
	}

	name := fixtures.UniqueName("t-fail")
	team := fixtures.NewTestTeam(name, name+"-lead", name+"-w1")

	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create team: %v", err)
	}
	t.Cleanup(func() { _ = deleteAndWait(t, team) })

	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		if got.Status.Phase != "Failed" {
			return fmt.Errorf("phase=%q, want Failed", got.Status.Phase)
		}
		if got.Status.Message == "" {
			return fmt.Errorf("message should contain failure reason")
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Team — member-level provision failure marks team Degraded, not Failed
// ---------------------------------------------------------------------------

func TestTeamCreate_WorkerProvisionFailure_Degraded(t *testing.T) {
	resetMocks()

	name := fixtures.UniqueName("t-degrade")
	badWorker := name + "-bad"

	mockProv.ProvisionWorkerFn = func(_ context.Context, req service.WorkerProvisionRequest) (*service.WorkerProvisionResult, error) {
		if req.Name == badWorker {
			return nil, fmt.Errorf("simulated worker failure")
		}
		return &service.WorkerProvisionResult{
			MatrixUserID:   "@" + req.Name + ":localhost",
			MatrixToken:    "mock-token-" + req.Name,
			RoomID:         "!room-" + req.Name + ":localhost",
			GatewayKey:     "mock-gw-key-" + req.Name,
			MinIOPassword:  "mock-minio-pw",
			MatrixPassword: "mock-matrix-pw",
		}, nil
	}

	team := fixtures.NewTestTeam(name, name+"-lead", name+"-ok", badWorker)

	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create team: %v", err)
	}
	t.Cleanup(func() { _ = deleteAndWait(t, team) })

	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		if got.Status.Phase != "Degraded" {
			return fmt.Errorf("phase=%q, want Degraded", got.Status.Phase)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Team — backend readiness dictates Active vs Pending
// ---------------------------------------------------------------------------

func TestTeamCreate_PartialReadiness_RemainsPending(t *testing.T) {
	resetMocks()

	name := fixtures.UniqueName("t-partial")
	leaderName := name + "-lead"

	// Leader reports Running; worker reports Starting (pod exists but not ready).
	// Using Starting avoids triggering recreate loops in the reconciler, which
	// would happen if we returned StatusNotFound.
	mockBackend.StatusFn = func(_ context.Context, workerName string) (*backend.WorkerResult, error) {
		if workerName == leaderName {
			return &backend.WorkerResult{Status: backend.StatusRunning}, nil
		}
		return &backend.WorkerResult{Status: backend.StatusStarting}, nil
	}

	team := fixtures.NewTestTeam(name, leaderName, name+"-w1")

	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create team: %v", err)
	}
	t.Cleanup(func() { _ = deleteAndWait(t, team) })

	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		if got.Status.Phase == "Active" {
			return fmt.Errorf("team reached Active too early")
		}
		if !got.Status.LeaderReady {
			return fmt.Errorf("LeaderReady should be true")
		}
		if got.Status.ReadyWorkers != 0 {
			return fmt.Errorf("ReadyWorkers=%d, want 0 (worker still Starting)", got.Status.ReadyWorkers)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Team — finalizer is added on first reconcile
// ---------------------------------------------------------------------------

func TestTeamFinalizer_AddedOnCreate(t *testing.T) {
	resetMocks()

	name := fixtures.UniqueName("t-final")
	team := fixtures.NewTestTeam(name, name+"-lead", name+"-w1")

	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create team: %v", err)
	}
	t.Cleanup(func() { _ = deleteAndWait(t, team) })

	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		for _, f := range got.Finalizers {
			if f == "hiclaw.io/cleanup" {
				return nil
			}
		}
		return fmt.Errorf("finalizer not found in %v", got.Finalizers)
	})
}

// ---------------------------------------------------------------------------
// Team — update: add a worker must not recreate existing members
// ---------------------------------------------------------------------------

// TestTeamUpdate_AddWorker_DoesNotRecreateExisting is the regression guard
// for the per-member spec-change-detection bug: previously the reconciler
// compared Team.Generation against MemberContext.ObservedGeneration, which
// was always 0 for team members, so every reconcile tore down every pod.
//
// Expected behaviour: adding a worker to the Team spec creates the new
// worker's container and leaves all previously-provisioned member containers
// untouched (no Delete, no new Create for existing members).
func TestTeamUpdate_AddWorker_DoesNotRecreateExisting(t *testing.T) {
	resetMocks()

	name := fixtures.UniqueName("t-addw")
	leader := name + "-lead"
	existing := name + "-w1"
	added := name + "-w2"

	team := fixtures.NewTestTeam(name, leader, existing)
	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create team: %v", err)
	}
	t.Cleanup(func() { _ = deleteAndWait(t, team) })

	waitForTeamPhase(t, team, "Active")

	// Baseline: one Create per member on the first convergence.
	creates, deletes, _, _, _ := mockBackend.CallSnapshot()
	if len(creates) < 2 {
		t.Fatalf("baseline creates=%v, want >=2 (leader + existing)", creates)
	}
	if len(deletes) != 0 {
		t.Fatalf("baseline deletes=%v, want 0", deletes)
	}

	mockBackend.ClearCalls()
	mockProv.ClearCalls()
	mockDeploy.ClearCalls()

	updateTeamSpec(t, team, func(tt *v1beta1.Team) {
		tt.Spec.Workers = append(tt.Spec.Workers, v1beta1.TeamWorkerSpec{
			Name:  added,
			Model: "gpt-4o",
		})
	})

	// Wait until the new worker is observed & team is Active again.
	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		if got.Status.TotalWorkers != 2 {
			return fmt.Errorf("TotalWorkers=%d, want 2", got.Status.TotalWorkers)
		}
		observed := make(map[string]bool)
		for _, ms := range got.Status.Members {
			if ms.Observed {
				observed[ms.Name] = true
			}
		}
		if !observed[added] {
			return fmt.Errorf("observed missing %q", added)
		}
		if got.Status.Phase != "Active" {
			return fmt.Errorf("phase=%q, want Active", got.Status.Phase)
		}
		return nil
	})

	// Status.Members[*].SpecHash must be populated for every member — proves
	// the per-member hash path was taken rather than the fallback "always
	// changed" path.
	var got v1beta1.Team
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
		t.Fatalf("get team: %v", err)
	}
	for _, n := range []string{leader, existing, added} {
		ms := got.Status.MemberByName(n)
		if ms == nil {
			t.Errorf("Status.Members is missing entry for %q", n)
			continue
		}
		if ms.SpecHash == "" {
			t.Errorf("Status.Members[%q].SpecHash is empty, want non-empty", n)
		}
	}

	// The critical assertion: existing leader/worker must not be recreated.
	// Only the new worker is allowed in the post-update Create set, and no
	// Deletes are allowed at all.
	creates, deletes, _, _, _ = mockBackend.CallSnapshot()
	for _, c := range creates {
		if c != added {
			t.Errorf("backend.Create called for existing member %q after spec update; creates=%v", c, creates)
		}
	}
	if len(deletes) != 0 {
		t.Errorf("backend.Delete called after non-destructive spec update: %v", deletes)
	}
}

// ---------------------------------------------------------------------------
// spec.env propagation for team members
// ---------------------------------------------------------------------------

func TestTeam_MemberEnv_PassesToBackend(t *testing.T) {
	resetMocks()

	name := fixtures.UniqueName("t-env")
	leader := name + "-lead"
	worker := name + "-dev"
	team := fixtures.NewTestTeam(name, leader, worker)
	team.Spec.Leader.Env = map[string]string{
		"USER_LEAD":          "L1",
		"USER_EMPTY":         "",
		"HICLAW_WORKER_NAME": "user-should-lose",
	}
	team.Spec.Workers[0].Env = map[string]string{
		"USER_WORK":          "W1",
		"USER_EMPTY":         "",
		"HICLAW_WORKER_NAME": "user-should-lose",
	}

	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create team: %v", err)
	}
	t.Cleanup(func() { _ = deleteAndWait(t, team) })

	waitForTeamPhase(t, team, "Active")

	leaderReq, ok := mockBackend.FindCreateReq(leader)
	if !ok {
		t.Fatalf("no CreateRequest recorded for team leader %q", leader)
	}
	if got := leaderReq.Env["USER_LEAD"]; got != "L1" {
		t.Errorf("leader USER_LEAD=%q, want %q", got, "L1")
	}
	if got, present := leaderReq.Env["USER_EMPTY"]; !present || got != "" {
		t.Errorf("leader USER_EMPTY present=%v value=%q, want present=true value=\"\"", present, got)
	}
	if got := leaderReq.Env["HICLAW_WORKER_NAME"]; got != leader {
		t.Errorf("leader HICLAW_WORKER_NAME=%q, want %q (system wins)", got, leader)
	}
	if got := leaderReq.Env["MOCK_ENV"]; got != "true" {
		t.Errorf("leader MOCK_ENV=%q, want %q (system env preserved)", got, "true")
	}

	workerReq, ok := mockBackend.FindCreateReq(worker)
	if !ok {
		t.Fatalf("no CreateRequest recorded for team worker %q", worker)
	}
	if got := workerReq.Env["USER_WORK"]; got != "W1" {
		t.Errorf("worker USER_WORK=%q, want %q", got, "W1")
	}
	if got, present := workerReq.Env["USER_EMPTY"]; !present || got != "" {
		t.Errorf("worker USER_EMPTY present=%v value=%q, want present=true value=\"\"", present, got)
	}
	if got := workerReq.Env["HICLAW_WORKER_NAME"]; got != worker {
		t.Errorf("worker HICLAW_WORKER_NAME=%q, want %q (system wins)", got, worker)
	}
	if got := workerReq.Env["MOCK_ENV"]; got != "true" {
		t.Errorf("worker MOCK_ENV=%q, want %q (system env preserved)", got, "true")
	}

	// Leader's env must NOT leak into worker's env.
	if _, present := workerReq.Env["USER_LEAD"]; present {
		t.Errorf("worker Env leaked leader-only key USER_LEAD: %v", workerReq.Env)
	}
	if _, present := leaderReq.Env["USER_WORK"]; present {
		t.Errorf("leader Env leaked worker-only key USER_WORK: %v", leaderReq.Env)
	}
}

// ---------------------------------------------------------------------------
// CR Labels → Pod Labels propagation (Team)
// ---------------------------------------------------------------------------

// TestTeamLabels_PropagateAndIsolatePerMember walks the full Team
// reconcile pipeline end-to-end and asserts three things at once for
// the captured backend.CreateRequest.Labels:
//   - Team.metadata.labels fans out to the leader AND every worker Pod.
//   - Team.spec.leader.labels lands ONLY on the leader; per-member
//     workers[i].labels land ONLY on that worker and do not leak to
//     other workers or the leader.
//   - Controller-forced system labels (hiclaw.io/controller,
//     hiclaw.io/team, hiclaw.io/role) always win over user-supplied
//     values, including reserved keys stuffed into any of the three
//     user layers (metadata, leader.labels, workers[].labels).
func TestTeamLabels_PropagateAndIsolatePerMember(t *testing.T) {
	resetMocks()

	cap := newLabelCapture()
	mockBackend.CreateFn = cap.CreateFn()

	name := fixtures.UniqueName("labels-team")
	leaderName := name + "-lead"
	devName := name + "-dev"
	qaName := name + "-qa"

	team := fixtures.NewTestTeam(name, leaderName, devName, qaName)
	team.ObjectMeta.Labels = map[string]string{
		"squad":  "alpha",
		"region": "us-west",
		// Reserved-key attempts at the team-metadata layer.
		v1beta1.LabelController: "metadata-attacker",
	}
	team.Spec.Leader.Labels = map[string]string{
		"role-hint": "planner",
		"squad":     "leader-squad", // should beat team metadata for leader
	}
	// Per-member labels — each worker gets its own disjoint set so we
	// can detect cross-member leakage.
	for i := range team.Spec.Workers {
		switch team.Spec.Workers[i].Name {
		case devName:
			team.Spec.Workers[i].Labels = map[string]string{
				"skill":            "rust",
				"hiclaw.io/role":   "evil", // reserved-key override attempt
			}
		case qaName:
			team.Spec.Workers[i].Labels = map[string]string{
				"skill": "go",
			}
		}
	}

	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	t.Cleanup(func() { _ = deleteAndWait(t, team) })

	waitForTeamPhase(t, team, "Active")

	leaderLabels := cap.LabelsFor(leaderName)
	devLabels := cap.LabelsFor(devName)
	qaLabels := cap.LabelsFor(qaName)
	if leaderLabels == nil || devLabels == nil || qaLabels == nil {
		t.Fatalf("missing captured create: leader=%v dev=%v qa=%v (captured=%v)",
			leaderLabels != nil, devLabels != nil, qaLabels != nil, cap.Keys())
	}

	// Team-wide metadata fans out to every member.
	for _, pair := range []struct {
		who    string
		labels map[string]string
	}{
		{"leader", leaderLabels},
		{"dev", devLabels},
		{"qa", qaLabels},
	} {
		if got := pair.labels["region"]; got != "us-west" {
			t.Errorf("%s missing team metadata region=us-west: %v", pair.who, pair.labels)
		}
	}

	// Per-member beats team-wide on collision (leader).
	assertLabel(t, leaderLabels, "squad", "leader-squad")
	// Non-leader members inherit team-wide value since they do not
	// override it.
	assertLabel(t, devLabels, "squad", "alpha")
	assertLabel(t, qaLabels, "squad", "alpha")

	// Leader-only label does not leak to workers.
	assertLabel(t, leaderLabels, "role-hint", "planner")
	if _, ok := devLabels["role-hint"]; ok {
		t.Errorf("dev leaked leader role-hint: %v", devLabels)
	}
	if _, ok := qaLabels["role-hint"]; ok {
		t.Errorf("qa leaked leader role-hint: %v", qaLabels)
	}

	// Per-worker labels do not leak across workers or to the leader.
	assertLabel(t, devLabels, "skill", "rust")
	assertLabel(t, qaLabels, "skill", "go")
	if _, ok := leaderLabels["skill"]; ok {
		t.Errorf("leader leaked worker skill label: %v", leaderLabels)
	}

	// System labels always win on collision.
	for _, labels := range []map[string]string{leaderLabels, devLabels, qaLabels} {
		assertLabel(t, labels, v1beta1.LabelController, "test-ctl")
		assertLabel(t, labels, "hiclaw.io/team", name)
	}
	assertLabel(t, leaderLabels, "hiclaw.io/role", "team_leader")
	assertLabel(t, devLabels, "hiclaw.io/role", "worker")
	assertLabel(t, qaLabels, "hiclaw.io/role", "worker")
}

// ---------------------------------------------------------------------------
// Team — helpers
// ---------------------------------------------------------------------------

func waitForTeamPhase(t *testing.T, team *v1beta1.Team, phase string) {
	t.Helper()
	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		if got.Status.Phase != phase {
			return fmt.Errorf("phase=%q want %q (leaderReady=%v ready=%d total=%d msg=%q)",
				got.Status.Phase, phase, got.Status.LeaderReady,
				got.Status.ReadyWorkers, got.Status.TotalWorkers, got.Status.Message)
		}
		return nil
	})
}

func updateTeamSpec(t *testing.T, team *v1beta1.Team, mutate func(*v1beta1.Team)) {
	t.Helper()
	assertEventually(t, func() error {
		var cur v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &cur); err != nil {
			return err
		}
		mutate(&cur)
		return k8sClient.Update(ctx, &cur)
	})
}

// registryKeys returns the set of member names currently in the registry,
// used for test error messages.
func registryKeys(reg *workersRegistryFile) []string {
	out := make([]string, 0, len(reg.Workers))
	for k := range reg.Workers {
		out = append(out, k)
	}
	return out
}

func deleteAndWait(t *testing.T, team *v1beta1.Team) error {
	if err := k8sClient.Delete(ctx, team); err != nil {
		return client.IgnoreNotFound(err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var got v1beta1.Team
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got)
		if err != nil {
			return client.IgnoreNotFound(err)
		}
		time.Sleep(interval)
	}
	return fmt.Errorf("team %s not deleted within timeout", team.Name)
}
