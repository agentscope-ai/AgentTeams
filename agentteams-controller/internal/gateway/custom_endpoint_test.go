package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestCustomEndpointServiceSource(t *testing.T) {
	for _, tc := range []struct {
		host                   string
		port                   int
		protocol, kind, domain string
	}{
		{"192.0.2.10", 3000, "http", "static", "192.0.2.10:3000"},
		{"192.0.2.10", 8443, "https", "static", "192.0.2.10:8443"},

		{"api.example.com", 443, "https", "dns", "api.example.com"},
	} {
		t.Run(tc.domain, func(t *testing.T) {
			client := newGatewayTestClient(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/system/init":
				case "/session/login":
					http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
				case "/v1/service-sources":
					var body map[string]interface{}
					json.NewDecoder(r.Body).Decode(&body)
					if body["type"] != tc.kind || body["domain"] != tc.domain || body["protocol"] != tc.protocol || body["port"] != float64(tc.port) {
						t.Errorf("wrong service: %#v", body)
					}
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			})
			c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)
			var err error
			if tc.kind == "static" {
				err = c.EnsureStaticServiceSource(context.Background(), "openai-compat", tc.host, tc.port, tc.protocol)
			} else {
				err = c.EnsureServiceSource(context.Background(), "openai-compat", tc.host, tc.port, tc.protocol)
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCustomEndpointUpgradeAndRestart(t *testing.T) {
	for _, path := range []string{"/v1/service-sources", "/v1/ai/providers"} {
		t.Run(path, func(t *testing.T) {
			puts := 0
			client := newGatewayTestClient(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/system/init":
				case r.URL.Path == "/session/login":
					http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
				case r.Method == http.MethodPost && r.URL.Path == path:
					w.WriteHeader(http.StatusConflict)
				case r.Method == http.MethodGet && r.URL.Path == path+"/openai-compat":
					json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"version": "7", "customOption": "preserve", "type": "dns", "domain": "192.0.2.10", "rawConfigs": map[string]interface{}{"customOption": "preserve", "openaiCustomServiceName": "openai-compat.dns"}}})
				case r.Method == http.MethodPut && r.URL.Path == path+"/openai-compat":
					puts++
					var body map[string]interface{}
					json.NewDecoder(r.Body).Decode(&body)
					if body["version"] != "7" || body["customOption"] != "preserve" {
						t.Errorf("lost existing fields %#v", body)
					}
					if path == "/v1/service-sources" {
						if body["type"] != "static" || body["protocol"] != "https" {
							t.Errorf("old service retained %#v", body)
						}
					} else {
						if body["rawConfigs"].(map[string]interface{})["customOption"] != "preserve" {
							t.Error("lost existing provider options")
						}
						if body["rawConfigs"].(map[string]interface{})["openaiCustomServiceName"] != "openai-compat.static" {
							t.Errorf("old provider retained %#v", body)
						}
					}
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			})
			c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)
			for run := 0; run < 2; run++ {
				var err error
				if path == "/v1/service-sources" {
					err = c.EnsureStaticServiceSource(context.Background(), "openai-compat", "192.0.2.10", 8443, "https")
				} else {
					err = c.EnsureAIProvider(context.Background(), AIProviderRequest{Name: "openai-compat", Type: "openai", Raw: map[string]interface{}{"openaiCustomServiceName": "openai-compat.static"}})
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if puts != 2 {
				t.Errorf("updates %d, want2", puts)
			}
		})
	}
}

func TestCustomEndpointUpdateFailure(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client := newGatewayTestClient(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/system/init":
				case r.URL.Path == "/session/login":
					http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
				case r.Method == http.MethodPost:
					w.WriteHeader(http.StatusConflict)
				case r.Method == http.MethodGet:
					w.Write([]byte(`{"data":{"version":"7"}}`))
				default:
					w.WriteHeader(status)
				}
			})
			c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)
			if err := c.EnsureStaticServiceSource(context.Background(), "openai-compat", "192.0.2.10", 3000, "http"); err == nil {
				t.Error("failed update reported success")
			}
		})
	}
}
