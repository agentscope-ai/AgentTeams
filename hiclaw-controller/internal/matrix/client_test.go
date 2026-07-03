package matrix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnsureUser_NewRegistration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/register":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{
				"user_id":      "@alice:test.domain",
				"access_token": "token-abc",
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL:         server.URL,
		Domain:            "test.domain",
		RegistrationToken: "reg-secret",
	}, server.Client())

	creds, err := c.EnsureUser(context.Background(), EnsureUserRequest{
		Username: "alice",
		Password: "pass123",
	})
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if !creds.Created {
		t.Error("expected Created=true for new registration")
	}
	if creds.UserID != "@alice:test.domain" {
		t.Errorf("UserID = %q, want @alice:test.domain", creds.UserID)
	}
	if creds.AccessToken != "token-abc" {
		t.Errorf("AccessToken = %q, want token-abc", creds.AccessToken)
	}
	if creds.Password != "pass123" {
		t.Errorf("Password = %q, want pass123", creds.Password)
	}
}

func TestEnsureUser_ExistingUser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/register":
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_USER_IN_USE",
				"error":   "User ID already taken",
			})
		case "/_matrix/client/v3/login":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{
				"access_token": "login-token-xyz",
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL:         server.URL,
		Domain:            "test.domain",
		RegistrationToken: "reg-secret",
	}, server.Client())

	creds, err := c.EnsureUser(context.Background(), EnsureUserRequest{
		Username: "bob",
		Password: "existing-pass",
	})
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if creds.Created {
		t.Error("expected Created=false for existing user")
	}
	if creds.AccessToken != "login-token-xyz" {
		t.Errorf("AccessToken = %q, want login-token-xyz", creds.AccessToken)
	}
}

func TestEnsureUser_GeneratesPassword(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"user_id":      "@gen:test.domain",
			"access_token": "tok",
		})
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL:         server.URL,
		Domain:            "test.domain",
		RegistrationToken: "reg-secret",
	}, server.Client())

	creds, err := c.EnsureUser(context.Background(), EnsureUserRequest{Username: "gen"})
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if len(creds.Password) != 32 { // 16 bytes hex = 32 chars
		t.Errorf("generated password length = %d, want 32", len(creds.Password))
	}
}

func TestCreateRoom(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/v3/createRoom" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer creator-token" {
			t.Errorf("Authorization = %q, want Bearer creator-token", auth)
		}

		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)

		if body["preset"] != "trusted_private_chat" {
			t.Errorf("preset = %v, want trusted_private_chat", body["preset"])
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"room_id": "!room123:test.domain",
		})
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL,
		Domain:    "test.domain",
	}, server.Client())

	info, err := c.CreateRoom(context.Background(), CreateRoomRequest{
		Name:         "Worker: alice",
		Topic:        "Communication channel",
		Invite:       []string{"@admin:test.domain", "@alice:test.domain"},
		CreatorToken: "creator-token",
		PowerLevels: map[string]int{
			"@admin:test.domain": 100,
			"@alice:test.domain": 0,
		},
	})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if !info.Created {
		t.Error("expected Created=true")
	}
	if info.RoomID != "!room123:test.domain" {
		t.Errorf("RoomID = %q, want !room123:test.domain", info.RoomID)
	}
}

func TestCreateRoom_WithAlias(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/v3/createRoom" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["room_alias_name"] != "hiclaw-worker-alice" {
			t.Errorf("room_alias_name = %v, want hiclaw-worker-alice", body["room_alias_name"])
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"room_id": "!new:test.domain"})
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{ServerURL: server.URL, Domain: "test.domain"}, server.Client())
	info, err := c.CreateRoom(context.Background(), CreateRoomRequest{
		Name:          "Worker: alice",
		RoomAliasName: "hiclaw-worker-alice",
		CreatorToken:  "tok",
	})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if !info.Created {
		t.Error("expected Created=true for fresh alias")
	}
	if info.RoomID != "!new:test.domain" {
		t.Errorf("RoomID = %q, want !new:test.domain", info.RoomID)
	}
}

func TestCreateRoom_AliasInUse_ResolvesExisting(t *testing.T) {
	var createCalls, resolveCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/createRoom":
			createCalls++
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_ROOM_IN_USE",
				"error":   "Room alias already exists.",
			})
		case "/_matrix/client/v3/directory/room/#hiclaw-worker-alice:test.domain":
			resolveCalls++
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"room_id": "!existing:test.domain",
				"servers": []string{"test.domain"},
			})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "test.domain",
		AdminUser: "admin", AdminPassword: "pw",
	}, server.Client())

	info, err := c.CreateRoom(context.Background(), CreateRoomRequest{
		Name:          "Worker: alice",
		RoomAliasName: "hiclaw-worker-alice",
	})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if info.Created {
		t.Error("expected Created=false when alias already claimed")
	}
	if info.RoomID != "!existing:test.domain" {
		t.Errorf("RoomID = %q, want !existing:test.domain", info.RoomID)
	}
	if createCalls != 1 {
		t.Errorf("createRoom call count = %d, want 1", createCalls)
	}
	if resolveCalls != 1 {
		t.Errorf("directory GET call count = %d, want 1", resolveCalls)
	}
}

func TestResolveRoomAlias_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/directory/room/#missing:d":
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_NOT_FOUND", "error": "Room alias not found.",
			})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "a", AdminPassword: "p",
	}, server.Client())
	roomID, found, err := c.ResolveRoomAlias(context.Background(), "#missing:d")
	if err != nil {
		t.Fatalf("ResolveRoomAlias: %v", err)
	}
	if found {
		t.Error("expected found=false for missing alias")
	}
	if roomID != "" {
		t.Errorf("roomID = %q, want empty", roomID)
	}
}

func TestDeleteRoomAlias_Idempotent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/directory/room/#gone:d":
			if r.Method != http.MethodDelete {
				t.Errorf("method = %s, want DELETE", r.Method)
			}
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_NOT_FOUND", "error": "Room alias not found.",
			})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "a", AdminPassword: "p",
	}, server.Client())
	if err := c.DeleteRoomAlias(context.Background(), "#gone:d"); err != nil {
		t.Errorf("DeleteRoomAlias should be idempotent on M_NOT_FOUND, got %v", err)
	}
}

func TestDeleteRoomAlias_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/directory/room/#live:d":
			if r.Method != http.MethodDelete {
				t.Errorf("method = %s, want DELETE", r.Method)
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{}"))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "a", AdminPassword: "p",
	}, server.Client())
	if err := c.DeleteRoomAlias(context.Background(), "#live:d"); err != nil {
		t.Fatalf("DeleteRoomAlias: %v", err)
	}
}

// adminLoginHandler returns a handler that responds to admin login with a
// fixed token, allowing tests that exercise admin-driven endpoints.
func adminLoginHandler(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"access_token": "admin-token"})
}

func TestListRoomMembers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/members":
			if auth := r.Header.Get("Authorization"); auth != "Bearer admin-token" {
				t.Errorf("Authorization = %q, want Bearer admin-token", auth)
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"chunk": []map[string]interface{}{
					{"state_key": "@alice:d", "content": map[string]string{"membership": "join"}},
					{"state_key": "@bob:d", "content": map[string]string{"membership": "invite"}},
					{"state_key": "@carol:d", "content": map[string]string{"membership": "leave"}},
					{"state_key": "@dave:d", "content": map[string]string{"membership": "ban"}},
					{"state_key": "", "content": map[string]string{"membership": "join"}},
				},
			})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL:     server.URL,
		Domain:        "d",
		AdminUser:     "admin",
		AdminPassword: "pw",
	}, server.Client())

	members, err := c.ListRoomMembers(context.Background(), "!room:d")
	if err != nil {
		t.Fatalf("ListRoomMembers: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("got %d members, want 2 (filtered join+invite); members=%+v", len(members), members)
	}
	if members[0].UserID != "@alice:d" || members[0].Membership != "join" {
		t.Errorf("members[0] = %+v, want {@alice:d join}", members[0])
	}
	if members[1].UserID != "@bob:d" || members[1].Membership != "invite" {
		t.Errorf("members[1] = %+v, want {@bob:d invite}", members[1])
	}
}

func TestInviteToRoom_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/invite":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["user_id"] != "@alice:d" {
				t.Errorf("user_id = %q, want @alice:d", body["user_id"])
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{}"))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "admin", AdminPassword: "pw",
	}, server.Client())
	if err := c.InviteToRoom(context.Background(), "!room:d", "@alice:d"); err != nil {
		t.Fatalf("InviteToRoom: %v", err)
	}
}

func TestInviteToRoom_Idempotent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/invite":
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_FORBIDDEN",
				"error":   "@alice:d is already in the room.",
			})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "admin", AdminPassword: "pw",
	}, server.Client())
	if err := c.InviteToRoom(context.Background(), "!room:d", "@alice:d"); err != nil {
		t.Errorf("expected nil for already-in-room, got %v", err)
	}
}

func TestInviteToRoom_RealError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/invite":
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_FORBIDDEN",
				"error":   "inviter has insufficient power level",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "admin", AdminPassword: "pw",
	}, server.Client())
	if err := c.InviteToRoom(context.Background(), "!room:d", "@alice:d"); err == nil {
		t.Error("expected error for unrelated 403, got nil")
	}
}

func TestKickFromRoom_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/kick":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["user_id"] != "@alice:d" {
				t.Errorf("user_id = %q", body["user_id"])
			}
			if body["reason"] != "access revoked" {
				t.Errorf("reason = %q", body["reason"])
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{}"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "admin", AdminPassword: "pw",
	}, server.Client())
	if err := c.KickFromRoom(context.Background(), "!room:d", "@alice:d", "access revoked"); err != nil {
		t.Fatalf("KickFromRoom: %v", err)
	}
}

func TestKickFromRoom_Idempotent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/kick":
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_FORBIDDEN",
				"error":   "User @alice:d is not in the room.",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "admin", AdminPassword: "pw",
	}, server.Client())
	if err := c.KickFromRoom(context.Background(), "!room:d", "@alice:d", ""); err != nil {
		t.Errorf("expected nil for not-in-room, got %v", err)
	}
}

func TestUserID(t *testing.T) {
	c := NewTuwunelClient(Config{Domain: "matrix.example.com:8080"}, nil)
	got := c.UserID("alice")
	want := "@alice:matrix.example.com:8080"
	if got != want {
		t.Errorf("UserID = %q, want %q", got, want)
	}
}

func TestEnsureUser_OrphanRecovery(t *testing.T) {
	var (
		registerCalls int32
		loginCalls    int32
		adminSendHit  int32
		adminLoginHit int32
		dirLookupHit  int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_matrix/client/v3/register":
			atomic.AddInt32(&registerCalls, 1)
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_USER_IN_USE",
				"error":   "User ID already taken",
			})

		case r.URL.Path == "/_matrix/client/v3/login":
			n := atomic.AddInt32(&loginCalls, 1)
			var body struct {
				Identifier struct {
					User string `json:"user"`
				} `json:"identifier"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Identifier.User == "admin" {
				atomic.AddInt32(&adminLoginHit, 1)
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]string{"access_token": "admin-token"})
				return
			}
			// First attempt (orphan) fails; retries succeed after the
			// admin reset-password command is "applied".
			if n <= 1 {
				w.WriteHeader(http.StatusForbidden)
				json.NewEncoder(w).Encode(map[string]string{
					"errcode": "M_FORBIDDEN",
					"error":   "Invalid password",
				})
				return
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"access_token": "user-token"})

		case r.URL.Path == "/_matrix/client/v3/directory/room/#admins:test.domain":
			atomic.AddInt32(&dirLookupHit, 1)
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"room_id": "!admins:test.domain"})

		case r.Method == http.MethodPut &&
			len(r.URL.Path) > len("/_matrix/client/v3/rooms/") &&
			r.URL.Path[:len("/_matrix/client/v3/rooms/")] == "/_matrix/client/v3/rooms/":
			atomic.AddInt32(&adminSendHit, 1)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"event_id":"$evt"}`))

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL:         server.URL,
		Domain:            "test.domain",
		RegistrationToken: "reg",
		AdminUser:         "admin",
		AdminPassword:     "adminpw",
	}, server.Client())
	c.orphanRetryBaseDelay = time.Millisecond

	creds, err := c.EnsureUser(context.Background(), EnsureUserRequest{
		Username: "bob",
		Password: "bobpw",
	})
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if creds.Created {
		t.Error("expected Created=false for orphan recovery path")
	}
	if creds.AccessToken != "user-token" {
		t.Errorf("AccessToken = %q, want user-token", creds.AccessToken)
	}
	if atomic.LoadInt32(&adminLoginHit) == 0 {
		t.Error("expected admin login to happen during orphan recovery")
	}
	if atomic.LoadInt32(&dirLookupHit) == 0 {
		t.Error("expected admin room alias to be resolved")
	}
	if atomic.LoadInt32(&adminSendHit) == 0 {
		t.Error("expected admin command to be sent to admin room")
	}
}

func TestAdminCommand(t *testing.T) {
	var (
		sentRoomID string
		sentBody   string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_matrix/client/v3/login":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"access_token": "admin-token"})
		case r.URL.Path == "/_matrix/client/v3/directory/room/#admins:test.domain":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"room_id": "!admins:test.domain"})
		case r.Method == http.MethodPut &&
			len(r.URL.Path) > len("/_matrix/client/v3/rooms/") &&
			r.URL.Path[:len("/_matrix/client/v3/rooms/")] == "/_matrix/client/v3/rooms/":
			sentRoomID = r.URL.Path
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			sentBody = body["body"]
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"event_id":"$evt"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL:     server.URL,
		Domain:        "test.domain",
		AdminUser:     "admin",
		AdminPassword: "adminpw",
	}, server.Client())

	if err := c.AdminCommand(context.Background(), "!admin users force-leave-room @x:test.domain !r:test.domain"); err != nil {
		t.Fatalf("AdminCommand: %v", err)
	}
	if sentRoomID == "" {
		t.Error("expected PUT to rooms/.../send/m.room.message/...")
	}
	if sentBody != "!admin users force-leave-room @x:test.domain !r:test.domain" {
		t.Errorf("sent body = %q", sentBody)
	}
}

func TestSendMessageAsAdmin(t *testing.T) {
	var (
		gotAuthHeader string
		gotPath       string
		gotBody       string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_matrix/client/v3/login":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"access_token": "admin-token"})
		case r.Method == http.MethodPut &&
			len(r.URL.Path) > len("/_matrix/client/v3/rooms/") &&
			r.URL.Path[:len("/_matrix/client/v3/rooms/")] == "/_matrix/client/v3/rooms/":
			gotAuthHeader = r.Header.Get("Authorization")
			gotPath = r.URL.Path
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotBody = body["body"]
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"event_id":"$evt"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL:     server.URL,
		Domain:        "test.domain",
		AdminUser:     "admin",
		AdminPassword: "adminpw",
	}, server.Client())

	if err := c.SendMessageAsAdmin(context.Background(), "!dm:test.domain", "hello world"); err != nil {
		t.Fatalf("SendMessageAsAdmin: %v", err)
	}
	if gotAuthHeader != "Bearer admin-token" {
		t.Errorf("Authorization = %q, want Bearer admin-token", gotAuthHeader)
	}
	if gotBody != "hello world" {
		t.Errorf("body = %q, want hello world", gotBody)
	}
	if gotPath == "" || gotPath[:len("/_matrix/client/v3/rooms/")] != "/_matrix/client/v3/rooms/" {
		t.Errorf("path = %q, want /_matrix/client/v3/rooms/...", gotPath)
	}
}

func TestListJoinedRooms(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/v3/joined_rooms" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer u-tok" {
			t.Errorf("Authorization = %q", auth)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string][]string{
			"joined_rooms": {"!a:d", "!b:d"},
		})
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{ServerURL: server.URL, Domain: "d"}, server.Client())
	rooms, err := c.ListJoinedRooms(context.Background(), "u-tok")
	if err != nil {
		t.Fatalf("ListJoinedRooms: %v", err)
	}
	if len(rooms) != 2 || rooms[0] != "!a:d" || rooms[1] != "!b:d" {
		t.Errorf("rooms = %v", rooms)
	}
}

func TestGeneratePassword(t *testing.T) {
	p1, err := GeneratePassword(16)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1) != 32 {
		t.Errorf("len = %d, want 32", len(p1))
	}

	p2, _ := GeneratePassword(16)
	if p1 == p2 {
		t.Error("two generated passwords should not be equal")
	}
}

// TestListJoinedRooms_403DoesNotClearAdminToken verifies that a 401/403 on a
// per-user-token call (ListJoinedRooms uses userToken, never the cached
// admin token) does not evict the cached admin token. Before the fix,
// doJSON unconditionally cleared c.adminToken on any 401/403 regardless of
// which token was used, forcing an unnecessary re-login on the next admin
// call.
func TestListJoinedRooms_403DoesNotClearAdminToken(t *testing.T) {
	var adminLoginCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			atomic.AddInt32(&adminLoginCalls, 1)
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/joined_rooms":
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_FORBIDDEN",
				"error":   "user token rejected",
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "admin", AdminPassword: "pw",
	}, server.Client())

	// Prime the admin token cache the same way ensureAdminToken would.
	if _, err := c.ensureAdminToken(context.Background()); err != nil {
		t.Fatalf("prime admin token: %v", err)
	}
	if got := atomic.LoadInt32(&adminLoginCalls); got != 1 {
		t.Fatalf("admin logins after priming = %d, want 1", got)
	}

	// A 403 on a per-user-token call must not evict the cached admin token.
	if _, err := c.ListJoinedRooms(context.Background(), "user-tok"); err == nil {
		t.Error("expected error for 403 response, got nil")
	}

	if _, err := c.ensureAdminToken(context.Background()); err != nil {
		t.Fatalf("ensureAdminToken after user-token 403: %v", err)
	}
	if got := atomic.LoadInt32(&adminLoginCalls); got != 1 {
		t.Errorf("admin logins after user-token 403 = %d, want 1 (cached token should not have been cleared)", got)
	}
}

// TestKickFromRoom_IdempotentForbidden_DoesNotClearAdminToken verifies that
// KickFromRoom's expected/idempotent 403 (M_FORBIDDEN, "not in room") does
// not evict the cached admin token, even though KickFromRoom authenticates
// with the admin token. Only a genuine admin-token invalidation (401, or
// 403 M_UNKNOWN_TOKEN) should clear the cache.
func TestKickFromRoom_IdempotentForbidden_DoesNotClearAdminToken(t *testing.T) {
	var adminLoginCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			atomic.AddInt32(&adminLoginCalls, 1)
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/kick":
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_FORBIDDEN",
				"error":   "User @alice:d is not in the room.",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "admin", AdminPassword: "pw",
	}, server.Client())

	if err := c.KickFromRoom(context.Background(), "!room:d", "@alice:d", ""); err != nil {
		t.Fatalf("expected nil for idempotent not-in-room kick, got %v", err)
	}
	if got := atomic.LoadInt32(&adminLoginCalls); got != 1 {
		t.Fatalf("admin logins after first kick = %d, want 1", got)
	}

	// A second admin-authenticated call should reuse the cached token
	// instead of re-logging in, proving the idempotent 403 above did not
	// clear the cache.
	if err := c.KickFromRoom(context.Background(), "!room:d", "@alice:d", ""); err != nil {
		t.Fatalf("expected nil for second idempotent kick, got %v", err)
	}
	if got := atomic.LoadInt32(&adminLoginCalls); got != 1 {
		t.Errorf("admin logins after second kick = %d, want 1 (cached token should not have been cleared)", got)
	}
}

// TestDoJSONAsAdmin_ClearsOnUnknownToken verifies the positive case: a 401
// (or 403 M_UNKNOWN_TOKEN) on an admin-token call must still clear the
// cache so the next call re-authenticates, preserving the original
// behavior for genuine token invalidation.
func TestDoJSONAsAdmin_ClearsOnUnknownToken(t *testing.T) {
	var adminLoginCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			atomic.AddInt32(&adminLoginCalls, 1)
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/directory/room/#missing:d":
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_UNKNOWN_TOKEN",
				"error":   "admin token expired",
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "admin", AdminPassword: "pw",
	}, server.Client())

	if _, _, err := c.ResolveRoomAlias(context.Background(), "#missing:d"); err == nil {
		t.Error("expected error for unauthorized resolve, got nil")
	}
	if got := atomic.LoadInt32(&adminLoginCalls); got != 1 {
		t.Fatalf("admin logins after first call = %d, want 1", got)
	}

	// The cached admin token should have been cleared, forcing a re-login
	// on the next admin-authenticated call.
	if _, err := c.ensureAdminToken(context.Background()); err != nil {
		t.Fatalf("ensureAdminToken after invalidation: %v", err)
	}
	if got := atomic.LoadInt32(&adminLoginCalls); got != 2 {
		t.Errorf("admin logins after invalidation = %d, want 2 (token should have been cleared and re-fetched)", got)
	}
}
