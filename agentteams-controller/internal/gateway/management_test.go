package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestManageProviderProvisionsCustomEndpointAfterInstallation(t *testing.T) {
	for _, tc := range []struct{ host, kind string }{{"api.example.com", "dns"}, {"192.0.2.10", "static"}} {
		t.Run(tc.kind, func(t *testing.T) {
			serviceCreated := false
			client := newGatewayTestClient(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/system/init":
				case "/session/login":
					http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
				case "/v1/service-sources":
					var body map[string]interface{}
					json.NewDecoder(r.Body).Decode(&body)
					if body["type"] != tc.kind || body["port"] != float64(8000) {
						t.Errorf("wrong upstream service source: %#v", body)
					}
					serviceCreated = true
				case "/v1/ai/providers":
					var body map[string]interface{}
					json.NewDecoder(r.Body).Decode(&body)
					raw := body["rawConfigs"].(map[string]interface{})
					if !serviceCreated || raw["openaiCustomServiceName"] != "onboarding-default."+tc.kind || raw["openaiCustomServicePort"] != float64(8000) || raw["openaiCustomUrl"] != "http://"+tc.host+"/v1" {
						t.Errorf("provider not connected to service source: %#v", raw)
					}
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
			})
			c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, client)
			_, _, err := c.ManageAI(context.Background(), http.MethodPost, "providers", "", json.RawMessage(`{"name":"onboarding-default","type":"openai","tokens":["synthetic-key"],"rawConfigs":{"openaiCustomUrl":"http://`+tc.host+`:8000/v1"}}`))
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestManageProviderRejectsMalformedEndpointBeforeWriting(t *testing.T) {
	c := NewHigressClient(Config{ConsoleURL: "http://higress.test"}, newGatewayTestClient(func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected request: %s", r.URL.Path) }))
	for _, endpoint := range []string{"file:///tmp/model", "https://user:password@example.com/v1", "http://[::1]:8000/v1", "http://example.com:70000/v1"} {
		payload, _ := json.Marshal(map[string]interface{}{"name": "onboarding-default", "rawConfigs": map[string]interface{}{"openaiCustomUrl": endpoint}})
		_, _, err := c.ManageAI(context.Background(), http.MethodPost, "providers", "", payload)
		if err == nil || !strings.Contains(err.Error(), "endpoint") && !strings.Contains(err.Error(), "port") {
			t.Errorf("endpoint %s: err=%v", endpoint, err)
		}
	}
}
