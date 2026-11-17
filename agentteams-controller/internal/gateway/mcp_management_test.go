package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestMCPManagementKeepsConsoleCredentialsAndMissingResourceSemantics(t *testing.T) {
	client := newGatewayTestClient(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/system/init":
		case "/session/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "private"})
		case "/v1/mcpServer/docs":
			if _, err := r.Cookie("session"); err != nil {
				t.Error("Console session not used")
			}
			w.WriteHeader(502)
			_, _ = w.Write([]byte(`{"success":false,"message":"com.alibaba.higress.sdk.exception.NotFoundException: can't found the bound route by name: mcp-server-docs.internal"}`))
		default:
			t.Error("unexpected path", r.URL.Path)
		}
	})
	c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)
	value, err := c.ManageMCP(context.Background(), "GET", "/v1/mcpServer/docs", nil)
	if err != nil || value != nil {
		t.Fatalf("value=%v err=%v", value, err)
	}
	if _, err = c.ManageMCP(context.Background(), "POST", "/session/login", nil); err == nil {
		t.Fatal("arbitrary Console path accepted")
	}
}

func TestLegacyGatewayRepairDoesNotGrantManagedModelsToEveryWorker(t *testing.T) {
	for _, filter := range []string{"", "provider"} {
		writes := 0
		client := newGatewayTestClient(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/system/init":
			case "/session/login":
				http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
			case "/v1/ai/routes":
				_, _ = w.Write([]byte(`{"data":[{"name":"agentteams-provider"}]}`))
			case "/v1/ai/routes/agentteams-provider":
				if r.Method == "PUT" {
					writes++
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					_ = json.NewEncoder(w).Encode(map[string]any{"data": body})
					return
				}
				_, _ = w.Write([]byte(`{"data":{"name":"agentteams-provider","upstreams":[{"provider":"provider"}],"authConfig":{"enabled":true,"allowedCredentialTypes":["key-auth"],"allowedConsumers":["manager"]}}}`))
			default:
				t.Error(r.URL.Path)
			}
		})
		c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)
		if err := c.AuthorizeAIRoutes(context.Background(), "worker-new", filter); err != nil {
			t.Fatal(err)
		}
		if (filter == "" && writes != 0) || (filter != "" && writes != 1) {
			t.Fatalf("filter=%q writes=%d", filter, writes)
		}
	}
}
