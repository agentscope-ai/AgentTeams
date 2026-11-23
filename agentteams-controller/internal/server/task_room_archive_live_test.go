package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Run only against a disposable, empty homeserver with registration enabled and
// first-user administration. Never point this fixture at an existing workspace.
func TestTaskRoomArchiveLiveControllerRelay(t *testing.T) {
	base := os.Getenv("TASK_ROOM_ARCHIVE_SMOKE_URL")
	if base == "" {
		t.Skip("set TASK_ROOM_ARCHIVE_SMOKE_URL for a disposable feature homeserver")
	}
	ctx := context.Background()
	httpClient := &http.Client{Timeout: 30 * time.Second}
	cfg := matrix.Config{ServerURL: base, Domain: "archive-smoke.invalid", RegistrationToken: os.Getenv("TASK_ROOM_ARCHIVE_SMOKE_REGISTRATION_TOKEN")}
	mx := matrix.NewTuwunelClient(cfg, httpClient)
	register := func(name string) *matrix.UserCredentials {
		t.Helper()
		user, err := mx.EnsureUser(ctx, matrix.EnsureUserRequest{Username: name, Password: "Archive-smoke-fixture-only-20261116!"})
		if err != nil {
			t.Fatal(err)
		}
		return user
	}
	admin := register("archive_service_admin")
	alice := register("archive_alice")
	bob := register("archive_bob")
	cfg.AdminUser, cfg.AdminPassword = admin.UserID, admin.Password
	mx = matrix.NewTuwunelClient(cfg, httpClient)
	// Initial password assignment must preserve the registration/AS token.
	// Tuwunel 2.0 logs out devices by default when reset-password is processed.
	const initialPassword = "Archive-initial-password-fixture-20261116!"
	if err := mx.SetPasswordAsAdmin(ctx, bob.UserID, initialPassword); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := mx.Login(ctx, "archive_bob", initialPassword); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("initial password did not propagate: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := mx.VerifyAccessToken(ctx, bob.AccessToken); err != nil {
		t.Fatalf("initial password assignment revoked the existing token: %v", err)
	}
	request := func(server, method, path, token string, body any, want int) map[string]any {
		t.Helper()
		if body == nil {
			body = map[string]any{}
		}
		data, _ := json.Marshal(body)
		req, err := http.NewRequest(method, server+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var result map[string]any
		_ = json.NewDecoder(res.Body).Decode(&result)
		if res.StatusCode != want {
			t.Fatalf("%s %s: got %d want %d: %v", method, path, res.StatusCode, want, result)
		}
		return result
	}
	room, err := mx.CreateRoom(ctx, matrix.CreateRoomRequest{Name: "Archive relay fixture", CreatorToken: alice.AccessToken, Invite: []string{bob.UserID}, PowerLevels: map[string]int{alice.UserID: 100}, InitialState: []matrix.StateEvent{{Type: "room.meta", Content: map[string]any{"roomKind": "task_room", "teamName": "smoke-team", "teamAdmin": map[string]any{"userId": alice.UserID}, "humanMembers": []any{map[string]any{"userId": bob.UserID}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	rp := "/_matrix/client/v3/rooms/" + url.PathEscape(room.RoomID)
	request(base, "POST", rp+"/join", bob.AccessToken, map[string]any{}, 200)
	created := request(base, "GET", rp+"/state/m.room.create/", alice.AccessToken, nil, 200)
	if created["room_version"] != "10" {
		t.Fatalf("legacy client default must remain version 10: %v", created)
	}
	request(base, "PUT", rp+"/send/m.room.message/result", alice.AccessToken, map[string]any{"msgtype": "m.text", "body": "Accepted workspace artifact"}, 200)
	scheme := runtime.NewScheme()
	_ = v1beta1.AddToScheme(scheme)
	team := &v1beta1.Team{ObjectMeta: metav1.ObjectMeta{Name: "smoke-team-cr", Namespace: "default"}, Spec: v1beta1.TeamSpec{TeamName: "smoke-team", Admin: &v1beta1.TeamAdminSpec{Name: "alice"}}}
	human := func(name, id string) *v1beta1.Human {
		return &v1beta1.Human{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: v1beta1.HumanSpec{PermissionLevel: 2, AccessibleTeams: []string{team.Name}}, Status: v1beta1.HumanStatus{MatrixUserID: id}}
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team, human("alice", alice.UserID), human("bob", bob.UserID)).Build()
	h := NewTaskRoomArchiveHandler(ServerDeps{Client: k8s, APIReader: k8s, Namespace: "default", MatrixConfig: cfg, MatrixClient: mx})
	mw := authpkg.NewMiddleware(authpkg.NewMatrixTokenAuthenticator(k8s, "default", mx), nil, nil, k8s, "default")
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/task-rooms/archived", mw.Authenticate(http.HandlerFunc(h.List)))
	mux.Handle("GET /api/v1/task-rooms/{roomId}/archive", mw.Authenticate(http.HandlerFunc(h.Status)))
	for _, action := range []string{"archive", "unarchive", "delete"} {
		mux.Handle("POST /api/v1/task-rooms/{roomId}/"+action, mw.Authenticate(h.Mutate(action)))
	}
	controller := httptest.NewServer(mux)
	defer controller.Close()
	cp := "/api/v1/task-rooms/" + url.PathEscape(room.RoomID)
	request(base, "POST", taskRoomPath(room.RoomID, "archive"), alice.AccessToken, nil, 403)
	request(controller.URL, "POST", cp+"/archive", bob.AccessToken, nil, 403)
	request(controller.URL, "POST", cp+"/archive", alice.AccessToken, nil, 200)
	listed := request(controller.URL, "GET", "/api/v1/task-rooms/archived?team_admin="+url.QueryEscape(alice.UserID)+"&human_member="+url.QueryEscape(bob.UserID), alice.AccessToken, nil, 200)
	if rooms, _ := listed["rooms"].([]any); len(rooms) != 1 {
		t.Fatalf("filtered archive list: %v", listed)
	}
	positions := map[string]string{}
	for _, user := range []*matrix.UserCredentials{alice, bob} {
		sync := request(base, "GET", "/_matrix/client/v3/sync?timeout=0", user.AccessToken, nil, 200)
		positions[user.UserID], _ = sync["next_batch"].(string)
		data, _ := json.Marshal(sync["rooms"])
		if strings.Contains(string(data), room.RoomID) {
			t.Fatal("archived room leaked into a member's sync")
		}
	}
	history := request(base, "GET", rp+"/messages?dir=b&limit=100", bob.AccessToken, nil, 200)
	if !strings.Contains(fmt.Sprint(history), "Accepted workspace artifact") {
		t.Fatal("archived history is unavailable")
	}
	joined := request(base, "GET", "/_matrix/client/v3/joined_rooms", admin.AccessToken, nil, 200)
	if strings.Contains(fmt.Sprint(joined), room.RoomID) {
		t.Fatal("Controller service administrator was joined to the task room")
	}
	// A change of Team ownership applies immediately, even with whoami cached.
	team.Spec.Admin.Name = "bob"
	if err := k8s.Update(ctx, team); err != nil {
		t.Fatal(err)
	}
	request(controller.URL, "POST", cp+"/unarchive", alice.AccessToken, nil, 403)
	request(controller.URL, "POST", cp+"/unarchive", bob.AccessToken, nil, 200)
	for _, user := range []*matrix.UserCredentials{alice, bob} {
		sync := request(base, "GET", "/_matrix/client/v3/sync?timeout=0&since="+url.QueryEscape(positions[user.UserID]), user.AccessToken, nil, 200)
		data, _ := json.Marshal(sync["rooms"])
		if !strings.Contains(string(data), room.RoomID) {
			t.Fatal("restored room did not reappear in a member's incremental sync")
		}
	}
	request(controller.URL, "POST", cp+"/delete", bob.AccessToken, nil, 403)
	request(controller.URL, "POST", cp+"/archive", bob.AccessToken, nil, 200)
	deleted := request(controller.URL, "POST", cp+"/delete", bob.AccessToken, nil, 200)
	if deleted["deleted"] != true || len(deleted["failed_to_kick_users"].([]any)) != 0 {
		t.Fatalf("room purge failed: %v", deleted)
	}
}
