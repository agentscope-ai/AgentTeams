package matrix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	appmetrics "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
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

func TestMatrixOperationUsesBoundedLabels(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		want   string
	}{
		{
			name:   "room state",
			method: http.MethodPut,
			path:   "/_matrix/client/v3/rooms/%21abc%3Ad/state/io.hiclaw.meta/",
			want:   "set_room_state",
		},
		{
			name:   "send message",
			method: http.MethodPut,
			path:   "/_matrix/client/v3/rooms/%21abc%3Ad/send/m.room.message/hc-123",
			want:   "send_message",
		},
		{
			name:   "sync query",
			method: http.MethodGet,
			path:   "/_matrix/client/v3/sync?since=s1&timeout=1000",
			want:   "sync_messages",
		},
		{
			name:   "unknown",
			method: http.MethodPatch,
			path:   "/_matrix/client/v3/rooms/%21abc%3Ad/custom",
			want:   "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matrixOperation(tt.method, tt.path); got != tt.want {
				t.Fatalf("matrixOperation() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDoJSONRecordsUpstreamMetrics(t *testing.T) {
	appmetrics.UpstreamRequestDuration.Reset()
	appmetrics.UpstreamRequests.Reset()
	appmetrics.UpstreamRequestErrors.Reset()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errcode":"M_UNKNOWN","error":"boom"}`, http.StatusInternalServerError)
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{ServerURL: server.URL, Domain: "test.domain"}, server.Client())
	statusCode, _, err := c.doJSON(context.Background(), http.MethodPost,
		"/_matrix/client/v3/createRoom", "token", map[string]string{"name": "room"}, nil)
	if err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	if statusCode != http.StatusInternalServerError {
		t.Fatalf("statusCode = %d, want 500", statusCode)
	}

	if got := testutil.ToFloat64(appmetrics.UpstreamRequests.WithLabelValues("matrix", "create_room", "error", "5xx")); got != 1 {
		t.Fatalf("upstream_requests_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(appmetrics.UpstreamRequestErrors.WithLabelValues("matrix", "create_room", "http")); got != 1 {
		t.Fatalf("upstream_request_errors_total = %v, want 1", got)
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

func TestCreateRoom_InitialStateAndE2EE(t *testing.T) {
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/v3/createRoom" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"room_id": "!room123:test.domain"})
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL,
		Domain:    "test.domain",
	}, server.Client())

	_, err := c.CreateRoom(context.Background(), CreateRoomRequest{
		Name: "Team: alpha",
		InitialState: []StateEvent{{
			Type:     "room.meta",
			StateKey: "",
			Content: map[string]interface{}{
				"roomKind": "team_room",
			},
		}},
		E2EE:         true,
		CreatorToken: "creator-token",
	})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	initialState, ok := gotBody["initial_state"].([]interface{})
	if !ok {
		t.Fatalf("initial_state=%T, want []interface{}", gotBody["initial_state"])
	}
	if len(initialState) != 2 {
		t.Fatalf("initial_state length=%d, want 2", len(initialState))
	}
	meta := initialState[0].(map[string]interface{})
	if meta["type"] != "room.meta" {
		t.Fatalf("first state type=%v, want room.meta", meta["type"])
	}
	content := meta["content"].(map[string]interface{})
	if content["roomKind"] != "team_room" {
		t.Fatalf("roomKind=%v, want team_room", content["roomKind"])
	}
	encryption := initialState[1].(map[string]interface{})
	if encryption["type"] != "m.room.encryption" {
		t.Fatalf("second state type=%v, want m.room.encryption", encryption["type"])
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
		if body["room_alias_name"] != "agentteams-worker-alice" {
			t.Errorf("room_alias_name = %v, want agentteams-worker-alice", body["room_alias_name"])
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"room_id": "!new:test.domain"})
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{ServerURL: server.URL, Domain: "test.domain"}, server.Client())
	info, err := c.CreateRoom(context.Background(), CreateRoomRequest{
		Name:          "Worker: alice",
		RoomAliasName: "agentteams-worker-alice",
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
		case "/_matrix/client/v3/directory/room/#agentteams-worker-alice:test.domain":
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
		RoomAliasName: "agentteams-worker-alice",
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

func TestSetRoomName(t *testing.T) {
	var gotBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/state/m.room.name/":
			if r.Method != http.MethodPut {
				t.Errorf("method = %s, want PUT", r.Method)
			}
			if auth := r.Header.Get("Authorization"); auth != "Bearer admin-token" {
				t.Errorf("Authorization = %q, want Bearer admin-token", auth)
			}
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("decode body: %v", err)
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
	if err := c.SetRoomName(context.Background(), "!room:d", "Team: alpha [deleted]", ""); err != nil {
		t.Fatalf("SetRoomName: %v", err)
	}
	if gotBody["name"] != "Team: alpha [deleted]" {
		t.Fatalf("name body=%v", gotBody)
	}
}

func TestSetRoomState(t *testing.T) {
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/rooms/!room:d/state/room.meta/":
			if r.Method != http.MethodPut {
				t.Errorf("method = %s, want PUT", r.Method)
			}
			if auth := r.Header.Get("Authorization"); auth != "Bearer user-token" {
				t.Errorf("Authorization = %q, want Bearer user-token", auth)
			}
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("decode body: %v", err)
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{}"))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{ServerURL: server.URL, Domain: "d"}, server.Client())
	err := c.SetRoomState(context.Background(), "!room:d", "room.meta", "", map[string]interface{}{
		"roomKind": "team_room",
	}, "user-token")
	if err != nil {
		t.Fatalf("SetRoomState: %v", err)
	}
	if gotBody["roomKind"] != "team_room" {
		t.Fatalf("roomKind body=%v", gotBody)
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
