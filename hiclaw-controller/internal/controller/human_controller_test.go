package controller

import (
	"context"
	"errors"
	"sort"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"github.com/hiclaw/hiclaw-controller/test/testutil/mocks"
)

// humanTestRig bundles the fake client, mock provisioner, reconciler, and
// a convenience "reconcile this object" helper. Every test builds a rig
// for its own object set so state doesn't leak between cases.
type humanTestRig struct {
	t      *testing.T
	client client.Client
	prov   *mocks.MockHumanProvisioner
	r      *HumanReconciler
}

func newHumanScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1beta1.AddToScheme(s); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	return s
}

func newHumanRig(t *testing.T, objs ...client.Object) *humanTestRig {
	t.Helper()
	scheme := newHumanScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1beta1.Human{}).
		Build()
	prov := mocks.NewMockHumanProvisioner()
	return &humanTestRig{
		t:      t,
		client: c,
		prov:   prov,
		r: &HumanReconciler{
			Client:      c,
			Provisioner: prov,
			// Legacy intentionally left nil — the reconciler's
			// legacy phase guards on nil/Enabled() so these tests
			// exercise incluster-mode behavior.
		},
	}
}

// reconcile drives a single Reconcile pass for the named Human and
// returns the freshly-fetched object so tests can assert on the status
// that the defer-patch wrote.
func (rig *humanTestRig) reconcile(name string) (*v1beta1.Human, reconcile.Result, error) {
	rig.t.Helper()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}}
	res, err := rig.r.Reconcile(context.Background(), req)
	var out v1beta1.Human
	if getErr := rig.client.Get(context.Background(), req.NamespacedName, &out); getErr != nil {
		// Deletion tests may race — return an empty object so callers
		// can still inspect status.
		return &v1beta1.Human{}, res, err
	}
	return &out, res, err
}

func newHuman(name string, spec v1beta1.HumanSpec) *v1beta1.Human {
	return &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spec,
	}
}

func newReadyWorker(name, roomID string) *v1beta1.Worker {
	w := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
	w.Status.RoomID = roomID
	return w
}

func newReadyTeam(name, roomID string) *v1beta1.Team {
	tm := &v1beta1.Team{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
	tm.Status.TeamRoomID = roomID
	return tm
}

// TestHumanReconciler_Create_HappyPath covers the first-time provisioning
// path: a brand-new Human CR, one ready Worker, one ready Team. Asserts
// that a **single** reconcile converges — EnsureHumanUser is called
// exactly once, each room gets one Invite+Join pair, Status is persisted
// in the same pass. The single-reconcile invariant is worth locking in
// explicitly because the finalizer-add path does *not* early-return;
// regressions that accidentally split the happy path into two reconciles
// would silently work thanks to the 5-minute requeue, but they'd also
// double the initial-provisioning latency and create opportunities for
// partial-state visibility.
//
// LoginAsHuman must NOT be called during first-time create: EnsureHumanUser
// already returns a usable access token, so a fresh provisioning reconcile
// costs exactly one Matrix device session.
func TestHumanReconciler_Create_HappyPath(t *testing.T) {
	worker := newReadyWorker("w1", "!room-w1:localhost")
	team := newReadyTeam("t1", "!room-t1:localhost")
	human := newHuman("alice", v1beta1.HumanSpec{
		DisplayName:       "Alice",
		PermissionLevel:   2,
		AccessibleWorkers: []string{"w1"},
		AccessibleTeams:   []string{"t1"},
	})

	rig := newHumanRig(t, human, worker, team)

	out, _, err := rig.reconcile("alice")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(rig.prov.Calls.EnsureHumanUser) != 1 {
		t.Errorf("EnsureHumanUser called %d times, want 1: %+v",
			len(rig.prov.Calls.EnsureHumanUser), rig.prov.Calls.EnsureHumanUser)
	}
	if len(rig.prov.Calls.LoginAsHuman) != 0 {
		t.Errorf("LoginAsHuman should not be called during first-time create (EnsureHumanUser already returned a token); got %d calls",
			len(rig.prov.Calls.LoginAsHuman))
	}

	if len(rig.prov.Calls.InviteToRoom) != 2 {
		t.Errorf("InviteToRoom calls=%d, want 2: %+v",
			len(rig.prov.Calls.InviteToRoom), rig.prov.Calls.InviteToRoom)
	}
	if len(rig.prov.Calls.JoinRoomAs) != 2 {
		t.Errorf("JoinRoomAs calls=%d, want 2: %+v",
			len(rig.prov.Calls.JoinRoomAs), rig.prov.Calls.JoinRoomAs)
	}

	invited := invitedRoomSet(rig.prov.Calls.InviteToRoom)
	if !invited["!room-w1:localhost"] || !invited["!room-t1:localhost"] {
		t.Errorf("Invite did not cover both rooms: %+v", rig.prov.Calls.InviteToRoom)
	}

	if out.Status.MatrixUserID != "@alice:localhost" {
		t.Errorf("Status.MatrixUserID=%q, want @alice:localhost", out.Status.MatrixUserID)
	}
	if out.Status.InitialPassword == "" {
		t.Error("Status.InitialPassword was not persisted")
	}
	if got := sortedCopy(out.Status.Rooms); len(got) != 2 ||
		got[0] != "!room-t1:localhost" || got[1] != "!room-w1:localhost" {
		t.Errorf("Status.Rooms=%v, want both rooms", got)
	}
	if out.Status.Phase != "Active" {
		t.Errorf("Status.Phase=%q, want Active", out.Status.Phase)
	}

	hasFinalizer := false
	for _, f := range out.Finalizers {
		if f == finalizerName {
			hasFinalizer = true
			break
		}
	}
	if !hasFinalizer {
		t.Errorf("finalizer %q not added on create reconcile; found %v",
			finalizerName, out.Finalizers)
	}
}

func TestHumanReconciler_ConditionsShowRoomAccessFailure(t *testing.T) {
	worker := newReadyWorker("w1", "!room-w1:localhost")
	human := newHuman("alice", v1beta1.HumanSpec{
		DisplayName:       "Alice",
		PermissionLevel:   2,
		AccessibleWorkers: []string{"w1"},
	})
	rig := newHumanRig(t, human, worker)
	rig.prov.InviteToRoomFn = func(context.Context, string, string) error {
		return errors.New("matrix invite failed")
	}

	out, _, err := rig.reconcile("alice")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if cond := meta.FindStatusCondition(out.Status.Conditions, v1beta1.ConditionMatrixUserReady); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("MatrixUserReady condition=%+v, want True", cond)
	}
	if cond := meta.FindStatusCondition(out.Status.Conditions, v1beta1.ConditionRoomAccessReady); cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "RoomAccessFailed" {
		t.Fatalf("RoomAccessReady condition=%+v, want False RoomAccessFailed", cond)
	}
	if cond := meta.FindStatusCondition(out.Status.Conditions, v1beta1.ConditionReady); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("Ready condition=%+v, want False", cond)
	}
	if len(out.Status.Rooms) != 0 {
		t.Fatalf("Status.Rooms=%v, want empty because invite failed", out.Status.Rooms)
	}
}

// TestHumanReconciler_Update_AddRoom adds a new AccessibleTeam to an
// already-Active Human and asserts that only the new room triggers
// Invite/Join (no duplicate calls against rooms already in Status.Rooms).
// This is the critical-path check for the unified
// desired-vs-observed convergence loop that replaced the old
// handleCreate/handleUpdate split.
func TestHumanReconciler_Update_AddRoom(t *testing.T) {
	worker := newReadyWorker("w1", "!room-w1:localhost")
	team := newReadyTeam("t1", "!room-t1:localhost")
	human := newHuman("alice", v1beta1.HumanSpec{
		AccessibleWorkers: []string{"w1"},
	})
	human.Status.MatrixUserID = "@alice:localhost"
	human.Status.InitialPassword = "stored-pw"
	human.Status.Rooms = []string{"!room-w1:localhost"}
	human.Status.Phase = "Active"
	human.Finalizers = []string{finalizerName}

	rig := newHumanRig(t, human, worker, team)

	// Now flip Spec to include the team — this is what a `kubectl apply`
	// of an updated spec would produce.
	var live v1beta1.Human
	if err := rig.client.Get(context.Background(),
		types.NamespacedName{Name: "alice", Namespace: "default"}, &live); err != nil {
		t.Fatalf("get: %v", err)
	}
	live.Spec.AccessibleTeams = []string{"t1"}
	if err := rig.client.Update(context.Background(), &live); err != nil {
		t.Fatalf("update spec: %v", err)
	}

	out, _, err := rig.reconcile("alice")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Steady-state login, not EnsureHumanUser.
	if len(rig.prov.Calls.EnsureHumanUser) != 0 {
		t.Errorf("EnsureHumanUser should not be called on update, got %d calls", len(rig.prov.Calls.EnsureHumanUser))
	}
	if len(rig.prov.Calls.LoginAsHuman) != 1 {
		t.Errorf("LoginAsHuman calls=%d, want 1", len(rig.prov.Calls.LoginAsHuman))
	}

	// Exactly one new room's worth of work: the team room.
	if len(rig.prov.Calls.InviteToRoom) != 1 ||
		rig.prov.Calls.InviteToRoom[0].RoomID != "!room-t1:localhost" {
		t.Errorf("Invite calls=%+v, want single invite to !room-t1:localhost", rig.prov.Calls.InviteToRoom)
	}
	if len(rig.prov.Calls.JoinRoomAs) != 1 ||
		rig.prov.Calls.JoinRoomAs[0].RoomID != "!room-t1:localhost" {
		t.Errorf("Join calls=%+v, want single join to !room-t1:localhost", rig.prov.Calls.JoinRoomAs)
	}
	if len(rig.prov.Calls.KickFromRoom) != 0 {
		t.Errorf("no kicks expected, got: %+v", rig.prov.Calls.KickFromRoom)
	}

	rooms := sortedCopy(out.Status.Rooms)
	if len(rooms) != 2 || rooms[0] != "!room-t1:localhost" || rooms[1] != "!room-w1:localhost" {
		t.Errorf("Status.Rooms=%v, want both rooms", rooms)
	}
}

// TestHumanReconciler_Update_RevokeRoom removes an AccessibleWorker from
// the spec of an Active Human and asserts that the corresponding room
// triggers KickFromRoom and disappears from Status.Rooms.
func TestHumanReconciler_Update_RevokeRoom(t *testing.T) {
	worker := newReadyWorker("w1", "!room-w1:localhost")
	team := newReadyTeam("t1", "!room-t1:localhost")
	human := newHuman("alice", v1beta1.HumanSpec{
		AccessibleWorkers: []string{"w1"},
		AccessibleTeams:   []string{"t1"},
	})
	human.Status.MatrixUserID = "@alice:localhost"
	human.Status.InitialPassword = "stored-pw"
	human.Status.Rooms = []string{"!room-w1:localhost", "!room-t1:localhost"}
	human.Status.Phase = "Active"
	human.Finalizers = []string{finalizerName}

	rig := newHumanRig(t, human, worker, team)

	// Remove the worker from the spec so it becomes a revocation.
	var live v1beta1.Human
	if err := rig.client.Get(context.Background(),
		types.NamespacedName{Name: "alice", Namespace: "default"}, &live); err != nil {
		t.Fatalf("get: %v", err)
	}
	live.Spec.AccessibleWorkers = nil
	if err := rig.client.Update(context.Background(), &live); err != nil {
		t.Fatalf("update spec: %v", err)
	}

	out, _, err := rig.reconcile("alice")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(rig.prov.Calls.KickFromRoom) != 1 ||
		rig.prov.Calls.KickFromRoom[0].RoomID != "!room-w1:localhost" ||
		rig.prov.Calls.KickFromRoom[0].UserID != "@alice:localhost" {
		t.Errorf("expected single kick from !room-w1:localhost for @alice:localhost, got %+v",
			rig.prov.Calls.KickFromRoom)
	}
	if len(rig.prov.Calls.InviteToRoom) != 0 {
		t.Errorf("no invites expected on pure revoke, got %+v", rig.prov.Calls.InviteToRoom)
	}

	if len(out.Status.Rooms) != 1 || out.Status.Rooms[0] != "!room-t1:localhost" {
		t.Errorf("Status.Rooms=%v, want [!room-t1:localhost]", out.Status.Rooms)
	}
}

// TestHumanReconciler_Update_PendingResource exercises the case where
// an AccessibleWorker references a Worker CR whose Status.RoomID is not
// yet populated (still provisioning). The reconciler must not invite
// into an empty room ID, not write anything to Status.Rooms for that
// worker, and come back cleanly on the next pass once the Worker is
// ready — in the meantime, no error.
func TestHumanReconciler_Update_PendingResource(t *testing.T) {
	// Worker exists but has no RoomID yet.
	worker := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "w1", Namespace: "default"}}
	human := newHuman("alice", v1beta1.HumanSpec{
		AccessibleWorkers: []string{"w1"},
	})
	rig := newHumanRig(t, human, worker)

	out, _, err := rig.reconcile("alice")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(rig.prov.Calls.InviteToRoom) != 0 {
		t.Errorf("no invites expected for pending worker, got: %+v", rig.prov.Calls.InviteToRoom)
	}
	if len(rig.prov.Calls.JoinRoomAs) != 0 {
		t.Errorf("no joins expected for pending worker, got: %+v", rig.prov.Calls.JoinRoomAs)
	}
	// With lazy login, a reconcile that has no new rooms to /join must
	// not trigger LoginAsHuman either — EnsureHumanUser on this
	// first-time pass already seeded scope.userToken, but there's no
	// work for it to do, and we must not generate a fresh device when
	// the spec is effectively a no-op.
	if len(rig.prov.Calls.LoginAsHuman) != 0 {
		t.Errorf("LoginAsHuman should not be called when there are no rooms to join, got %d",
			len(rig.prov.Calls.LoginAsHuman))
	}
	if len(out.Status.Rooms) != 0 {
		t.Errorf("Status.Rooms=%v, want empty (worker still pending)", out.Status.Rooms)
	}
	// We still successfully provisioned the Matrix account; phase stays
	// Active because MatrixUserID is set.
	if out.Status.Phase != "Active" {
		t.Errorf("Status.Phase=%q, want Active", out.Status.Phase)
	}
}

// TestHumanReconciler_SteadyState_NoLogin locks in the device-bloat
// fix: once a Human is Active and Status.Rooms matches the desired set,
// periodic reconciles (driven by reconcileInterval every 5 minutes)
// must NOT call LoginAsHuman. Every Login call without a device_id
// creates a new Matrix device session on the homeserver; under the
// pre-fix behavior a single Human would accumulate ~288 orphan devices
// per day. The invariant "desired == observed ⇒ zero Matrix writes"
// is the cheapest way to guard against that regression returning.
func TestHumanReconciler_SteadyState_NoLogin(t *testing.T) {
	worker := newReadyWorker("w1", "!room-w1:localhost")
	team := newReadyTeam("t1", "!room-t1:localhost")
	human := newHuman("alice", v1beta1.HumanSpec{
		AccessibleWorkers: []string{"w1"},
		AccessibleTeams:   []string{"t1"},
	})
	// Already-converged state: account provisioned, both rooms joined.
	human.Status.MatrixUserID = "@alice:localhost"
	human.Status.InitialPassword = "stored-pw"
	human.Status.Rooms = []string{"!room-w1:localhost", "!room-t1:localhost"}
	human.Status.Phase = "Active"
	human.Finalizers = []string{finalizerName}

	rig := newHumanRig(t, human, worker, team)

	out, _, err := rig.reconcile("alice")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(rig.prov.Calls.EnsureHumanUser) != 0 {
		t.Errorf("EnsureHumanUser must not be called on steady-state reconcile, got %d",
			len(rig.prov.Calls.EnsureHumanUser))
	}
	if len(rig.prov.Calls.LoginAsHuman) != 0 {
		t.Errorf("LoginAsHuman must not be called when desired == observed; device bloat regression! got %d",
			len(rig.prov.Calls.LoginAsHuman))
	}
	if len(rig.prov.Calls.InviteToRoom) != 0 {
		t.Errorf("no invites expected on steady-state, got %+v", rig.prov.Calls.InviteToRoom)
	}
	if len(rig.prov.Calls.JoinRoomAs) != 0 {
		t.Errorf("no joins expected on steady-state, got %+v", rig.prov.Calls.JoinRoomAs)
	}
	if len(rig.prov.Calls.KickFromRoom) != 0 {
		t.Errorf("no kicks expected on steady-state, got %+v", rig.prov.Calls.KickFromRoom)
	}

	rooms := sortedCopy(out.Status.Rooms)
	if len(rooms) != 2 || rooms[0] != "!room-t1:localhost" || rooms[1] != "!room-w1:localhost" {
		t.Errorf("Status.Rooms must remain unchanged on steady-state, got %v", rooms)
	}
	if out.Status.Phase != "Active" {
		t.Errorf("Status.Phase=%q, want Active", out.Status.Phase)
	}
}

// TestHumanReconciler_Delete_FinalizerCleanup sets DeletionTimestamp on
// an Active Human and asserts that every room in Status.Rooms triggers
// a ForceLeaveRoom (admin-bot path, since we have no valid user token
// to /leave with), and that the finalizer is removed so the CR can be
// garbage-collected.
func TestHumanReconciler_Delete_FinalizerCleanup(t *testing.T) {
	now := metav1.Now()
	human := newHuman("alice", v1beta1.HumanSpec{})
	human.DeletionTimestamp = &now
	human.Finalizers = []string{finalizerName}
	human.Status.MatrixUserID = "@alice:localhost"
	human.Status.InitialPassword = "stored-pw"
	human.Status.Rooms = []string{"!room-a:localhost", "!room-b:localhost"}
	human.Status.Phase = "Active"

	rig := newHumanRig(t, human)

	_, _, err := rig.reconcile("alice")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(rig.prov.Calls.ForceLeaveRoom) != 2 {
		t.Fatalf("ForceLeaveRoom calls=%d, want 2: %+v",
			len(rig.prov.Calls.ForceLeaveRoom), rig.prov.Calls.ForceLeaveRoom)
	}
	rooms := map[string]bool{}
	for _, call := range rig.prov.Calls.ForceLeaveRoom {
		if call.UserID != "@alice:localhost" {
			t.Errorf("ForceLeaveRoom userID=%q, want @alice:localhost", call.UserID)
		}
		rooms[call.RoomID] = true
	}
	if !rooms["!room-a:localhost"] || !rooms["!room-b:localhost"] {
		t.Errorf("ForceLeaveRoom did not cover both rooms: %+v", rig.prov.Calls.ForceLeaveRoom)
	}

	// Finalizer must be gone after a successful delete reconcile so
	// garbage collection can proceed.
	var out v1beta1.Human
	getErr := rig.client.Get(context.Background(),
		types.NamespacedName{Name: "alice", Namespace: "default"}, &out)
	if getErr == nil {
		for _, f := range out.Finalizers {
			if f == finalizerName {
				t.Errorf("finalizer %q still present after delete", finalizerName)
			}
		}
	}
}

// TestHumanReconciler_Login_StalePassword simulates the case where a
// user changed their password in Element after the controller's initial
// provisioning (so Status.InitialPassword no longer works). The
// reconciler must:
//  1. NOT fall back to EnsureHumanUser (that would trigger
//     reset-password and clobber the user's chosen password)
//  2. NOT return an error (the situation is expected, not a failure)
//  3. Still invite the user into new rooms via the admin token, but
//     skip the /join step (no user token available)
//  4. Leave Status.Rooms unchanged for rooms where the admin could only
//     invite (the user must /join themselves next reconcile when we can
//     log in again — or via Element directly).
func TestHumanReconciler_Login_StalePassword(t *testing.T) {
	worker := newReadyWorker("w1", "!room-w1:localhost")
	human := newHuman("alice", v1beta1.HumanSpec{
		AccessibleWorkers: []string{"w1"},
	})
	human.Status.MatrixUserID = "@alice:localhost"
	human.Status.InitialPassword = "stale-pw"
	human.Status.Phase = "Active"
	human.Finalizers = []string{finalizerName}

	rig := newHumanRig(t, human, worker)
	rig.prov.LoginAsHumanFn = func(ctx context.Context, name, password string) (string, error) {
		return "", errors.New("M_FORBIDDEN: invalid password")
	}

	out, _, err := rig.reconcile("alice")
	if err != nil {
		t.Fatalf("reconcile returned error on stale-password path (should be non-fatal): %v", err)
	}

	if len(rig.prov.Calls.EnsureHumanUser) != 0 {
		t.Errorf("EnsureHumanUser must not be called on stale password; got %d calls", len(rig.prov.Calls.EnsureHumanUser))
	}
	if len(rig.prov.Calls.LoginAsHuman) != 1 {
		t.Errorf("LoginAsHuman should be attempted once, got %d", len(rig.prov.Calls.LoginAsHuman))
	}

	// Admin-only invite happened; no /join.
	if len(rig.prov.Calls.InviteToRoom) != 1 || rig.prov.Calls.InviteToRoom[0].RoomID != "!room-w1:localhost" {
		t.Errorf("expected single invite to !room-w1:localhost, got %+v", rig.prov.Calls.InviteToRoom)
	}
	if len(rig.prov.Calls.JoinRoomAs) != 0 {
		t.Errorf("JoinRoomAs should be skipped when user token is unavailable, got %+v", rig.prov.Calls.JoinRoomAs)
	}

	// Status.Rooms does NOT include the room yet — next reconcile
	// (presumably after the user re-syncs) will retry and record it.
	if len(out.Status.Rooms) != 0 {
		t.Errorf("Status.Rooms=%v, want empty (invite-only this pass)", out.Status.Rooms)
	}
	if out.Status.Phase != "Active" {
		t.Errorf("Status.Phase=%q, want Active (account still healthy)", out.Status.Phase)
	}
}

// --- helpers ---

func invitedRoomSet(calls []mocks.RoomMembershipCall) map[string]bool {
	out := make(map[string]bool, len(calls))
	for _, c := range calls {
		out[c.RoomID] = true
	}
	return out
}

func sortedCopy(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}

// Silence unused import lint when the service package is referenced only
// via the mock type alias in some future subtest.
var _ = service.HumanCredentials{}
