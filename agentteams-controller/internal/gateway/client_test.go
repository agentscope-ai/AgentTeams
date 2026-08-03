package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newGatewayTestClient(handler http.HandlerFunc) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			return rec.Result(), nil
		}),
	}
}

func TestEnsureConsumer_Created(t *testing.T) {
	client := newGatewayTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/system/init":
			w.WriteHeader(http.StatusOK)
		case "/session/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
			w.WriteHeader(http.StatusOK)
		case "/v1/consumers":
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	c := NewHigressClient(Config{
		ConsoleURL:    "http://higress.test",
		AdminUser:     "admin",
		AdminPassword: "admin",
	}, client)

	result, err := c.EnsureConsumer(context.Background(), ConsumerRequest{
		Name:          "worker-alice",
		CredentialKey: "key-abc-123",
	})
	if err != nil {
		t.Fatalf("EnsureConsumer: %v", err)
	}
	if result.Status != "created" {
		t.Errorf("Status = %q, want created", result.Status)
	}
	if result.APIKey != "key-abc-123" {
		t.Errorf("APIKey = %q, want key-abc-123", result.APIKey)
	}
}

func TestEnsureConsumer_Exists(t *testing.T) {
	client := newGatewayTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/system/init":
			w.WriteHeader(http.StatusOK)
		case "/session/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
			w.WriteHeader(http.StatusOK)
		case "/v1/consumers":
			w.WriteHeader(http.StatusConflict)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)
	result, err := c.EnsureConsumer(context.Background(), ConsumerRequest{
		Name:          "worker-bob",
		CredentialKey: "key-xyz",
	})
	if err != nil {
		t.Fatalf("EnsureConsumer: %v", err)
	}
	if result.Status != "exists" {
		t.Errorf("Status = %q, want exists", result.Status)
	}
}

func TestAuthorizeAIRoutes(t *testing.T) {
	client := newGatewayTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system/init":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/session/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v1/ai/routes" && r.Method == "GET":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]interface{}{
					{"name": "route-1"},
				},
			})
		case r.URL.Path == "/v1/ai/routes/route-1" && r.Method == "GET":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"name": "route-1",
					"authConfig": map[string]interface{}{
						"allowedConsumers": []string{"manager"},
					},
				},
			})
		case r.URL.Path == "/v1/ai/routes/route-1" && r.Method == "PUT":
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			authConfig, _ := body["authConfig"].(map[string]interface{})
			consumers := toStringSlice(authConfig["allowedConsumers"])
			if !containsString(consumers, "worker-alice") {
				t.Errorf("expected worker-alice in allowedConsumers, got %v", consumers)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Logf("unexpected: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		}
	}))

	c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)
	if err := c.AuthorizeAIRoutes(context.Background(), "worker-alice", ""); err != nil {
		t.Fatalf("AuthorizeAIRoutes: %v", err)
	}
}

func TestAuthorizeAIRoutesSerializesRouteUpdates(t *testing.T) {
	var mu sync.Mutex
	activePUTs := 0
	maxActivePUTs := 0

	client := newGatewayTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system/init":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/session/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v1/ai/routes" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]interface{}{
					{"name": "route-1"},
				},
			})
		case r.URL.Path == "/v1/ai/routes/route-1" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"name": "route-1",
					"authConfig": map[string]interface{}{
						"allowedConsumers": []string{"manager"},
					},
				},
			})
		case r.URL.Path == "/v1/ai/routes/route-1" && r.Method == "PUT":
			mu.Lock()
			activePUTs++
			if activePUTs > maxActivePUTs {
				maxActivePUTs = activePUTs
			}
			mu.Unlock()

			time.Sleep(20 * time.Millisecond)

			mu.Lock()
			activePUTs--
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			t.Logf("unexpected: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		}
	}))

	c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for _, consumer := range []string{"worker-a", "worker-b", "worker-c", "worker-d"} {
		wg.Add(1)
		go func(consumer string) {
			defer wg.Done()
			errs <- c.AuthorizeAIRoutes(context.Background(), consumer, "")
		}(consumer)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("AuthorizeAIRoutes: %v", err)
		}
	}

	if maxActivePUTs != 1 {
		t.Fatalf("AI route PUTs ran concurrently: maxActivePUTs=%d, want 1", maxActivePUTs)
	}
}

func TestExposePort(t *testing.T) {
	var calledDomain, calledSvcSrc, calledRoute bool
	client := newGatewayTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system/init":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/session/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v1/domains":
			calledDomain = true
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v1/service-sources":
			calledSvcSrc = true
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v1/routes":
			calledRoute = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))

	c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)
	err := c.ExposePort(context.Background(), PortExposeRequest{
		WorkerName: "alice",
		Port:       3000,
	})
	if err != nil {
		t.Fatalf("ExposePort: %v", err)
	}
	if !calledDomain || !calledSvcSrc || !calledRoute {
		t.Errorf("expected all three Higress APIs called: domain=%v svcSrc=%v route=%v",
			calledDomain, calledSvcSrc, calledRoute)
	}
}

func TestSessionReauth(t *testing.T) {
	loginCount := 0
	client := newGatewayTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/system/init":
			w.WriteHeader(http.StatusOK)
		case "/session/login":
			loginCount++
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
			w.WriteHeader(http.StatusOK)
		case "/v1/consumers":
			if loginCount == 1 {
				w.WriteHeader(http.StatusUnauthorized)
			} else {
				w.WriteHeader(http.StatusOK)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)

	// First call triggers 401 which clears cookies
	c.EnsureConsumer(context.Background(), ConsumerRequest{Name: "test", CredentialKey: "k"})
	// Second call should re-authenticate
	c.EnsureConsumer(context.Background(), ConsumerRequest{Name: "test", CredentialKey: "k"})

	if loginCount < 2 {
		t.Errorf("expected at least 2 logins after 401, got %d", loginCount)
	}
}

func TestEnsureConsumer_EmbeddedFallbackConvergesPassword(t *testing.T) {
	currentPassword := "admin"
	changePasswordCalled := false
	loginPasswords := []string{}

	client := newGatewayTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/system/init":
			w.WriteHeader(http.StatusOK)
		case "/session/login":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode login body: %v", err)
			}
			loginPasswords = append(loginPasswords, body["password"])
			if body["username"] == "admin" && body["password"] == currentPassword {
				http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		case "/user/changePassword":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode changePassword body: %v", err)
			}
			if body["oldPassword"] != "admin" || body["newPassword"] != "target-secret" {
				t.Fatalf("unexpected changePassword payload: %+v", body)
			}
			changePasswordCalled = true
			currentPassword = body["newPassword"]
			w.WriteHeader(http.StatusOK)
		case "/v1/consumers":
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	c := NewHigressClient(Config{
		ConsoleURL:                "http://higress.test",
		AdminUser:                 "admin",
		AdminPassword:             "target-secret",
		AllowDefaultAdminFallback: true,
	}, client)

	result, err := c.EnsureConsumer(context.Background(), ConsumerRequest{
		Name:          "worker-alice",
		CredentialKey: "key-abc-123",
	})
	if err != nil {
		t.Fatalf("EnsureConsumer: %v", err)
	}
	if result.Status != "created" {
		t.Errorf("Status = %q, want created", result.Status)
	}
	if !changePasswordCalled {
		t.Fatal("expected changePassword to be called after embedded fallback login")
	}
	if currentPassword != "target-secret" {
		t.Fatalf("currentPassword = %q, want target-secret", currentPassword)
	}
	if len(loginPasswords) != 3 {
		t.Fatalf("expected 3 login attempts, got %d (%v)", len(loginPasswords), loginPasswords)
	}
	if loginPasswords[0] != "target-secret" || loginPasswords[1] != "admin" || loginPasswords[2] != "target-secret" {
		t.Fatalf("unexpected login password sequence: %v", loginPasswords)
	}
}

// TestAuthorizeAIRoutes_PUTFailsPropagatesError asserts that a non-2xx, non-409
// PUT response is surfaced as an error instead of being swallowed. Prior to
// the fix, PUT failures were silently broken by `break`-ing the retry loop
// and returning nil, which left the data plane stuck after controller
// restarts.
func TestAuthorizeAIRoutes_PUTFailsPropagatesError(t *testing.T) {
	client := newGatewayTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system/init":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/session/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v1/ai/routes" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]interface{}{
					{"name": "route-1"},
				},
			})
		case r.URL.Path == "/v1/ai/routes/route-1" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"name": "route-1",
					"authConfig": map[string]interface{}{
						"allowedConsumers": []string{},
					},
				},
			})
		case r.URL.Path == "/v1/ai/routes/route-1" && r.Method == "PUT":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Logf("unexpected: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		}
	}))

	c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)
	if err := c.AuthorizeAIRoutes(context.Background(), "worker-alice", ""); err == nil {
		t.Fatalf("AuthorizeAIRoutes: expected error from 500 PUT, got nil")
	}
}

// TestEnsureAIRoute_ExistingSkipsPOST asserts EnsureAIRoute never writes to
// /v1/ai/routes when the route already exists with a matching skeleton. This
// is the cornerstone of the ownership split: the Initializer must not touch
// authConfig.allowedConsumers on restart.
func TestEnsureAIRoute_ExistingSkipsPOST(t *testing.T) {
	var postCalled bool
	client := newGatewayTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system/init":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/session/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v1/ai/routes/default-ai-route" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"name": "default-ai-route",
					"pathPredicate": map[string]interface{}{
						"matchValue": "/v1",
					},
					"upstreams": []interface{}{
						map[string]interface{}{"provider": "openai"},
					},
					"authConfig": map[string]interface{}{
						"enabled":          true,
						"allowedConsumers": []string{"manager", "worker-alice"},
					},
				},
			})
		case r.URL.Path == "/v1/ai/routes" && r.Method == "POST":
			postCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Logf("unexpected: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		}
	}))

	c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)
	err := c.EnsureAIRoute(context.Background(), AIRouteRequest{
		Name:       "default-ai-route",
		PathPrefix: "/v1",
		Provider:   "openai",
	})
	if err != nil {
		t.Fatalf("EnsureAIRoute: %v", err)
	}
	if postCalled {
		t.Fatalf("EnsureAIRoute POSTed to /v1/ai/routes despite route existing; must be a no-op")
	}
}

func TestEnsureStreamIdleTimeoutPatchesHigressConfig(t *testing.T) {
	var putBody map[string]interface{}
	client := newGatewayTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system/init":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/session/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/system/higress-config" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"data":    "data:\n  higress: |-\n    downstream:\n      connectionBufferLimits: 32768\n      idleTimeout: 180\n      routeTimeout: 0\n    upstream:\n      idleTimeout: 10\n",
			})
		case r.URL.Path == "/system/higress-config" && r.Method == http.MethodPut:
			_ = json.NewDecoder(r.Body).Decode(&putBody)
			w.WriteHeader(http.StatusOK)
		default:
			t.Logf("unexpected: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		}
	}))

	c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)
	if err := c.EnsureStreamIdleTimeout(context.Background(), 900); err != nil {
		t.Fatalf("EnsureStreamIdleTimeout: %v", err)
	}
	if putBody == nil {
		t.Fatalf("expected PUT to /system/higress-config")
	}
	config, _ := putBody["config"].(string)
	if !strings.Contains(config, "      idleTimeout: 900") {
		t.Fatalf("patched config does not contain downstream idleTimeout 900:\n%s", config)
	}
	if !strings.Contains(config, "    upstream:\n      idleTimeout: 10") {
		t.Fatalf("patched config should preserve upstream idleTimeout:\n%s", config)
	}
}

// TestEnsureAIRoute_MissingCreatesSkeletonWithoutAllowedConsumers asserts
// that when the route is absent, the POST body enables the key-auth framework
// but omits allowedConsumers entirely, so Higress defaults it to [] and
// ownership of the field stays with the reconcilers.
func TestEnsureAIRoute_MissingCreatesSkeletonWithoutAllowedConsumers(t *testing.T) {
	var postBody map[string]interface{}
	client := newGatewayTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system/init":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/session/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v1/ai/routes/default-ai-route" && r.Method == "GET":
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/v1/ai/routes" && r.Method == "POST":
			_ = json.NewDecoder(r.Body).Decode(&postBody)
			w.WriteHeader(http.StatusOK)
		default:
			t.Logf("unexpected: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		}
	}))

	c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)
	err := c.EnsureAIRoute(context.Background(), AIRouteRequest{
		Name:       "default-ai-route",
		PathPrefix: "/v1",
		Provider:   "openai",
	})
	if err != nil {
		t.Fatalf("EnsureAIRoute: %v", err)
	}
	if postBody == nil {
		t.Fatalf("expected POST to /v1/ai/routes with a body; got none")
	}
	authConfig, ok := postBody["authConfig"].(map[string]interface{})
	if !ok {
		t.Fatalf("authConfig missing in POST body: %+v", postBody)
	}
	if enabled, _ := authConfig["enabled"].(bool); !enabled {
		t.Fatalf("authConfig.enabled = %v, want true", authConfig["enabled"])
	}
	if _, present := authConfig["allowedConsumers"]; present {
		t.Fatalf("authConfig.allowedConsumers must NOT be written by EnsureAIRoute; got %v", authConfig["allowedConsumers"])
	}
}

func TestAuthorizeAIRoutes_ProviderFilter(t *testing.T) {
	putRoutes := map[string]map[string]interface{}{}
	client := newGatewayTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system/init":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/session/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v1/ai/routes" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]interface{}{
					{"name": "qwen-route"},
					{"name": "openai-route"},
				},
			})
		case r.URL.Path == "/v1/ai/routes/qwen-route" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"name":      "qwen-route",
					"upstreams": []interface{}{map[string]interface{}{"provider": "qwen"}},
					"authConfig": map[string]interface{}{
						"allowedConsumers": []string{"manager"},
					},
				},
			})
		case r.URL.Path == "/v1/ai/routes/openai-route" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"name":      "openai-route",
					"upstreams": []interface{}{map[string]interface{}{"provider": "openai"}},
					"authConfig": map[string]interface{}{
						"allowedConsumers": []string{"manager", "worker-alice"},
					},
				},
			})
		case strings.HasPrefix(r.URL.Path, "/v1/ai/routes/") && r.Method == "PUT":
			routeName := strings.TrimPrefix(r.URL.Path, "/v1/ai/routes/")
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode PUT body: %v", err)
			}
			putRoutes[routeName] = body
			w.WriteHeader(http.StatusOK)
		default:
			t.Logf("unexpected: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		}
	}))

	c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)

	// With provider filter "qwen", qwen-route is authorized and stale
	// worker-alice authorization is removed from non-matching openai-route.
	if err := c.AuthorizeAIRoutes(context.Background(), "worker-alice", "qwen"); err != nil {
		t.Fatalf("AuthorizeAIRoutes: %v", err)
	}
	if len(putRoutes) != 2 {
		t.Fatalf("expected PUT on qwen-route and stale openai-route, got %v", putRoutes)
	}
	qwenConsumers := toStringSlice(putRoutes["qwen-route"]["authConfig"].(map[string]interface{})["allowedConsumers"])
	if !containsString(qwenConsumers, "worker-alice") {
		t.Fatalf("qwen-route allowedConsumers=%v, want worker-alice", qwenConsumers)
	}
	openAIConsumers := toStringSlice(putRoutes["openai-route"]["authConfig"].(map[string]interface{})["allowedConsumers"])
	if containsString(openAIConsumers, "worker-alice") {
		t.Fatalf("openai-route allowedConsumers=%v, want worker-alice removed", openAIConsumers)
	}

	// Without provider filter, both routes should be PUT
	putRoutes = map[string]map[string]interface{}{}
	if err := c.AuthorizeAIRoutes(context.Background(), "worker-bob", ""); err != nil {
		t.Fatalf("AuthorizeAIRoutes (no filter): %v", err)
	}
	if len(putRoutes) != 2 {
		t.Errorf("expected PUT on 2 routes, got %v", putRoutes)
	}
}

func TestResolveModelProvider_Higress(t *testing.T) {
	client := newGatewayTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system/init":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/session/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v1/ai/providers/qwen" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{"name": "qwen", "type": "qwen"},
			})
		case r.URL.Path == "/v1/ai/routes" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]interface{}{
					{"name": "qwen-route"},
					{"name": "openai-route"},
				},
			})
		case r.URL.Path == "/v1/ai/routes/qwen-route" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"name": "qwen-route",
					"pathPredicate": map[string]interface{}{
						"matchValue": "/v1/qwen",
					},
					"upstreams": []interface{}{
						map[string]interface{}{"provider": "qwen", "weight": 100},
					},
				},
			})
		case r.URL.Path == "/v1/ai/routes/openai-route" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"name": "openai-route",
					"pathPredicate": map[string]interface{}{
						"matchValue": "/v1",
					},
					"upstreams": []interface{}{
						map[string]interface{}{"provider": "openai", "weight": 100},
					},
				},
			})
		default:
			t.Logf("unexpected: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		}
	}))

	c := NewHigressClient(Config{
		ConsoleURL:   "http://higress.test",
		DataPlaneURL: "http://aigw-local.agentteams.io:8080",
	}, client)

	info, err := c.ResolveModelProvider(context.Background(), "qwen")
	if err != nil {
		t.Fatalf("ResolveModelProvider: %v", err)
	}
	if info.HttpApiID != "qwen" {
		t.Errorf("HttpApiID = %q, want qwen", info.HttpApiID)
	}
	if info.BasePath != "/v1/qwen" {
		t.Errorf("BasePath = %q, want /v1/qwen", info.BasePath)
	}
	if info.IntranetURL != "http://aigw-local.agentteams.io:8080/v1/qwen" {
		t.Errorf("IntranetURL = %q, want http://aigw-local.agentteams.io:8080/v1/qwen", info.IntranetURL)
	}
}

func TestResolveModelProvider_Higress_NotFound(t *testing.T) {
	client := newGatewayTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system/init":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/session/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v1/ai/providers/nonexist" && r.Method == "GET":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Logf("unexpected: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		}
	}))

	c := NewHigressClient(Config{
		ConsoleURL:   "http://higress.test",
		DataPlaneURL: "http://aigw-local.agentteams.io:8080",
	}, client)

	_, err := c.ResolveModelProvider(context.Background(), "nonexist")
	if err == nil {
		t.Fatal("expected error for nonexistent provider, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to contain 'not found'", err.Error())
	}
}
