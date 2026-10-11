package matrix

import (
	"context"
	"encoding/json"
	"fmt"
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
			path:   "/_matrix/client/v3/rooms/%21abc%3Ad/state/io.agentteams.meta/",
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

func TestGetRoomState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/state/m.room.power_levels/":
			// Trailing slash: the empty state key is always included in the
			// URL (aligned with SetRoomState, strict-homeserver safe).
			if r.Method != http.MethodGet {
				t.Errorf("method = %s, want GET", r.Method)
			}
			if auth := r.Header.Get("Authorization"); auth != "Bearer admin-token" {
				t.Errorf("Authorization = %q, want Bearer admin-token", auth)
			}
			w.WriteHeader(http.StatusOK)
			// A compliant homeserver returns the state CONTENT object
			// directly — no event envelope (no type/state_key/sender, no
			// "content" wrapper).
			json.NewEncoder(w).Encode(map[string]interface{}{
				"users": map[string]interface{}{"@a:d": 100.0},
			})
		case "/_matrix/client/v3/rooms/!room:d/state/room.meta/":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"errcode":"M_NOT_FOUND"}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{ServerURL: server.URL, Domain: "d"}, server.Client())

	st, err := c.GetRoomState(context.Background(), "!room:d", "m.room.power_levels", "", "")
	if err != nil {
		t.Fatalf("GetRoomState: %v", err)
	}
	users, ok := st["users"].(map[string]interface{})
	if !ok || users["@a:d"] != 100.0 {
		t.Fatalf("users=%#v, want @a:d=100 (content, not event envelope)", st)
	}

	// A room that never had the state set yields (nil, nil), not an error.
	st, err = c.GetRoomState(context.Background(), "!room:d", "room.meta", "", "")
	if err != nil {
		t.Fatalf("missing state must not error: %v", err)
	}
	if st != nil {
		t.Errorf("missing state = %#v, want nil", st)
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

func TestInviteToRoom_IdempotentTuwunelJoinedOrBanned(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/invite":
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_FORBIDDEN",
				"error":   "M_FORBIDDEN: Auth check failed: cannot invite user that is joined or banned",
			})
		case "/_matrix/client/v3/rooms/!room:d/members":
			json.NewEncoder(w).Encode(map[string]any{
				"chunk": []map[string]any{
					{
						"state_key": "@alice:d",
						"content":   map[string]string{"membership": "join"},
					},
				},
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
		t.Errorf("expected nil for joined member, got %v", err)
	}
}

func TestInviteToRoom_TuwunelJoinedOrBannedKeepsBanError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/invite":
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_FORBIDDEN",
				"error":   "M_FORBIDDEN: Auth check failed: cannot invite user that is joined or banned",
			})
		case "/_matrix/client/v3/rooms/!room:d/members":
			json.NewEncoder(w).Encode(map[string]any{
				"chunk": []map[string]any{
					{
						"state_key": "@alice:d",
						"content":   map[string]string{"membership": "ban"},
					},
				},
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
	if err := c.InviteToRoom(context.Background(), "!room:d", "@alice:d"); err == nil {
		t.Fatal("expected banned member error, got nil")
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

// A state read with an explicit user token must authenticate as that
// token, not the homeserver admin — TeamAdmin-owned rooms reject the
// admin (not a member) and require a member's token.
func TestGetRoomState_WithUserToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/rooms/!room:d/state/m.room.power_levels/":
			if auth := r.Header.Get("Authorization"); auth != "Bearer teamadmin-token" {
				t.Errorf("Authorization = %q, want Bearer teamadmin-token", auth)
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"users": map[string]interface{}{"@a:d": 100.0},
			})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{ServerURL: server.URL, Domain: "d"}, server.Client())
	st, err := c.GetRoomState(context.Background(), "!room:d", "m.room.power_levels", "", "teamadmin-token")
	if err != nil {
		t.Fatalf("GetRoomState: %v", err)
	}
	if st["users"] == nil {
		t.Fatalf("unexpected state: %#v", st)
	}
}

// A rejected state write (e.g. the strict-greater power-level rule, or a
// non-member actor) must surface as a decodable M_FORBIDDEN so callers
// can detect it with matrix.IsForbidden and fall back to an authorized
// actor / self-write.
func TestSetRoomState_Forbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/state/m.room.power_levels/":
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_FORBIDDEN",
				"error":   "You don't have permission to modify that state.",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "admin", AdminPassword: "pw",
	}, server.Client())
	err := c.SetRoomState(context.Background(), "!room:d", "m.room.power_levels", "",
		map[string]interface{}{"users": map[string]interface{}{"@a:d": 50.0}}, "")
	if err == nil {
		t.Fatal("expected M_FORBIDDEN, got nil")
	}
	if !IsForbidden(err) {
		t.Fatalf("IsForbidden(%v) = false, want true", err)
	}
}

// A state read by a non-member actor (the homeserver admin in a
// TeamAdmin-owned room) surfaces M_FORBIDDEN rather than a generic error.
func TestGetRoomState_Forbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/state/m.room.power_levels/":
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_FORBIDDEN",
				"error":   "You are not a member of that room.",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "admin", AdminPassword: "pw",
	}, server.Client())
	_, err := c.GetRoomState(context.Background(), "!room:d", "m.room.power_levels", "", "")
	if err == nil {
		t.Fatal("expected M_FORBIDDEN, got nil")
	}
	if !IsForbidden(err) {
		t.Fatalf("IsForbidden(%v) = false, want true", err)
	}
}

// Regression: an equal-power kick rejection ("cannot kick ...", 403) must
// be returned as an error — the old message-sniffing branch treated it as
// idempotent success, which made the caller drop the room from status
// while the user stayed in it.
func TestKickFromRoom_EqualPowerForbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/kick":
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_FORBIDDEN",
				"error":   "Cannot kick @alice:d: their power level is not below yours.",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "admin", AdminPassword: "pw",
	}, server.Client())
	err := c.KickFromRoom(context.Background(), "!room:d", "@alice:d", "access revoked")
	if err == nil {
		t.Fatal("equal-power kick rejection must be an error, got nil")
	}
	if !IsForbidden(err) {
		t.Fatalf("IsForbidden(%v) = false, want true", err)
	}
}

// Leaving a room you are not (or no longer) in is idempotent.
func TestLeaveRoom_IdempotentNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			adminLoginHandler(t, w)
		case "/_matrix/client/v3/rooms/!room:d/leave":
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_NOT_FOUND",
				"error":   "Not a member of that room.",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{
		ServerURL: server.URL, Domain: "d", AdminUser: "admin", AdminPassword: "pw",
	}, server.Client())
	if err := c.LeaveRoom(context.Background(), "!room:d", ""); err != nil {
		t.Errorf("expected nil for not-in-room, got %v", err)
	}
}

// Login caching: a successful login caches the token per user; a second
// login for the same user is served from the cache (no HTTP), while a
// different user still goes to the homeserver.
func TestLogin_TokenCachedPerUser(t *testing.T) {
	var logins atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/v3/login" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n := logins.Add(1)
		json.NewEncoder(w).Encode(map[string]string{"access_token": fmt.Sprintf("tok-%d", n)})
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{ServerURL: server.URL, Domain: "d"}, server.Client())

	tok1, err := c.Login(context.Background(), "alice", "pw")
	if err != nil {
		t.Fatalf("login 1: %v", err)
	}
	if logins.Load() != 1 {
		t.Fatalf("logins=%d, want 1", logins.Load())
	}
	tok2, err := c.Login(context.Background(), "alice", "pw")
	if err != nil {
		t.Fatalf("login 2: %v", err)
	}
	if tok2 != tok1 {
		t.Errorf("cached token = %q, want %q (no second login)", tok2, tok1)
	}
	if logins.Load() != 1 {
		t.Errorf("logins=%d, want 1 (per-user cache)", logins.Load())
	}
	if _, err := c.Login(context.Background(), "bob", "pw"); err != nil {
		t.Fatalf("login bob: %v", err)
	}
	if logins.Load() != 2 {
		t.Errorf("logins=%d, want 2 (different user)", logins.Load())
	}
}

// Login caching: an expired cache entry goes back to the homeserver.
func TestLogin_TokenCacheExpires(t *testing.T) {
	var logins atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/v3/login" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n := logins.Add(1)
		json.NewEncoder(w).Encode(map[string]string{"access_token": fmt.Sprintf("tok-%d", n)})
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{ServerURL: server.URL, Domain: "d"}, server.Client())
	if _, err := c.Login(context.Background(), "alice", "pw"); err != nil {
		t.Fatalf("login 1: %v", err)
	}
	// White-box: expire the cached entry (same package).
	c.userLoginMu.Lock()
	e := c.userLoginCache[c.UserID("alice")]
	e.expiresAt = time.Now().Add(-time.Second)
	c.userLoginCache[c.UserID("alice")] = e
	c.userLoginMu.Unlock()
	if _, err := c.Login(context.Background(), "alice", "pw"); err != nil {
		t.Fatalf("login 2: %v", err)
	}
	if logins.Load() != 2 {
		t.Errorf("logins=%d, want 2 after TTL expiry", logins.Load())
	}
}

// Login caching: the AppService impersonation login caches the same way.
func TestLogin_AppServiceTokenCached(t *testing.T) {
	var logins atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/v3/login" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n := logins.Add(1)
		json.NewEncoder(w).Encode(map[string]string{"access_token": fmt.Sprintf("as-tok-%d", n)})
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{ServerURL: server.URL, Domain: "d", AppServiceToken: "as"}, server.Client())
	tok1, err := c.LoginAppServiceUser(context.Background(), "carol")
	if err != nil {
		t.Fatalf("AS login 1: %v", err)
	}
	tok2, err := c.LoginAppServiceUser(context.Background(), "carol")
	if err != nil {
		t.Fatalf("AS login 2: %v", err)
	}
	if tok2 != tok1 {
		t.Errorf("cached AS token = %q, want %q", tok2, tok1)
	}
	if logins.Load() != 1 {
		t.Errorf("logins=%d, want 1 (AS per-user cache)", logins.Load())
	}
}

// InvalidateUserToken: an explicit invalidation forces a fresh login.
func TestInvalidateUserToken_FreshLogin(t *testing.T) {
	var logins atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/v3/login" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n := logins.Add(1)
		json.NewEncoder(w).Encode(map[string]string{"access_token": fmt.Sprintf("tok-%d", n)})
	}))
	defer server.Close()

	c := NewTuwunelClient(Config{ServerURL: server.URL, Domain: "d"}, server.Client())
	if _, err := c.Login(context.Background(), "dave", "pw"); err != nil {
		t.Fatalf("login 1: %v", err)
	}
	c.InvalidateUserToken(c.UserID("dave"))
	if _, err := c.Login(context.Background(), "dave", "pw"); err != nil {
		t.Fatalf("login 2: %v", err)
	}
	if logins.Load() != 2 {
		t.Errorf("logins=%d, want 2 after invalidation", logins.Load())
	}
}

// Regression: a STALE cached token (account deactivated out-of-band) must
// not short-circuit orphan recovery. EnsureUser must reach the homeserver,
// see the failed login, issue the reset-password command, and return a
// fresh token — not the cached dead one.
func TestEnsureUser_OrphanRecovery_IgnoresStaleCachedToken(t *testing.T) {
	var (
		bobLoginCalls int32
		adminSendHit  int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_matrix/client/v3/register":
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_USER_IN_USE",
				"error":   "User ID already taken",
			})

		case r.URL.Path == "/_matrix/client/v3/login":
			var body struct {
				Identifier struct {
					User string `json:"user"`
				} `json:"identifier"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Identifier.User == "admin" {
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]string{"access_token": "admin-token"})
				return
			}
			// bob: first login fails (stale password), retry succeeds.
			n := atomic.AddInt32(&bobLoginCalls, 1)
			if n <= 1 {
				w.WriteHeader(http.StatusForbidden)
				json.NewEncoder(w).Encode(map[string]string{
					"errcode": "M_FORBIDDEN",
					"error":   "Invalid password",
				})
				return
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"access_token": "fresh-token"})

		case r.URL.Path == "/_matrix/client/v3/directory/room/#admins:test.domain":
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

	// A previously successful login left a cached token; the account was
	// since deactivated, so that token is dead.
	c.storeLoginToken(c.UserID("bob"), "stale-token")

	creds, err := c.EnsureUser(context.Background(), EnsureUserRequest{
		Username: "bob",
		Password: "bobpw",
	})
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if creds.AccessToken == "stale-token" {
		t.Error("EnsureUser returned the cached dead token — orphan recovery was skipped")
	}
	if creds.AccessToken != "fresh-token" {
		t.Errorf("AccessToken = %q, want fresh-token", creds.AccessToken)
	}
	if atomic.LoadInt32(&bobLoginCalls) < 2 {
		t.Errorf("bob login calls=%d, want >=2 (fail then retry via orphan recovery)", bobLoginCalls)
	}
	if atomic.LoadInt32(&adminSendHit) == 0 {
		t.Error("expected the reset-password admin command to be sent")
	}
}
