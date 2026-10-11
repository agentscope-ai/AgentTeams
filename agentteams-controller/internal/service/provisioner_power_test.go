package service

import (
	"context"
	"errors"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
)

func TestEnsureRoomPowerLevel_LegacyRoomGrantsLevel(t *testing.T) {
	fake := newFakeTeamMatrix() // no powerStates: legacy room, state never set
	p := NewProvisioner(ProvisionerConfig{
		Matrix:   fake,
		Creds:    fakeCredentialStore{},
		OSSAdmin: &fakeStorageAdmin{},
	})

	if err := p.EnsureRoomPowerLevel(context.Background(), "!r:hs", "@alice:hs", 100, "", ""); err != nil {
		t.Fatalf("EnsureRoomPowerLevel: %v", err)
	}
	calls := fake.roomStates
	if len(calls) != 1 || calls[0].eventType != "m.room.power_levels" || calls[0].roomID != "!r:hs" {
		t.Fatalf("room state calls=%+v, want single m.room.power_levels write", calls)
	}
	users, ok := calls[0].content["users"].(map[string]interface{})
	if !ok {
		t.Fatalf("users not map[string]interface{}: %#v", calls[0].content["users"])
	}
	if users["@alice:hs"] != 100.0 {
		t.Errorf("alice level=%v, want 100", users["@alice:hs"])
	}
}

func TestEnsureRoomPowerLevel_MergesExistingUsers(t *testing.T) {
	existing := map[string]interface{}{
		"users": map[string]interface{}{
			"@manager:hs": 100.0,
			"@alice:hs":   0.0, // human currently at 0 — the bug
			"@worker:hs":  0.0,
		},
		"users_default": 0.0,
		"state_default": 50.0,
		// Extension fields the write path must survive untouched.
		"events":        map[string]interface{}{"m.room.name": 50.0},
		"invite":        50.0,
		"notifications": map[string]interface{}{"room": 50.0},
	}
	fake := newFakeTeamMatrix()
	fake.powerStates = map[string]map[string]interface{}{"!r:hs": existing}
	p := NewProvisioner(ProvisionerConfig{
		Matrix:   fake,
		Creds:    fakeCredentialStore{},
		OSSAdmin: &fakeStorageAdmin{},
	})

	if err := p.EnsureRoomPowerLevel(context.Background(), "!r:hs", "@alice:hs", 50, "", ""); err != nil {
		t.Fatalf("EnsureRoomPowerLevel: %v", err)
	}
	users, ok := fake.powerStates["!r:hs"]["users"].(map[string]interface{})
	if !ok {
		t.Fatalf("stored users not a map: %#v", fake.powerStates["!r:hs"])
	}
	if users["@alice:hs"] != 50.0 {
		t.Errorf("alice=%v, want 50", users["@alice:hs"])
	}
	if users["@manager:hs"] != 100.0 || users["@worker:hs"] != 0.0 {
		t.Errorf("existing users disturbed: %v", users)
	}
	// Non-user power-level settings preserved.
	if _, ok := fake.powerStates["!r:hs"]["users_default"]; !ok {
		t.Errorf("users_default dropped")
	}
	if _, ok := fake.powerStates["!r:hs"]["state_default"]; !ok {
		t.Errorf("state_default dropped")
	}
	// Extension fields survive the write — only the target users entry
	// may be mutated, never a rebuilt struct.
	if ev, ok := fake.powerStates["!r:hs"]["events"].(map[string]interface{}); !ok || ev["m.room.name"] != 50.0 {
		t.Errorf("events field dropped or altered: %#v", fake.powerStates["!r:hs"]["events"])
	}
	if inv, ok := fake.powerStates["!r:hs"]["invite"].(float64); !ok || inv != 50.0 {
		t.Errorf("invite field dropped or altered: %#v", fake.powerStates["!r:hs"]["invite"])
	}
	if n, ok := fake.powerStates["!r:hs"]["notifications"].(map[string]interface{}); !ok || n["room"] != 50.0 {
		t.Errorf("notifications field dropped or altered: %#v", fake.powerStates["!r:hs"]["notifications"])
	}
}

// A demoted human must actually be lowered: a user sitting at 100 whose
// permissionLevel drops from 1 to 2 is written at 50, not kept at 100.
// The admin (creator, 100) CANNOT make this write directly — spec v8
// rule 9.6 rejects changing another user whose current level (100) is not
// strictly below the sender's (100). The provisioner must therefore fall
// back to the human's OWN token, whose own entry is exempt from 9.6. The
// authorization-aware fake enforces exactly that, so this test proves the
// fallback path rather than trusting a permissive double.
func TestEnsureRoomPowerLevel_DemotionRevokesLevel(t *testing.T) {
	existing := map[string]interface{}{
		"users": map[string]interface{}{
			"@manager:hs": 100.0,
			"@alice:hs":   100.0, // was L1, now being demoted to L2
			"@worker:hs":  0.0,
		},
		"ban":  50.0,
		"kick": 50.0,
	}
	fake := newFakeTeamMatrix()
	fake.powerStates = map[string]map[string]interface{}{"!r:hs": existing}
	fake.members["!r:hs"] = []matrix.RoomMember{{UserID: "@alice:hs", Membership: "join"}}
	fake.tokenUsers = map[string]string{"alice-token": "@alice:hs"}
	p := NewProvisioner(ProvisionerConfig{
		Matrix:   fake,
		Creds:    fakeCredentialStore{},
		OSSAdmin: &fakeStorageAdmin{},
	})

	if err := p.EnsureRoomPowerLevel(context.Background(), "!r:hs", "@alice:hs", 50, "", "alice-token"); err != nil {
		t.Fatalf("EnsureRoomPowerLevel: %v", err)
	}
	if len(fake.roomStates) != 2 {
		t.Fatalf("expected actor attempt + self-write, got %d attempts: %+v", len(fake.roomStates), fake.roomStates)
	}
	if fake.roomStates[0].token != "" {
		t.Errorf("first attempt should run as the default admin actor, got token %q", fake.roomStates[0].token)
	}
	if fake.roomStates[1].token != "alice-token" {
		t.Errorf("self-write must use the human's own token, got %q", fake.roomStates[1].token)
	}
	users, ok := fake.powerStates["!r:hs"]["users"].(map[string]interface{})
	if !ok {
		t.Fatalf("stored users not a map: %#v", fake.powerStates["!r:hs"])
	}
	if users["@alice:hs"] != 50.0 {
		t.Errorf("alice=%v, want 50 (demoted from 100)", users["@alice:hs"])
	}
	if users["@manager:hs"] != 100.0 || users["@worker:hs"] != 0.0 {
		t.Errorf("other users disturbed: %v", users)
	}
}

// Without the human's own token there is NO authorized demotion path for
// an equal-level user: the enforced 9.6 rejects the admin's write and the
// call must surface that error (no silent success, no state change) so
// the reconcile retries next cycle.
func TestEnsureRoomPowerLevel_EqualLevelDemotionWithoutSelfTokenFails(t *testing.T) {
	existing := map[string]interface{}{
		"users": map[string]interface{}{
			"@manager:hs": 100.0,
			"@alice:hs":   100.0,
		},
	}
	fake := newFakeTeamMatrix()
	fake.powerStates = map[string]map[string]interface{}{"!r:hs": existing}
	p := NewProvisioner(ProvisionerConfig{
		Matrix:   fake,
		Creds:    fakeCredentialStore{},
		OSSAdmin: &fakeStorageAdmin{},
	})

	err := p.EnsureRoomPowerLevel(context.Background(), "!r:hs", "@alice:hs", 50, "", "")
	if err == nil {
		t.Fatal("equal-level demotion without a self token must fail (spec 9.6)")
	}
	if !matrix.IsForbidden(err) {
		t.Fatalf("expected M_FORBIDDEN from the homeserver rule, got: %v", err)
	}
	users := fake.powerStates["!r:hs"]["users"].(map[string]interface{})
	if users["@alice:hs"] != 100.0 {
		t.Errorf("state must be unchanged after a rejected demotion, alice=%v", users["@alice:hs"])
	}
}

// A simple grant (0 -> 50) is an ordinary actor write: the target's
// current level (0) is below the actor's (100), so no self-write is
// needed and exactly one attempt is made.
func TestEnsureRoomPowerLevel_SimpleGrantIsSingleActorWrite(t *testing.T) {
	existing := map[string]interface{}{
		"users": map[string]interface{}{"@alice:hs": 0.0},
	}
	fake := newFakeTeamMatrix()
	fake.powerStates = map[string]map[string]interface{}{"!r:hs": existing}
	p := NewProvisioner(ProvisionerConfig{
		Matrix:   fake,
		Creds:    fakeCredentialStore{},
		OSSAdmin: &fakeStorageAdmin{},
	})

	if err := p.EnsureRoomPowerLevel(context.Background(), "!r:hs", "@alice:hs", 50, "", ""); err != nil {
		t.Fatalf("EnsureRoomPowerLevel: %v", err)
	}
	if len(fake.roomStates) != 1 {
		t.Fatalf("expected exactly one write attempt, got %d", len(fake.roomStates))
	}
	if fake.roomStates[0].token != "" {
		t.Errorf("simple grant should run as the default admin actor, got token %q", fake.roomStates[0].token)
	}
	users := fake.powerStates["!r:hs"]["users"].(map[string]interface{})
	if users["@alice:hs"] != 50.0 {
		t.Errorf("alice=%v, want 50", users["@alice:hs"])
	}
}

// TeamAdmin-owned room (P1-2): the homeserver admin is deliberately NOT a
// member, so the default admin identity cannot even READ the room state —
// let alone write it. The grant must run with a token of an authorized
// member (here the team admin, the room's creator).
func TestEnsureRoomPowerLevel_TeamAdminOwnedRoom(t *testing.T) {
	existing := map[string]interface{}{
		"users": map[string]interface{}{
			"@teamadmin:hs": 100.0,
			"@manager:hs":   100.0,
			"@alice:hs":     0.0,
		},
	}
	room := "!team-owned:hs"
	fake := newFakeTeamMatrix()
	fake.powerStates = map[string]map[string]interface{}{room: existing}
	fake.roomCreators = map[string]string{room: "@teamadmin:hs"}
	fake.adminIsMember = map[string]bool{} // admin is a member of NO room
	fake.tokenUsers = map[string]string{"teamadmin-token": "@teamadmin:hs"}
	p := NewProvisioner(ProvisionerConfig{
		Matrix:   fake,
		Creds:    fakeCredentialStore{},
		OSSAdmin: &fakeStorageAdmin{},
	})

	// The default admin identity is rejected on the READ (non-member).
	if _, err := fake.GetRoomState(context.Background(), room, "m.room.power_levels", "", ""); !matrix.IsForbidden(err) {
		t.Fatalf("admin read of a TeamAdmin-owned room must be M_FORBIDDEN, got %v", err)
	}

	// The team-admin actor can read AND write the grant.
	if err := p.EnsureRoomPowerLevel(context.Background(), room, "@alice:hs", 50, "teamadmin-token", ""); err != nil {
		t.Fatalf("EnsureRoomPowerLevel as team admin: %v", err)
	}
	users := fake.powerStates[room]["users"].(map[string]interface{})
	if users["@alice:hs"] != 50.0 {
		t.Errorf("alice=%v, want 50", users["@alice:hs"])
	}
	if users["@teamadmin:hs"] != 100.0 || users["@manager:hs"] != 100.0 {
		t.Errorf("other users disturbed: %v", users)
	}
}

// A team-admin actor in a room where the homeserver admin is not a member
// must still be able to demote an equal-level human — the actor (100)
// hits the same 9.6 wall as the admin would, so the self fallback runs as
// the human.
func TestEnsureRoomPowerLevel_TeamAdminRoomEqualLevelDemotion(t *testing.T) {
	existing := map[string]interface{}{
		"users": map[string]interface{}{
			"@teamadmin:hs": 100.0,
			"@alice:hs":     100.0, // former L1 human, now demoted
		},
	}
	room := "!team-owned:hs"
	fake := newFakeTeamMatrix()
	fake.powerStates = map[string]map[string]interface{}{room: existing}
	fake.roomCreators = map[string]string{room: "@teamadmin:hs"}
	fake.adminIsMember = map[string]bool{}
	fake.members[room] = []matrix.RoomMember{{UserID: "@alice:hs", Membership: "join"}}
	fake.tokenUsers = map[string]string{
		"teamadmin-token": "@teamadmin:hs",
		"alice-token":     "@alice:hs",
	}
	p := NewProvisioner(ProvisionerConfig{
		Matrix:   fake,
		Creds:    fakeCredentialStore{},
		OSSAdmin: &fakeStorageAdmin{},
	})

	if err := p.EnsureRoomPowerLevel(context.Background(), room, "@alice:hs", 50, "teamadmin-token", "alice-token"); err != nil {
		t.Fatalf("EnsureRoomPowerLevel (team-admin actor + self fallback): %v", err)
	}
	if len(fake.roomStates) != 2 {
		t.Fatalf("expected actor attempt + self-write, got %d attempts", len(fake.roomStates))
	}
	users := fake.powerStates[room]["users"].(map[string]interface{})
	if users["@alice:hs"] != 50.0 {
		t.Errorf("alice=%v, want 50", users["@alice:hs"])
	}
}

func TestEnsureRoomPowerLevel_ExactMatchNoWrite(t *testing.T) {
	existing := map[string]interface{}{
		"users": map[string]interface{}{"@alice:hs": 50.0, "@manager:hs": 100.0},
	}
	fake := newFakeTeamMatrix()
	fake.powerStates = map[string]map[string]interface{}{"!r:hs": existing}
	p := NewProvisioner(ProvisionerConfig{
		Matrix:   fake,
		Creds:    fakeCredentialStore{},
		OSSAdmin: &fakeStorageAdmin{},
	})

	if err := p.EnsureRoomPowerLevel(context.Background(), "!r:hs", "@alice:hs", 50, "", ""); err != nil {
		t.Fatalf("EnsureRoomPowerLevel: %v", err)
	}
	if len(fake.roomStates) != 0 {
		t.Errorf("expected no write when already at exactly the desired level, got %d writes", len(fake.roomStates))
	}
}

func TestEnsureRoomPowerLevel_ReadErrorPropagates(t *testing.T) {
	fake := newFakeTeamMatrix()
	fake.powerStateErr = errors.New("matrix down")
	p := NewProvisioner(ProvisionerConfig{
		Matrix:   fake,
		Creds:    fakeCredentialStore{},
		OSSAdmin: &fakeStorageAdmin{},
	})

	err := p.EnsureRoomPowerLevel(context.Background(), "!r:hs", "@alice:hs", 50, "", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if len(fake.roomStates) != 0 {
		t.Errorf("no write allowed after read failure, got %d", len(fake.roomStates))
	}
}

// A second human granted later must not clobber the first human's level —
// the write path must merge against what the previous write stored (via the
// homeserver's JSON round-trip in production, mirrored in the fake).
func TestEnsureRoomPowerLevel_SecondGrantPreservesFirst(t *testing.T) {
	fake := newFakeTeamMatrix()
	p := NewProvisioner(ProvisionerConfig{
		Matrix:   fake,
		Creds:    fakeCredentialStore{},
		OSSAdmin: &fakeStorageAdmin{},
	})

	if err := p.EnsureRoomPowerLevel(context.Background(), "!r:hs", "@alice:hs", 100, "", ""); err != nil {
		t.Fatalf("first grant: %v", err)
	}
	if err := p.EnsureRoomPowerLevel(context.Background(), "!r:hs", "@bob:hs", 50, "", ""); err != nil {
		t.Fatalf("second grant: %v", err)
	}
	stored := fake.powerStates["!r:hs"]
	users, ok := stored["users"].(map[string]interface{})
	if !ok {
		t.Fatalf("users=%#v, want map after second grant", stored["users"])
	}
	if users["@alice:hs"] != 100.0 || users["@bob:hs"] != 50.0 {
		t.Errorf("users=%v, want alice=100 bob=50 (no clobber)", users)
	}
}

// EnsureRoomPowerLevel must also handle a power_levels state that exists
// without a users map (defensive: treat as empty users).
func TestEnsureRoomPowerLevel_StateWithoutUsersMap(t *testing.T) {
	fake := newFakeTeamMatrix()
	fake.powerStates = map[string]map[string]interface{}{
		"!r:hs": {"users_default": 0.0},
	}
	p := NewProvisioner(ProvisionerConfig{
		Matrix:   fake,
		Creds:    fakeCredentialStore{},
		OSSAdmin: &fakeStorageAdmin{},
	})

	if err := p.EnsureRoomPowerLevel(context.Background(), "!r:hs", "@alice:hs", 50, "", ""); err != nil {
		t.Fatalf("EnsureRoomPowerLevel: %v", err)
	}
	stored := fake.powerStates["!r:hs"]
	users, ok := stored["users"].(map[string]interface{})
	if !ok || users["@alice:hs"] != 50.0 {
		t.Errorf("users=%#v, want alice=50", stored["users"])
	}
}
