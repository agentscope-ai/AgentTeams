package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil/mocks"
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
// Steady-state login must NOT be called during first-time create:
// EnsurePrecreated already returns a usable access token, so a fresh
// provisioning reconcile costs exactly one Matrix device session.
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
	if len(rig.prov.Calls.LoginWithPassword) != 0 {
		t.Errorf("LoginWithPassword should not be called during first-time create (EnsurePrecreated already returned a token); got %d calls",
			len(rig.prov.Calls.LoginWithPassword))
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

// TestHumanReconciler_FinalizerPatchPreservesIdentitySource locks in the
// Worker-style MergeFrom patch when adding the cleanup finalizer. A full
// Update would rewrite the entire spec from the in-memory object and can
// drop fields the typed client failed to round-trip (notably
// spec.identitySource), silently converting SSO Humans into legacy ones.
func TestHumanReconciler_FinalizerPatchPreservesIdentitySource(t *testing.T) {
	issuer := "https://idp.example.com/pool"
	subject := "user-subject-123"
	human := newHuman("sso-user", v1beta1.HumanSpec{
		DisplayName:     "SSO User",
		Username:        "sso-user",
		PermissionLevel: 1,
		IdentitySource: &v1beta1.IdentitySourceSpec{
			Issuer:  issuer,
			Subject: subject,
		},
	})

	rig := newHumanRig(t, human)
	rig.prov.AppServiceEnabled = true

	out, _, err := rig.reconcile("sso-user")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if out.Spec.IdentitySource == nil {
		t.Fatal("spec.identitySource was dropped while adding finalizer")
	}
	if out.Spec.IdentitySource.Issuer != issuer {
		t.Errorf("spec.identitySource.issuer=%q, want %q", out.Spec.IdentitySource.Issuer, issuer)
	}
	if out.Spec.IdentitySource.Subject != subject {
		t.Errorf("spec.identitySource.subject=%q, want %q", out.Spec.IdentitySource.Subject, subject)
	}
	if len(rig.prov.Calls.RegisterLegacyUser) != 0 {
		t.Errorf("RegisterLegacyUser should not be called for SSO human, got %d calls",
			len(rig.prov.Calls.RegisterLegacyUser))
	}
	if len(rig.prov.Calls.RegisterAppServiceUser) != 1 {
		t.Errorf("RegisterAppServiceUser calls=%d, want 1", len(rig.prov.Calls.RegisterAppServiceUser))
	}
	if out.Status.InitialPassword != "" {
		t.Errorf("Status.InitialPassword=%q, want empty for SSO human", out.Status.InitialPassword)
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

	// Steady-state token acquisition goes through the legacy_password
	// identity source's EnsureUserToken -> LoginWithPassword, not the
	// EnsureHumanUser composite and not the legacy LoginAsHuman shim.
	if len(rig.prov.Calls.EnsureHumanUser) != 0 {
		t.Errorf("EnsureHumanUser should not be called on update, got %d calls", len(rig.prov.Calls.EnsureHumanUser))
	}
	if len(rig.prov.Calls.LoginWithPassword) != 1 {
		t.Errorf("LoginWithPassword calls=%d, want 1", len(rig.prov.Calls.LoginWithPassword))
	}
	if len(rig.prov.Calls.LoginAsHuman) != 0 {
		t.Errorf("LoginAsHuman (legacy shim) should not be used by the human steady-state path, got %d calls", len(rig.prov.Calls.LoginAsHuman))
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

// TestHumanReconciler_TeamAdminRoomNotRevoked pins the CI test-19
// regression (join-403 deadlock): the team reconciler's
// syncTeamRoomHumanStatuses writes the team room into the spec.admin's
// Status.Rooms without touching the admin's AccessibleTeams. The
// access-revocation path must therefore treat the admin's team room as
// desired — not "access revoked". Without the team-membership leg in
// buildDesiredHumanRooms the admin gets self-left out of their own team
// room, and the team then fails on join (M_FORBIDDEN: cannot join a room
// that is not public) on every reconcile because the admin is
// deliberately excluded from the team-room invite list.
func TestHumanReconciler_TeamAdminRoomNotRevoked(t *testing.T) {
	team := newReadyTeam("t1", "!room-t1:localhost")
	team.Spec.Admin = &v1beta1.TeamAdminSpec{Name: "alice"}
	human := newHuman("alice", v1beta1.HumanSpec{})
	human.Status.MatrixUserID = "@alice:localhost"
	human.Status.InitialPassword = "stored-pw"
	// The team reconciler already synced the room into Status.Rooms.
	human.Status.Rooms = []string{"!room-t1:localhost"}
	human.Status.Phase = "Active"
	human.Finalizers = []string{finalizerName}

	rig := newHumanRig(t, human, team)

	out, _, err := rig.reconcile("alice")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rig.prov.Calls.KickFromRoom) != 0 {
		t.Errorf("no admin kick expected for the admin's own team room, got %+v", rig.prov.Calls.KickFromRoom)
	}
	if len(rig.prov.Calls.KickFromRoomAs) != 0 {
		t.Errorf("no actor kick expected, got %+v", rig.prov.Calls.KickFromRoomAs)
	}
	if len(rig.prov.Calls.LeaveRoomAs) != 0 {
		t.Errorf("no self-leave expected, got %+v", rig.prov.Calls.LeaveRoomAs)
	}
	if len(rig.prov.Calls.ForceLeaveRoom) != 0 {
		t.Errorf("no force-leave expected, got %+v", rig.prov.Calls.ForceLeaveRoom)
	}
	if len(out.Status.Rooms) != 1 || out.Status.Rooms[0] != "!room-t1:localhost" {
		t.Errorf("Status.Rooms=%v, want [!room-t1:localhost] retained", out.Status.Rooms)
	}
}

// TestHumanReconciler_HumanMemberRoomNotRevoked is the humanMembers variant
// of TestHumanReconciler_TeamAdminRoomNotRevoked: a human listed in
// team.Spec.HumanMembers (and mirrored into the team's members by the
// team reconciler) must also keep their team room even when their own
// AccessibleTeams is empty.
func TestHumanReconciler_HumanMemberRoomNotRevoked(t *testing.T) {
	team := newReadyTeam("t1", "!room-t1:localhost")
	team.Spec.HumanMembers = []v1beta1.TeamMemberSpec{{Name: "bob", MatrixUserID: "@bob:localhost"}}
	human := newHuman("bob", v1beta1.HumanSpec{})
	human.Status.MatrixUserID = "@bob:localhost"
	human.Status.InitialPassword = "stored-pw"
	human.Status.Rooms = []string{"!room-t1:localhost"}
	human.Status.Phase = "Active"
	human.Finalizers = []string{finalizerName}

	rig := newHumanRig(t, human, team)

	out, _, err := rig.reconcile("bob")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rig.prov.Calls.KickFromRoom) != 0 {
		t.Errorf("no kick expected for a humanMember's team room, got %+v", rig.prov.Calls.KickFromRoom)
	}
	if len(rig.prov.Calls.LeaveRoomAs) != 0 {
		t.Errorf("no self-leave expected, got %+v", rig.prov.Calls.LeaveRoomAs)
	}
	if len(out.Status.Rooms) != 1 || out.Status.Rooms[0] != "!room-t1:localhost" {
		t.Errorf("Status.Rooms=%v, want [!room-t1:localhost] retained", out.Status.Rooms)
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
	// not trigger a steady-state login either — EnsurePrecreated on this
	// first-time pass already seeded scope.userToken, but there's no
	// work for it to do, and we must not generate a fresh device when
	// the spec is effectively a no-op.
	if len(rig.prov.Calls.LoginWithPassword) != 0 {
		t.Errorf("LoginWithPassword should not be called when there are no rooms to join, got %d",
			len(rig.prov.Calls.LoginWithPassword))
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
// must NOT call LoginWithPassword. Every Login call without a device_id
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
	if len(rig.prov.Calls.LoginWithPassword) != 0 {
		t.Errorf("LoginWithPassword must not be called when desired == observed; device bloat regression! got %d",
			len(rig.prov.Calls.LoginWithPassword))
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
	// Steady-state token acquisition uses LoginWithPassword; simulate the
	// stored password no longer working after the user rotated it.
	rig.prov.LoginWithPasswordFn = func(ctx context.Context, name, password string) (string, error) {
		return "", errors.New("M_FORBIDDEN: invalid password")
	}

	out, _, err := rig.reconcile("alice")
	if err != nil {
		t.Fatalf("reconcile returned error on stale-password path (should be non-fatal): %v", err)
	}

	if len(rig.prov.Calls.EnsureHumanUser) != 0 {
		t.Errorf("EnsureHumanUser must not be called on stale password; got %d calls", len(rig.prov.Calls.EnsureHumanUser))
	}
	if len(rig.prov.Calls.LoginWithPassword) != 1 {
		t.Errorf("LoginWithPassword should be attempted once, got %d", len(rig.prov.Calls.LoginWithPassword))
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

// TestHumanReconciler_RevertSSOToLegacy_Blocked covers the identity-switch
// guard for the SSO→legacy direction. A Human that was provisioned through
// an external SSO identity carries an SSO-derived MatrixUserID and a set of
// rooms that user already joined. If spec.identitySource is then removed,
// the spec resolves to the legacy_password source whose username-derived
// MXID differs from the recorded one.
//
// Re-provisioning in place would be unsafe: reconcileHumanInfra would swap
// MatrixUserID to the new legacy account, but Status.Rooms still lists the
// old SSO user's memberships, so the rooms phase would treat every desired
// room as already observed and never invite/join the new user — leaving a
// Human that reports Active with rooms while the new identity is in none of
// them. The guard must instead degrade the Human and keep all prior state
// intact so the operator can recreate the CR.
func TestHumanReconciler_RevertSSOToLegacy_Blocked(t *testing.T) {
	worker := newReadyWorker("w1", "!room-w1:localhost")
	human := newHuman("sso-user", v1beta1.HumanSpec{
		Username:          "sso-user",
		AccessibleWorkers: []string{"w1"},
	})
	// Previously provisioned via SSO: recorded MXID is the SSO hash-derived
	// localpart, which differs from the legacy "@sso-user:localhost" the
	// now-identitySource-less spec resolves to.
	ssoUserID := "@ssohash00112233445566778899aabb:localhost"
	human.Status.MatrixUserID = ssoUserID
	human.Status.Rooms = []string{"!room-w1:localhost"}
	human.Status.Phase = "Active"
	human.Finalizers = []string{finalizerName}

	rig := newHumanRig(t, human, worker)

	out, _, err := rig.reconcile("sso-user")
	if err != nil {
		t.Fatalf("reconcile should degrade (non-fatal), got error: %v", err)
	}

	if out.Status.Phase != "Degraded" {
		t.Errorf("Status.Phase=%q, want Degraded", out.Status.Phase)
	}
	if out.Status.Message == "" {
		t.Error("Status.Message should explain the blocked identity switch")
	}
	// Identity must not be silently swapped to the legacy account.
	if out.Status.MatrixUserID != ssoUserID {
		t.Errorf("Status.MatrixUserID=%q, want unchanged %q", out.Status.MatrixUserID, ssoUserID)
	}
	// No new account may be provisioned under the legacy username.
	if len(rig.prov.Calls.EnsureHumanUser) != 0 || len(rig.prov.Calls.RegisterLegacyUser) != 0 ||
		len(rig.prov.Calls.RegisterAppServiceUser) != 0 {
		t.Errorf("no provisioning expected on blocked switch; EnsureHumanUser=%d RegisterLegacyUser=%d RegisterAppServiceUser=%d",
			len(rig.prov.Calls.EnsureHumanUser), len(rig.prov.Calls.RegisterLegacyUser), len(rig.prov.Calls.RegisterAppServiceUser))
	}
	// The rooms phase must not run, so no membership churn for either user.
	if len(rig.prov.Calls.InviteToRoom) != 0 || len(rig.prov.Calls.JoinRoomAs) != 0 ||
		len(rig.prov.Calls.KickFromRoom) != 0 {
		t.Errorf("no room mutations expected on blocked switch; invites=%+v joins=%+v kicks=%+v",
			rig.prov.Calls.InviteToRoom, rig.prov.Calls.JoinRoomAs, rig.prov.Calls.KickFromRoom)
	}
	// Prior room state is preserved untouched.
	if len(out.Status.Rooms) != 1 || out.Status.Rooms[0] != "!room-w1:localhost" {
		t.Errorf("Status.Rooms=%v, want preserved [!room-w1:localhost]", out.Status.Rooms)
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

// TestHumanReconciler_PowerLevelMapping checks that every room in the
// desired set — new and already-observed — gets EnsureRoomPowerLevel with
// the level derived from the Human CR's permissionLevel.
func TestHumanReconciler_PowerLevelMapping(t *testing.T) {
	worker := newReadyWorker("w1", "!room-w1:localhost")
	team := newReadyTeam("t1", "!room-t1:localhost")
	human := newHuman("alice", v1beta1.HumanSpec{
		PermissionLevel:   1, // admin-equivalent → 100
		AccessibleWorkers: []string{"w1"},
		AccessibleTeams:   []string{"t1"},
	})
	human.Status.MatrixUserID = "@alice:localhost"
	human.Status.InitialPassword = "stored-pw"
	human.Status.Rooms = []string{"!room-w1:localhost"} // already in the worker room
	human.Status.Phase = "Active"
	human.Finalizers = []string{finalizerName}

	rig := newHumanRig(t, human, worker, team)
	out, _, err := rig.reconcile("alice")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	byRoom := map[string]int{}
	for _, c := range rig.prov.Calls.EnsureRoomPowerLevel {
		byRoom[c.RoomID] = c.Level
	}
	if byRoom["!room-w1:localhost"] != 100 {
		t.Errorf("existing room heal: level=%d, want 100 (calls=%+v)", byRoom["!room-w1:localhost"], rig.prov.Calls.EnsureRoomPowerLevel)
	}
	if byRoom["!room-t1:localhost"] != 100 {
		t.Errorf("new room: level=%d, want 100 (calls=%+v)", byRoom["!room-t1:localhost"], rig.prov.Calls.EnsureRoomPowerLevel)
	}
	for _, c := range rig.prov.Calls.EnsureRoomPowerLevel {
		if c.UserID != "@alice:localhost" {
			t.Errorf("power level granted to %s, want @alice:localhost", c.UserID)
		}
	}
	if len(out.Status.Rooms) != 2 {
		t.Errorf("Status.Rooms=%v, want both rooms", out.Status.Rooms)
	}
}

// Team-scoped humans (level 2) get the default member level (50), not room
// ownership.
func TestHumanReconciler_PowerLevelL2GetsDefault(t *testing.T) {
	worker := newReadyWorker("w1", "!room-w1:localhost")
	human := newHuman("bob", v1beta1.HumanSpec{
		PermissionLevel:   2,
		AccessibleWorkers: []string{"w1"},
	})
	human.Status.MatrixUserID = "@bob:localhost"
	human.Status.InitialPassword = "stored-pw"
	human.Status.Phase = "Active"
	human.Finalizers = []string{finalizerName}

	rig := newHumanRig(t, human, worker)
	if _, _, err := rig.reconcile("bob"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rig.prov.Calls.EnsureRoomPowerLevel) != 1 {
		t.Fatalf("power calls=%+v, want 1", rig.prov.Calls.EnsureRoomPowerLevel)
	}
	if got := rig.prov.Calls.EnsureRoomPowerLevel[0].Level; got != 50 {
		t.Errorf("level=%d, want 50", got)
	}
}

// A power-level grant failure is non-fatal: the room is still recorded and
// the next cycle retries.
func TestHumanReconciler_PowerLevelErrorNonFatal(t *testing.T) {
	worker := newReadyWorker("w1", "!room-w1:localhost")
	human := newHuman("carol", v1beta1.HumanSpec{
		PermissionLevel:   2,
		AccessibleWorkers: []string{"w1"},
	})
	human.Status.MatrixUserID = "@carol:localhost"
	human.Status.InitialPassword = "stored-pw"
	human.Status.Phase = "Active"
	human.Finalizers = []string{finalizerName}

	rig := newHumanRig(t, human, worker)
	rig.prov.EnsureRoomPowerLevelFn = func(ctx context.Context, roomID, userID string, level int, actorToken, selfToken string) error {
		return fmt.Errorf("matrix unavailable")
	}

	out, _, err := rig.reconcile("carol")
	if err != nil {
		t.Fatalf("power-level failure must be non-fatal, got: %v", err)
	}
	if len(out.Status.Rooms) != 1 || out.Status.Rooms[0] != "!room-w1:localhost" {
		t.Errorf("room not recorded despite power failure: %v", out.Status.Rooms)
	}
}

// A TeamAdmin-owned team room must be granted with the team admin's token
// (the homeserver admin is not a member of those rooms), while worker DM
// rooms keep the default admin actor.
func TestHumanReconciler_PowerGrantUsesTeamAdminActor(t *testing.T) {
	worker := newReadyWorker("w1", "!room-w1:localhost")
	team := newReadyTeam("t1", "!room-t1:localhost")
	team.Spec.Admin = &v1beta1.TeamAdminSpec{Name: "ada", MatrixUserID: "@ada:localhost"}
	adminHuman := newHuman("ada", v1beta1.HumanSpec{})
	adminHuman.Status.MatrixUserID = "@ada:localhost"
	adminHuman.Status.InitialPassword = "ada-pw"
	adminHuman.Status.Phase = "Active"

	human := newHuman("alice", v1beta1.HumanSpec{
		PermissionLevel:   2,
		AccessibleWorkers: []string{"w1"},
		AccessibleTeams:   []string{"t1"},
	})
	human.Status.MatrixUserID = "@alice:localhost"
	human.Status.InitialPassword = "alice-pw"
	human.Status.Rooms = []string{"!room-w1:localhost"}
	human.Status.Phase = "Active"
	human.Finalizers = []string{finalizerName}

	var adminLogins atomic.Int32
	rig := newHumanRig(t, human, worker, team, adminHuman)
	rig.prov.LoginWithPasswordFn = func(ctx context.Context, name, password string) (string, error) {
		switch name {
		case "ada":
			adminLogins.Add(1)
			return "teamadmin-token", nil
		case "alice":
			return "alice-token", nil
		}
		return "", fmt.Errorf("unexpected login %s", name)
	}

	out, _, err := rig.reconcile("alice")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	byRoom := map[string]string{}
	for _, c := range rig.prov.Calls.EnsureRoomPowerLevel {
		byRoom[c.RoomID] = c.ActorToken
	}
	if got := byRoom["!room-t1:localhost"]; got != "teamadmin-token" {
		t.Errorf("team room actor=%q, want teamadmin-token (calls=%+v)", got, rig.prov.Calls.EnsureRoomPowerLevel)
	}
	if got := byRoom["!room-w1:localhost"]; got != "" {
		t.Errorf("worker room actor=%q, want default admin (\"\")", got)
	}
	if adminLogins.Load() == 0 {
		t.Error("team admin token was never resolved via login")
	}
	if len(out.Status.Rooms) != 2 {
		t.Errorf("Status.Rooms=%v, want both rooms", out.Status.Rooms)
	}
}

// Steady-state: an equal-level demotion (actor 403 on the strict-greater
// rule) must be retried with the human's OWN token — lazily, i.e. the
// login happens only after the 403.
func TestHumanReconciler_EqualLevelDemotionSelfWriteFallback(t *testing.T) {
	worker := newReadyWorker("w1", "!room-w1:localhost")
	human := newHuman("carol", v1beta1.HumanSpec{
		PermissionLevel:   2,
		AccessibleWorkers: []string{"w1"},
	})
	human.Status.MatrixUserID = "@carol:localhost"
	human.Status.InitialPassword = "stored-pw"
	human.Status.Rooms = []string{"!room-w1:localhost"} // already a member
	human.Status.Phase = "Active"
	human.Finalizers = []string{finalizerName}

	var logins atomic.Int32
	rig := newHumanRig(t, human, worker)
	rig.prov.EnsureRoomPowerLevelFn = func(ctx context.Context, roomID, userID string, level int, actorToken, selfToken string) error {
		if selfToken == "" {
			// The homeserver's strict-greater rule rejects the actor's
			// equal-level demotion.
			return &matrix.APIError{StatusCode: 403, ErrCode: "M_FORBIDDEN", Message: "target level not below sender"}
		}
		return nil
	}
	rig.prov.LoginWithPasswordFn = func(ctx context.Context, name, password string) (string, error) {
		logins.Add(1)
		return "carol-token", nil
	}

	if _, _, err := rig.reconcile("carol"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	calls := rig.prov.Calls.EnsureRoomPowerLevel
	if len(calls) != 2 {
		t.Fatalf("power calls=%d, want 2 (actor + self), got %+v", len(calls), calls)
	}
	if calls[0].SelfToken != "" {
		t.Errorf("first attempt must not carry a self token: %+v", calls[0])
	}
	if calls[1].SelfToken != "carol-token" {
		t.Errorf("retry must use the human's own token, got %q", calls[1].SelfToken)
	}
	if logins.Load() != 1 {
		t.Errorf("logins=%d, want exactly 1 (lazy, after the 403)", logins.Load())
	}
}

// Steady-state: when the grant never 403s, no login is issued.
func TestHumanReconciler_PowerGrantNoLoginOnSuccess(t *testing.T) {
	worker := newReadyWorker("w1", "!room-w1:localhost")
	human := newHuman("carol", v1beta1.HumanSpec{
		PermissionLevel:   2,
		AccessibleWorkers: []string{"w1"},
	})
	human.Status.MatrixUserID = "@carol:localhost"
	human.Status.InitialPassword = "stored-pw"
	human.Status.Rooms = []string{"!room-w1:localhost"}
	human.Status.Phase = "Active"
	human.Finalizers = []string{finalizerName}

	var logins atomic.Int32
	rig := newHumanRig(t, human, worker)
	rig.prov.LoginWithPasswordFn = func(ctx context.Context, name, password string) (string, error) {
		logins.Add(1)
		return "carol-token", nil
	}

	if _, _, err := rig.reconcile("carol"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if logins.Load() != 0 {
		t.Errorf("logins=%d, want 0 (no 403, no login)", logins.Load())
	}
}

// Revocation: when the admin kick is rejected (equal-power, or the admin
// is not a member), the human leaves with their own token and the room is
// dropped from status.
func TestHumanReconciler_RevocationSelfLeaveFallback(t *testing.T) {
	human := newHuman("dave", v1beta1.HumanSpec{})
	human.Status.MatrixUserID = "@dave:localhost"
	human.Status.InitialPassword = "stored-pw"
	human.Status.Rooms = []string{"!room-gone:localhost"} // no longer desired
	human.Status.Phase = "Active"
	human.Finalizers = []string{finalizerName}

	var leaves []string
	rig := newHumanRig(t, human)
	rig.prov.KickFromRoomFn = func(ctx context.Context, roomID, userID, reason string) error {
		return &matrix.APIError{StatusCode: 403, ErrCode: "M_FORBIDDEN", Message: "cannot kick: target power level not below kicker"}
	}
	rig.prov.LoginWithPasswordFn = func(ctx context.Context, name, password string) (string, error) {
		return "dave-token", nil
	}
	rig.prov.LeaveRoomAsFn = func(ctx context.Context, roomID, userToken string) error {
		leaves = append(leaves, roomID+"/"+userToken)
		return nil
	}

	out, _, err := rig.reconcile("dave")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rig.prov.Calls.KickFromRoom) != 1 {
		t.Fatalf("kick calls=%+v, want 1", rig.prov.Calls.KickFromRoom)
	}
	if len(leaves) != 1 || leaves[0] != "!room-gone:localhost/dave-token" {
		t.Errorf("self-leave=%v, want [!room-gone:localhost/dave-token]", leaves)
	}
	if len(out.Status.Rooms) != 0 {
		t.Errorf("Status.Rooms=%v, want empty (revoked)", out.Status.Rooms)
	}
}

// Revocation: with no usable human token, the admin-bot force-leave is
// the last resort.
func TestHumanReconciler_RevocationForceLeaveLastResort(t *testing.T) {
	human := newHuman("erin", v1beta1.HumanSpec{})
	human.Status.MatrixUserID = "@erin:localhost"
	human.Status.InitialPassword = "stale-pw"
	human.Status.Rooms = []string{"!room-gone:localhost"}
	human.Status.Phase = "Active"
	human.Finalizers = []string{finalizerName}

	var forced []string
	rig := newHumanRig(t, human)
	rig.prov.KickFromRoomFn = func(ctx context.Context, roomID, userID, reason string) error {
		return &matrix.APIError{StatusCode: 403, ErrCode: "M_FORBIDDEN", Message: "cannot kick: target power level not below kicker"}
	}
	rig.prov.LoginWithPasswordFn = func(ctx context.Context, name, password string) (string, error) {
		return "", errors.New("password stale: M_FORBIDDEN from homeserver login")
	}
	rig.prov.ForceLeaveRoomFn = func(ctx context.Context, userID, roomID string) error {
		forced = append(forced, roomID+"/"+userID)
		return nil
	}

	out, _, err := rig.reconcile("erin")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(forced) != 1 || forced[0] != "!room-gone:localhost/@erin:localhost" {
		t.Errorf("force-leave=%v, want [!room-gone:localhost/@erin:localhost]", forced)
	}
	if len(out.Status.Rooms) != 0 {
		t.Errorf("Status.Rooms=%v, want empty (revoked)", out.Status.Rooms)
	}
}

// Regression (CI test-19 join-403 deadlock, 2nd wave): the team names the
// human as admin and its room is already in the human's Status.Rooms
// (written by syncTeamRoomHumanStatuses), but the team's
// Status.TeamRoomID is not yet visible in the cache (informer lag right
// after team provisioning). The room's origin is unresolved while the
// human holds a team membership claim -> the revocation path must DEFER
// the kick for one cycle. Kicking here evicts the team admin from their
// own team room; with the admin excluded from the invite list
// (creator-join design), every later team reconcile then fails on join
// (M_FORBIDDEN: cannot join a room that is not public) forever.
func TestHumanReconciler_RevocationDeferredWhileTeamRoomUnresolved(t *testing.T) {
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-alpha", Namespace: "default"},
		Spec:       v1beta1.TeamSpec{Admin: &v1beta1.TeamAdminSpec{Name: "dave"}},
		// Status.TeamRoomID intentionally empty: room not yet visible.
	}
	human := newHuman("dave", v1beta1.HumanSpec{})
	human.Status.MatrixUserID = "@dave:localhost"
	human.Status.InitialPassword = "stored-pw"
	human.Status.Rooms = []string{"!team-new:localhost"}
	human.Status.Phase = "Active"
	human.Finalizers = []string{finalizerName}

	rig := newHumanRig(t, human, team)
	out, _, err := rig.reconcile("dave")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rig.prov.Calls.KickFromRoom) != 0 {
		t.Fatalf("kick calls=%+v, want 0 (deferred while claim unresolved)", rig.prov.Calls.KickFromRoom)
	}
	if len(out.Status.Rooms) != 1 || out.Status.Rooms[0] != "!team-new:localhost" {
		t.Errorf("Status.Rooms=%v, want [!team-new:localhost] (kept for next cycle)", out.Status.Rooms)
	}
}

// Control: the deferral must not mask a genuine revocation. The human
// holds an unresolved claim on team-new (room not visible) but the room
// in Status.Rooms belongs to team-old, whose Status.TeamRoomID IS
// visible and which no longer names the human -> known origin, not
// desired -> the kick proceeds immediately.
func TestHumanReconciler_RevocationProceedsForKnownOriginTeamRoom(t *testing.T) {
	teamOld := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-old", Namespace: "default"},
		Spec:       v1beta1.TeamSpec{}, // admin removed: human no longer belongs
		Status:     v1beta1.TeamStatus{TeamRoomID: "!old-room:localhost"},
	}
	teamNew := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-new", Namespace: "default"},
		Spec:       v1beta1.TeamSpec{Admin: &v1beta1.TeamAdminSpec{Name: "dave"}},
		// Status.TeamRoomID intentionally empty: room not yet visible.
	}
	human := newHuman("dave", v1beta1.HumanSpec{})
	human.Status.MatrixUserID = "@dave:localhost"
	human.Status.InitialPassword = "stored-pw"
	human.Status.Rooms = []string{"!old-room:localhost"}
	human.Status.Phase = "Active"
	human.Finalizers = []string{finalizerName}

	rig := newHumanRig(t, human, teamOld, teamNew)
	out, _, err := rig.reconcile("dave")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rig.prov.Calls.KickFromRoom) != 1 {
		t.Fatalf("kick calls=%+v, want 1 (known-origin revocation stays prompt)", rig.prov.Calls.KickFromRoom)
	}
	if len(out.Status.Rooms) != 0 {
		t.Errorf("Status.Rooms=%v, want empty (revoked)", out.Status.Rooms)
	}
}
