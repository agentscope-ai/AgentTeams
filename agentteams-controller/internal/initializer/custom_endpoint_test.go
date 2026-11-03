package initializer

import (
	"context"
	"encoding/json"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCustomProviderEndpoint(t *testing.T) {
	for _, tc := range []struct {
		url, host       string
		port            int
		custom, service string
	}{
		{"http://192.0.2.10:3000/v1", "192.0.2.10", 3000, "http://192.0.2.10/v1", "local.static"},
		{"https://192.0.2.10:8443/v1", "192.0.2.10", 8443, "https://192.0.2.10/v1", "local.static"},
		{"https://api.example.com:8443/v1?x=y", "api.example.com", 8443, "https://api.example.com/v1?x=y", "local.dns"},
		{"https://api.example.com/v1", "api.example.com", 443, "https://api.example.com/v1", "local.dns"},
	} {
		t.Run(tc.url, func(t *testing.T) {
			h, p, err := parseCustomEndpoint(tc.url)
			if err != nil || h != tc.host || p != tc.port {
				t.Fatalf("parse: %s %d %v", h, p, err)
			}
			if got := customProviderURL(tc.url); got != tc.custom {
				t.Errorf("URL %s", got)
			}
			if got := customServiceName("local", h); got != tc.service {
				t.Errorf("service %s", got)
			}
		})
	}
}

func TestInvalidCustomEndpoint(t *testing.T) {
	for _, value := range []string{"http://[2001:db8::1]:3000/v1", "http:///v1", "ftp://api.example.com/v1", "https://user:password@api.example.com/v1", "http://api.example.com:0/v1"} {
		if _, _, err := parseCustomEndpoint(value); err == nil {
			t.Errorf("accepted %s", value)
		}
	}
}

// Exercise both initializer branches through the real Higress HTTP client.
func TestCustomEndpointInitializer(t *testing.T) {
	for _, provider := range []string{"openai-compat", "local-model"} {
		t.Run(provider, func(t *testing.T) {
			seenSource, seenProvider := false, false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/session/login":
					http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
				case "/system/higress-config":
					if r.Method == http.MethodGet {
						w.Write([]byte(`{"success":true,"data":""}`))
					}
				case "/v1/service-sources":
					var body map[string]interface{}
					json.NewDecoder(r.Body).Decode(&body)
					seenSource = body["type"] == "static" && body["protocol"] == "https" && body["domain"] == "192.0.2.10:8443"
				case "/v1/ai/providers":
					var body map[string]interface{}
					json.NewDecoder(r.Body).Decode(&body)
					raw := body["rawConfigs"].(map[string]interface{})
					seenProvider = body["name"] == provider && raw["openaiCustomServiceName"] == provider+".static" && raw["openaiCustomUrl"] == "https://192.0.2.10/v1" && raw["openaiCustomServicePort"] == float64(8443)
				}
			}))
			defer server.Close()
			i := Initializer{Config: Config{LLMProvider: provider, LLMAPIKey: "synthetic", OpenAIBaseURL: "https://192.0.2.10:8443/v1"}, Gateway: gateway.NewHigressClient(gateway.Config{ConsoleURL: server.URL}, server.Client())}
			if err := i.initGatewayRoutes(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !seenSource || !seenProvider {
				t.Fatalf("source=%v provider=%v", seenSource, seenProvider)
			}
		})
	}
}
