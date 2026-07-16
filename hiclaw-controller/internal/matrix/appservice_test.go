package matrix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// helperAdminServer creates a test server that handles admin login, admin room
// resolution, and message sending. It captures the sent admin command body.
// TestRenderAppServiceRegistration_DefaultBroadNamespace documents and pins
// the managed-homeserver security model: with no override the AppService
// claims the exclusive "@.*:<domain>" namespace (safe only for an
// exclusively AgentTeams-managed homeserver).
func TestRenderAppServiceRegistration_DefaultBroadNamespace(t *testing.T) {
	cfg := Config{
		Domain:                    "test.domain",
		AppServiceID:              "agentteams-controller",
		AppServiceToken:           "as",
		AppServiceHSToken:         "hs",
		AppServiceSenderLocalpart: "agentteams-controller",
	}
	reg := RenderAppServiceRegistration(cfg)
	if len(reg.Namespaces.Users) != 1 {
		t.Fatalf("want 1 user namespace, got %d", len(reg.Namespaces.Users))
	}
	u := reg.Namespaces.Users[0]
	if !u.Exclusive || u.Regex != "@.*:test.domain" {
		t.Errorf("user namespace = {Exclusive:%v, Regex:%q}, want {true, @.*:test.domain}", u.Exclusive, u.Regex)
	}
	if len(reg.Namespaces.Aliases) != 2 {
		t.Fatalf("want AgentTeams and legacy HiClaw alias namespaces, got %d", len(reg.Namespaces.Aliases))
	}
	if got, want := reg.Namespaces.Aliases[0].Regex, "#agentteams-.*:test.domain"; got != want {
		t.Errorf("primary alias namespace = %q, want %q", got, want)
	}
	if got, want := reg.Namespaces.Aliases[1].Regex, "#hiclaw-.*:test.domain"; got != want {
		t.Errorf("legacy alias namespace = %q, want %q", got, want)
	}
}

// TestRenderAppServiceRegistration_NarrowedNamespace verifies the operator
// escape hatch: setting AppServiceUserNamespaceRegex narrows impersonation
// scope so the as_token cannot act as non-AgentTeams local users on a shared
// homeserver.
func TestRenderAppServiceRegistration_NarrowedNamespace(t *testing.T) {
	cfg := Config{
		Domain:                       "test.domain",
		AppServiceID:                 "agentteams-controller",
		AppServiceToken:              "as",
		AppServiceHSToken:            "hs",
		AppServiceSenderLocalpart:    "agentteams-controller",
		AppServiceUserNamespaceRegex: "@hiclaw-.*:test.domain",
	}
	reg := RenderAppServiceRegistration(cfg)
	u := reg.Namespaces.Users[0]
	if !u.Exclusive || u.Regex != "@hiclaw-.*:test.domain" {
		t.Errorf("narrowed user namespace = {Exclusive:%v, Regex:%q}, want {true, @hiclaw-.*:test.domain}", u.Exclusive, u.Regex)
	}
}

func helperAdminServer(t *testing.T, onMessage func(body string)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_matrix/client/v3/login":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"access_token": "admin-token"})
		case r.URL.Path == "/_matrix/client/v3/directory/room/#admins:test.domain":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"room_id": "!admins:test.domain"})
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/send/m.room.message/"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if onMessage != nil {
				onMessage(body["body"])
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"event_id":"$evt"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newTestASClient(serverURL string, httpClient *http.Client) *TuwunelClient {
	return NewTuwunelClient(Config{
		ServerURL:                 serverURL,
		Domain:                    "test.domain",
		AdminUser:                 "admin",
		AdminPassword:             "adminpw",
		AppServiceID:              "agentteams-controller",
		AppServiceToken:           "test-as-token",
		AppServiceHSToken:         "test-hs-token",
		AppServiceSenderLocalpart: "agentteams-controller",
	}, httpClient)
}

func TestUnregisterAppService_SendsCorrectCommand(t *testing.T) {
	var gotBody string
	server := helperAdminServer(t, func(body string) { gotBody = body })
	defer server.Close()

	c := newTestASClient(server.URL, server.Client())
	if err := c.UnregisterAppService(context.Background(), "agentteams-controller"); err != nil {
		t.Fatalf("UnregisterAppService: %v", err)
	}
	want := "!admin appservices unregister agentteams-controller"
	if gotBody != want {
		t.Errorf("admin command = %q, want %q", gotBody, want)
	}
}

func TestRegisterAppService_SmokeTestPasses_SkipsRegistration(t *testing.T) {
	adminCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_matrix/client/v3/login":
			// AS login (smoke test) succeeds with the as_token
			auth := r.Header.Get("Authorization")
			if auth == "Bearer test-as-token" {
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]string{"access_token": "user-token"})
				return
			}
			// Admin login
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"access_token": "admin-token"})
		case r.URL.Path == "/_matrix/client/v3/directory/room/#admins:test.domain":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"room_id": "!admins:test.domain"})
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/send/m.room.message/"):
			adminCalled = true
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"event_id":"$evt"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestASClient(server.URL, server.Client())
	reg := RenderAppServiceRegistration(c.config)
	if err := c.RegisterAppService(context.Background(), reg); err != nil {
		t.Fatalf("RegisterAppService: %v", err)
	}
	if adminCalled {
		t.Error("expected admin register command to be skipped when smoke test passes")
	}
}

func TestRegisterAppService_SmokeTestFails_UnregistersThenRegisters(t *testing.T) {
	var commands []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_matrix/client/v3/login":
			// AS login always fails (simulating stale token)
			auth := r.Header.Get("Authorization")
			if auth == "Bearer test-as-token" {
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{"errcode": "M_UNAUTHORIZED"})
				return
			}
			// Admin login succeeds
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"access_token": "admin-token"})
		case r.URL.Path == "/_matrix/client/v3/directory/room/#admins:test.domain":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"room_id": "!admins:test.domain"})
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/send/m.room.message/"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			commands = append(commands, body["body"])
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"event_id":"$evt"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestASClient(server.URL, server.Client())
	// Reduce smoke test retries for faster test
	c.config.AppServiceSenderLocalpart = "agentteams-controller"

	reg := RenderAppServiceRegistration(c.config)
	// Note: this will take ~10s due to 5 smoke test retries * 2s each.
	// The important thing is verifying the unregister→register sequence.
	if err := c.RegisterAppService(context.Background(), reg); err != nil {
		t.Fatalf("RegisterAppService: %v", err)
	}

	if len(commands) < 2 {
		t.Fatalf("expected at least 2 admin commands (unregister + register), got %d", len(commands))
	}
	if !strings.Contains(commands[0], "unregister") {
		t.Errorf("first command should be unregister, got %q", commands[0])
	}
	if !strings.Contains(commands[1], "register") {
		t.Errorf("second command should be register, got %q", commands[1])
	}
}

func TestAppServiceSmokeTest_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_matrix/client/v3/login" {
			auth := r.Header.Get("Authorization")
			if auth == "Bearer test-as-token" {
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]string{"access_token": "user-token"})
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	c := newTestASClient(server.URL, server.Client())
	if err := c.AppServiceSmokeTest(context.Background()); err != nil {
		t.Fatalf("AppServiceSmokeTest: %v", err)
	}
}

func TestAppServiceSmokeTest_AllRetriesFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"errcode": "M_UNAUTHORIZED"})
	}))
	defer server.Close()

	c := newTestASClient(server.URL, server.Client())
	// Speed up retries for testing
	c.orphanRetryBaseDelay = 0

	err := c.AppServiceSmokeTest(context.Background())
	if err == nil {
		t.Fatal("expected error from failed smoke test")
	}
	if !strings.Contains(err.Error(), "smoke test failed after 5 attempts") {
		t.Errorf("unexpected error message: %v", err)
	}
}
