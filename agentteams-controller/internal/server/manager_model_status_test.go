package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type modelStatusGateway struct {
	gateway.Client
	routes []gateway.AIRouteInfo
	err    error
}

func (g modelStatusGateway) ListAIRoutes(context.Context) ([]gateway.AIRouteInfo, error) {
	return g.routes, g.err
}

func TestManagerModelStatusDistinguishesMissingConfigurationAndUnavailableGateway(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		routes      []gateway.AIRouteInfo
		err         error
		code        int
		configured  string
	}{
		{name: "no provider", model: "qwen3.6-plus", code: 200, configured: "false"},
		{name: "no model", routes: []gateway.AIRouteInfo{{Name: "chat"}}, code: 200, configured: "false"},
		{name: "configured", model: "chat", routes: []gateway.AIRouteInfo{{Name: "chat"}}, code: 200, configured: "true"},
		{name: "gateway unavailable", model: "chat", err: errors.New("unreachable"), code: 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := &v1beta1.Manager{}
			manager.Name, manager.Namespace, manager.Spec.Model = "default", "default", tc.model
			h := &managerModelStatus{client: fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).WithObjects(manager).Build(), namespace: "default", gateway: modelStatusGateway{routes: tc.routes, err: tc.err}}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.SetPathValue("name", "default")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.code || (tc.configured != "" && !strings.Contains(rec.Body.String(), `"configured":`+tc.configured)) {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestManagerGatewayProbeUsesSavedIdentityAndModel(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer saved-manager-key" {
			t.Error("probe did not use the manager identity")
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"saved-model"`) || strings.Contains(string(body), "injected") {
			t.Errorf("probe did not use saved model: %s", body)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"private-response"}}]}`))
	}))
	defer upstream.Close()
	manager := &v1beta1.Manager{}
	manager.Name, manager.Namespace, manager.Spec.Model = "default", "default", "saved-model"
	h := &workerGatewayProbe{client: fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).WithObjects(manager).Build(), namespace: "default", gatewayURL: upstream.URL, manager: true, key: func(_ context.Context, name string) (string, error) {
		if name != "default" {
			t.Errorf("wrong credential owner: %s", name)
		}
		return "saved-manager-key", nil
	}}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"kind":"model","model":"injected","url":"https://example.invalid"}`))
	req.SetPathValue("name", "default")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if calls != 1 || !strings.Contains(rec.Body.String(), `"success":true`) || strings.Contains(rec.Body.String(), "private-response") || strings.Contains(rec.Body.String(), "saved-manager-key") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
